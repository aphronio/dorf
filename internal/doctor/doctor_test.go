package doctor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/incus"
)

type fakeIncusReadClient struct {
	network     string
	networkName string
	networkErr  error
	closed      bool
}

func (c *fakeIncusReadClient) NetworkIPv4(_ context.Context, name string) (string, error) {
	c.networkName = name
	return c.network, c.networkErr
}

func (c *fakeIncusReadClient) Close() { c.closed = true }

func TestIncusChecksUseExplicitSDKConnectionWithoutCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	authority := &deployment.Incus{Endpoint: "unix:///run/incus/dorf.socket"}
	profile := localIncusProfile(t, authority)
	profile.IncusGatewayURL = "http://10.173.0.1:8317/v1"
	client := &fakeIncusReadClient{network: "10.173.0.1/24"}
	var opened, resolved incus.ConnectionConfig
	var reference string
	checks := collectIncusChecks(context.Background(), authority, profile, incusCheckDependencies{
		openClient: func(_ context.Context, config incus.ConnectionConfig) (incusReadClient, error) {
			opened = config
			return client, nil
		},
		resolveImageFingerprint: func(_ context.Context, config incus.ConnectionConfig, image string) (string, error) {
			resolved, reference = config, image
			return image, nil
		},
	})

	if _, found := checks["incus-command"]; found {
		t.Fatal("doctor still reports an ambient Incus CLI dependency")
	}
	if opened.Endpoint != authority.Endpoint || opened.Project != profile.IncusProject || opened.StoragePool != profile.IncusStoragePool {
		t.Fatalf("SDK connection=%#v", opened)
	}
	if resolved != opened || reference != profile.Artifact {
		t.Fatalf("image connection=%#v reference=%q", resolved, reference)
	}
	if client.networkName != profile.IncusNetwork || !client.closed {
		t.Fatalf("network=%q closed=%t", client.networkName, client.closed)
	}
	assertCheckStatus(t, checks, "incus-access", "ready", "")
	assertCheckStatus(t, checks, "incus-network", "ready", "")
	assertCheckStatus(t, checks, "incus-image", "ready", "")
}

func TestIncusChecksDoNotInferOperatorGatewayRoutes(t *testing.T) {
	authority := &deployment.Incus{Endpoint: "unix:///run/incus/dorf.socket"}
	for _, gatewayURL := range []string{"http://10.173.0.99:8317/v1", "https://gateway.example/v1"} {
		profile := localIncusProfile(t, authority)
		profile.IncusGatewayURL = gatewayURL
		checks := collectIncusChecks(context.Background(), authority, profile, incusCheckDependencies{
			openClient: func(context.Context, incus.ConnectionConfig) (incusReadClient, error) {
				return &fakeIncusReadClient{network: "10.173.0.1/24"}, nil
			},
			resolveImageFingerprint: func(_ context.Context, _ incus.ConnectionConfig, reference string) (string, error) {
				return reference, nil
			},
		})

		assertCheckStatus(t, checks, "incus-network", "ready", "")
		if route, found := checks["incus-gateway-route"]; found {
			t.Fatalf("operator gateway %q was inferred from Incus network authority: %#v", gatewayURL, route)
		}
	}
}

func TestIncusChecksFenceMissingAndMismatchedEndpointAuthority(t *testing.T) {
	validAuthority := &deployment.Incus{Endpoint: "unix:///run/incus/dorf.socket"}
	validProfile := localIncusProfile(t, validAuthority)
	for _, test := range []struct {
		name      string
		authority *deployment.Incus
		profile   core.SandboxProfile
		want      string
	}{
		{name: "missing deployment authority", profile: validProfile, want: "not configured"},
		{name: "missing profile authority", authority: validAuthority, profile: func() core.SandboxProfile {
			profile := validProfile
			profile.IncusEndpointAuthorityHash = ""
			return profile
		}(), want: "does not select"},
		{name: "mismatched authority", authority: validAuthority, profile: func() core.SandboxProfile {
			profile := validProfile
			profile.IncusEndpointAuthorityHash = strings.Repeat("b", 64)
			return profile
		}(), want: "does not match"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			checks := collectIncusChecks(context.Background(), test.authority, test.profile, incusCheckDependencies{
				openClient: func(context.Context, incus.ConnectionConfig) (incusReadClient, error) {
					calls++
					return &fakeIncusReadClient{}, nil
				},
				resolveImageFingerprint: func(context.Context, incus.ConnectionConfig, string) (string, error) {
					calls++
					return "", nil
				},
			})
			if calls != 0 {
				t.Fatalf("made %d SDK calls across a failed authority fence", calls)
			}
			assertCheckStatus(t, checks, "incus-access", "failed", test.want)
			assertCheckStatus(t, checks, "incus-network", "failed", test.want)
			assertCheckStatus(t, checks, "incus-image", "failed", test.want)
		})
	}
}

func TestIncusChecksReportMissingNetworkAndImage(t *testing.T) {
	authority := &deployment.Incus{Endpoint: "unix:///run/incus/dorf.socket"}
	profile := localIncusProfile(t, authority)
	profile.IncusGatewayURL = "http://10.173.0.1:8317/v1"
	client := &fakeIncusReadClient{networkErr: incus.ErrNotFound}
	checks := collectIncusChecks(context.Background(), authority, profile, incusCheckDependencies{
		openClient: func(context.Context, incus.ConnectionConfig) (incusReadClient, error) {
			return client, nil
		},
		resolveImageFingerprint: func(_ context.Context, _ incus.ConnectionConfig, reference string) (string, error) {
			return "", errors.New("Incus image " + reference + " was not found")
		},
	})

	assertCheckStatus(t, checks, "incus-access", "ready", "")
	assertCheckStatus(t, checks, "incus-network", "failed", profile.IncusNetwork)
	assertCheckStatus(t, checks, "incus-image", "failed", "was not found")
}

func TestIncusEndpointAccessFailureGatesResourceChecks(t *testing.T) {
	authority := &deployment.Incus{Endpoint: "unix:///run/incus/dorf.socket"}
	profile := localIncusProfile(t, authority)
	profile.IncusGatewayURL = "http://10.173.0.1:8317/v1"
	resolved := false
	checks := collectIncusChecks(context.Background(), authority, profile, incusCheckDependencies{
		openClient: func(context.Context, incus.ConnectionConfig) (incusReadClient, error) {
			return nil, errors.New("dial failed")
		},
		resolveImageFingerprint: func(context.Context, incus.ConnectionConfig, string) (string, error) {
			resolved = true
			return "", nil
		},
	})

	if resolved {
		t.Fatal("image resolver made a second endpoint connection after access failed")
	}
	assertCheckStatus(t, checks, "incus-access", "failed", "dial failed")
	assertCheckStatus(t, checks, "incus-network", "failed", "access is unavailable")
	assertCheckStatus(t, checks, "incus-image", "failed", "access is unavailable")
}

func TestRemoteIncusChecksUsePinnedHTTPSIdentityWithoutLocalHostRequirements(t *testing.T) {
	serverCertificate, _ := doctorTestCertificate(t, "incus.example")
	clientCertificate, clientPrivateKey := doctorTestCertificate(t, "dorf-worker")
	authority := &deployment.Incus{
		Endpoint:          "https://incus.example:8443",
		ServerCertificate: serverCertificate,
		ClientCertificate: clientCertificate,
		ClientPrivateKey:  clientPrivateKey,
	}
	profile := localIncusProfile(t, authority)
	client := &fakeIncusReadClient{network: "10.173.0.1/24"}
	var opened incus.ConnectionConfig
	checks := collectIncusChecks(context.Background(), authority, profile, incusCheckDependencies{
		openClient: func(_ context.Context, config incus.ConnectionConfig) (incusReadClient, error) {
			opened = config
			return client, nil
		},
		resolveImageFingerprint: func(_ context.Context, _ incus.ConnectionConfig, reference string) (string, error) {
			return reference, nil
		},
	})

	for _, name := range []string{"host-platform", "hardware-virtualization", "incus-command"} {
		if _, found := checks[name]; found {
			t.Fatalf("remote Incus reported local-only check %q", name)
		}
	}
	if opened.Endpoint != authority.Endpoint || opened.Project != profile.IncusProject || opened.StoragePool != profile.IncusStoragePool ||
		opened.TLSServerCertificate != serverCertificate || opened.TLSClientCertificate != clientCertificate || opened.TLSClientKey != clientPrivateKey {
		t.Fatalf("remote SDK connection=%#v", opened)
	}
	assertCheckStatus(t, checks, "incus-access", "ready", "")
}

func localIncusProfile(t *testing.T, authority *deployment.Incus) core.SandboxProfile {
	t.Helper()
	hash, err := authority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	return core.SandboxProfile{
		Name:                       "incus-test",
		Provider:                   core.SandboxProviderIncus,
		Artifact:                   strings.Repeat("a", 64),
		IncusEndpointAuthorityHash: hash,
		IncusProject:               "dorf-project",
		IncusStoragePool:           "dorf-pool",
		IncusNetwork:               "dorf-private",
	}
}

func collectIncusChecks(ctx context.Context, authority *deployment.Incus, profile core.SandboxProfile, dependencies incusCheckDependencies) map[string]Check {
	checks := map[string]Check{}
	addIncusChecksWithDependencies(ctx, authority, profile, func(name string, err error, repair string) {
		check := Check{Name: name, Status: "ready", Detail: "ready"}
		if err != nil {
			check.Status = "failed"
			check.Detail = err.Error()
			if repair != "" {
				check.Detail += "; " + repair
			}
		}
		checks[name] = check
	}, dependencies)
	return checks
}

func assertCheckStatus(t *testing.T, checks map[string]Check, name, status, detail string) {
	t.Helper()
	check, found := checks[name]
	if !found || check.Status != status || (detail != "" && !strings.Contains(check.Detail, detail)) {
		t.Fatalf("check %q=%#v found=%t, want status=%q detail containing %q", name, check, found, status, detail)
	}
}

func doctorTestCertificate(t *testing.T, commonName string) (string, string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
}
