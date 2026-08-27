package incus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	incustls "github.com/lxc/incus/v7/shared/tls"

	"github.com/aphronio/dorf/internal/deployment"
)

type fakeEnrollmentRemote struct {
	mu                   sync.Mutex
	serverCertificate    string
	clientCertificate    string
	clientPrivateKey     string
	trusted              bool
	authenticateErr      error
	redeemErrors         map[string]error
	trustOnRedeemError   bool
	generated            int
	redeemed             int
	redeemedCertificates []string
}

func (remote *fakeEnrollmentRemote) FetchServerCertificate(context.Context, string) (string, error) {
	return remote.serverCertificate, nil
}

func (remote *fakeEnrollmentRemote) GenerateClientIdentity() (string, string, error) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.generated++
	return remote.clientCertificate, remote.clientPrivateKey, nil
}

func (remote *fakeEnrollmentRemote) AuthenticateAndProve(context.Context, deployment.Incus) (bool, error) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.authenticateErr != nil {
		return false, remote.authenticateErr
	}
	return remote.trusted, nil
}

func (remote *fakeEnrollmentRemote) Redeem(_ context.Context, authority deployment.Incus, _ string, token string) error {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.redeemed++
	remote.redeemedCertificates = append(remote.redeemedCertificates, authority.ClientCertificate)
	if err := remote.redeemErrors[token]; err != nil {
		if remote.trustOnRedeemError {
			remote.trusted = true
		}
		return err
	}
	remote.trusted = true
	return nil
}

type faultEnrollmentCustody struct {
	filesystemEnrollmentCustody
	mu             sync.Mutex
	failRetainOnce bool
	failRemoveOnce bool
}

func (custody *faultEnrollmentCustody) RetainAccepted(path string, authority deployment.Incus) error {
	custody.mu.Lock()
	if custody.failRetainOnce {
		custody.failRetainOnce = false
		custody.mu.Unlock()
		return errors.New("injected crash before accepted retention")
	}
	custody.mu.Unlock()
	return custody.filesystemEnrollmentCustody.RetainAccepted(path, authority)
}

func (custody *faultEnrollmentCustody) RemovePending(path string, expected pendingEnrollment) error {
	custody.mu.Lock()
	if custody.failRemoveOnce {
		custody.failRemoveOnce = false
		custody.mu.Unlock()
		return errors.New("injected crash before pending cleanup")
	}
	custody.mu.Unlock()
	return custody.filesystemEnrollmentCustody.RemovePending(path, expected)
}

func TestEnsureEnrollmentRetainsAuthorityAndRemovesPendingCandidate(t *testing.T) {
	fixture := newEnrollmentFixture(t)
	authority, err := ensureEnrollmentWith(context.Background(), fixture.request(fixture.offer("first")), fixture.dependencies())
	if err != nil {
		t.Fatal(err)
	}
	if authority.Endpoint != "https://100.100.10.20:8443" || fixture.remote.generated != 1 || fixture.remote.redeemed != 1 {
		t.Fatalf("authority=%#v generated=%d redeemed=%d", authority, fixture.remote.generated, fixture.remote.redeemed)
	}
	stored, found, err := deployment.Load(fixture.deploymentPath)
	if err != nil || !found || stored.Incus == nil || *stored.Incus != authority {
		t.Fatalf("stored=%#v found=%t error=%v", stored, found, err)
	}
	if _, err := os.Lstat(fixture.pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending candidate remains: %v", err)
	}
}

func TestEnsureEnrollmentRecoversAcrossEachCommitBoundary(t *testing.T) {
	for _, test := range []struct {
		name    string
		custody *faultEnrollmentCustody
	}{
		{name: "redeemed before accepted retention", custody: &faultEnrollmentCustody{failRetainOnce: true}},
		{name: "accepted before pending cleanup", custody: &faultEnrollmentCustody{failRemoveOnce: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEnrollmentFixture(t)
			dependencies := fixture.dependencies()
			dependencies.custody = test.custody
			request := fixture.request(fixture.offer("first"))
			if _, err := ensureEnrollmentWith(context.Background(), request, dependencies); err == nil || !strings.Contains(err.Error(), "injected crash") {
				t.Fatalf("first attempt error=%v", err)
			}
			authority, err := ensureEnrollmentWith(context.Background(), request, dependencies)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.remote.generated != 1 || fixture.remote.redeemed != 1 {
				t.Fatalf("generated=%d redeemed=%d", fixture.remote.generated, fixture.remote.redeemed)
			}
			stored, _, err := deployment.Load(fixture.deploymentPath)
			if err != nil || stored.Incus == nil || *stored.Incus != authority {
				t.Fatalf("stored=%#v error=%v", stored, err)
			}
		})
	}
}

func TestEnsureEnrollmentUsesFreshOfferWithTheSamePendingClientKey(t *testing.T) {
	fixture := newEnrollmentFixture(t)
	oldOffer := fixture.offer("old")
	fixture.remote.redeemErrors = map[string]error{oldOffer: errors.New("offer consumed")}
	request := fixture.request(oldOffer)
	if _, err := ensureEnrollmentWith(context.Background(), request, fixture.dependencies()); err == nil || !strings.Contains(err.Error(), "fresh offer") {
		t.Fatalf("first attempt error=%v", err)
	}
	freshOffer := fixture.offer("fresh")
	request.TrustToken = freshOffer
	authority, err := ensureEnrollmentWith(context.Background(), request, fixture.dependencies())
	if err != nil {
		t.Fatal(err)
	}
	if fixture.remote.generated != 1 || fixture.remote.redeemed != 3 {
		t.Fatalf("generated=%d redeemed=%d", fixture.remote.generated, fixture.remote.redeemed)
	}
	for _, certificate := range fixture.remote.redeemedCertificates {
		if certificate != authority.ClientCertificate {
			t.Fatal("fresh offer changed the retained client identity")
		}
	}
}

func TestEnsureEnrollmentReconcilesEffectBeforeRedeemError(t *testing.T) {
	fixture := newEnrollmentFixture(t)
	oldOffer := fixture.offer("old")
	candidate, err := createEnrollmentCandidate(context.Background(), fixture.request(oldOffer), fixture.dependencies())
	if err != nil {
		t.Fatal(err)
	}
	if err := (filesystemEnrollmentCustody{}).SavePending(fixture.pendingPath, candidate); err != nil {
		t.Fatal(err)
	}
	fixture.remote.redeemErrors = map[string]error{oldOffer: errors.New("response lost")}
	fixture.remote.trustOnRedeemError = true
	request := fixture.request(fixture.offer("fresh"))
	if _, err := ensureEnrollmentWith(context.Background(), request, fixture.dependencies()); err != nil {
		t.Fatal(err)
	}
	if fixture.remote.redeemed != 1 {
		t.Fatalf("redeemed=%d, fresh offer must remain unused", fixture.remote.redeemed)
	}
}

func TestEnsureEnrollmentSerializesConcurrentAttempts(t *testing.T) {
	fixture := newEnrollmentFixture(t)
	requests := []EnrollmentRequest{
		fixture.request(fixture.offer("first")),
		fixture.request(fixture.offer("second")),
	}
	requests[1].PendingPath = filepath.Join(filepath.Dir(fixture.pendingPath), "other-incus-enrollment.json")
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, request := range requests {
		go func(request EnrollmentRequest) {
			<-start
			_, err := ensureEnrollmentWith(context.Background(), request, fixture.dependencies())
			results <- err
		}(request)
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if fixture.remote.generated != 1 || fixture.remote.redeemed != 1 {
		t.Fatalf("generated=%d redeemed=%d", fixture.remote.generated, fixture.remote.redeemed)
	}
	for _, request := range requests {
		if _, err := os.Lstat(request.PendingPath); !os.IsNotExist(err) {
			t.Fatalf("pending path %s remains: %v", request.PendingPath, err)
		}
	}
}

func TestEnsureEnrollmentFailsClosedOnPendingOrAcceptedConflict(t *testing.T) {
	fixture := newEnrollmentFixture(t)
	custody := filesystemEnrollmentCustody{}
	candidate, err := createEnrollmentCandidate(context.Background(), fixture.request(fixture.offer("first")), fixture.dependencies())
	if err != nil {
		t.Fatal(err)
	}
	if err := custody.SavePending(fixture.pendingPath, candidate); err != nil {
		t.Fatal(err)
	}
	conflict := candidate.authority()
	conflict.Endpoint = "https://100.100.10.21:8443"
	if err := deployment.RetainIncus(fixture.deploymentPath, conflict); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureEnrollmentWith(context.Background(), fixture.request(""), fixture.dependencies()); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflict error=%v", err)
	}
	stored, _, err := custody.LoadPending(fixture.pendingPath)
	if err != nil || stored != candidate {
		t.Fatalf("pending=%#v error=%v", stored, err)
	}
}

func TestEnsureEnrollmentDoesNotRedeemAfterTransportOrCapabilityFailure(t *testing.T) {
	fixture := newEnrollmentFixture(t)
	fixture.remote.authenticateErr = errors.New("capability proof failed")
	if _, err := ensureEnrollmentWith(context.Background(), fixture.request(fixture.offer("first")), fixture.dependencies()); err == nil || !strings.Contains(err.Error(), "capability proof failed") {
		t.Fatalf("error=%v", err)
	}
	if fixture.remote.redeemed != 0 {
		t.Fatalf("redeemed=%d", fixture.remote.redeemed)
	}
}

func TestExactProjectConfinementAcceptsOnlyTheRemoteProject(t *testing.T) {
	for _, test := range []struct {
		name     string
		projects []string
		err      error
		want     bool
	}{
		{name: "exact", projects: []string{RemoteProjectName}, want: true},
		{name: "no project"},
		{name: "default only", projects: []string{api.ProjectDefaultName}},
		{name: "additional project", projects: []string{RemoteProjectName, "other"}},
		{name: "server failure", err: api.StatusErrorf(500, "failed")},
		{name: "transport failure", err: errors.New("connection reset")},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := attestExactEnrollmentProjects(test.projects, test.err)
			if (err == nil) != test.want {
				t.Fatalf("accepted=%t want=%t error=%v", err == nil, test.want, err)
			}
		})
	}
}

type fakeEnrollmentProjectNamesReader struct {
	called   bool
	projects []string
	err      error
}

func (reader *fakeEnrollmentProjectNamesReader) GetProjectNames() ([]string, error) {
	reader.called = true
	return reader.projects, reader.err
}

func TestProjectConfinementProofReadsTheFilteredProjectSet(t *testing.T) {
	reader := &fakeEnrollmentProjectNamesReader{projects: []string{RemoteProjectName}}
	if err := proveExactProjectConfinement(reader); err != nil {
		t.Fatal(err)
	}
	if !reader.called {
		t.Fatal("project names were not read")
	}
}

func TestPreparedRemoteProjectRequiresExactRestrictionConfig(t *testing.T) {
	configured := &api.Project{
		Name: RemoteProjectName,
		ProjectPut: api.ProjectPut{Config: map[string]string{
			"restricted": "true", "features.images": "true", "features.networks": "false",
			"features.profiles": "true", "features.storage.buckets": "false", "features.storage.volumes": "true",
			"limits.instances": "4", "limits.virtual-machines": "4",
			"restricted.networks.access": RemoteNetworkName, "restricted.storage-pools.access": DefaultStoragePool,
		}},
	}
	if err := attestPreparedEnrollmentProject(configured); err != nil {
		t.Fatal(err)
	}
	wrongProject := *configured
	wrongProject.Name = DefaultProject
	if err := attestPreparedEnrollmentProject(&wrongProject); err == nil || !strings.Contains(err.Error(), RemoteProjectName) {
		t.Fatalf("wrong project error=%v", err)
	}
	for key := range configured.Config {
		broken := *configured
		broken.Config = make(map[string]string, len(configured.Config))
		for name, value := range configured.Config {
			broken.Config[name] = value
		}
		broken.Config[key] = "wrong"
		if err := attestPreparedEnrollmentProject(&broken); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("config %s error=%v", key, err)
		}
	}
}

func TestEnrollmentOfferRequiresBoundedExpiryAndOneAdvertisedTailnetEndpoint(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newEnrollmentFixtureAt(t, now)
	fingerprint := certificateFingerprint(t, fixture.remote.serverCertificate)
	for _, test := range []struct {
		name     string
		offer    api.CertificateAddToken
		override string
		detail   string
	}{
		{name: "no expiry", offer: api.CertificateAddToken{ClientName: "dorf", Fingerprint: fingerprint, Addresses: []string{"100.100.10.20:8443"}, Secret: "secret"}, detail: "expire"},
		{name: "too long", offer: api.CertificateAddToken{ClientName: "dorf", Fingerprint: fingerprint, Addresses: []string{"100.100.10.20:8443"}, Secret: "secret", ExpiresAt: now.Add(MaxEnrollmentOfferLifetime + time.Second)}, detail: "expire"},
		{name: "public IP", offer: api.CertificateAddToken{ClientName: "dorf", Fingerprint: fingerprint, Addresses: []string{"203.0.113.10:8443"}, Secret: "secret", ExpiresAt: now.Add(time.Minute)}, detail: "Tailscale"},
		{name: "wrong port", offer: api.CertificateAddToken{ClientName: "dorf", Fingerprint: fingerprint, Addresses: []string{"100.100.10.20:9443"}, Secret: "secret", ExpiresAt: now.Add(time.Minute)}, detail: "Tailscale"},
		{name: "ambiguous", offer: api.CertificateAddToken{ClientName: "dorf", Fingerprint: fingerprint, Addresses: []string{"100.100.10.20:8443", "100.100.10.21:8443"}, Secret: "secret", ExpiresAt: now.Add(time.Minute)}, detail: "exactly one"},
		{name: "unadvertised override", offer: api.CertificateAddToken{ClientName: "dorf", Fingerprint: fingerprint, Addresses: []string{"100.100.10.20:8443"}, Secret: "secret", ExpiresAt: now.Add(time.Minute)}, override: "https://100.100.10.21:8443", detail: "not advertised"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := decodeEnrollmentOffer(test.offer.String(), test.override, now); err == nil || !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("error=%v want=%q", err, test.detail)
			}
		})
	}
}

func TestReadTrustTokenFileRejectsSymlinksPermissionsAndOversize(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(directory, "offer")
	if err := os.WriteFile(valid, []byte("  retained-offer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := ReadTrustTokenFile(valid); err != nil || token != "retained-offer" {
		t.Fatalf("token=%q error=%v", token, err)
	}
	linked := filepath.Join(directory, "linked")
	if err := os.Symlink(valid, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustTokenFile(linked); err == nil {
		t.Fatal("offer symlink was accepted")
	}
	if err := os.Chmod(valid, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustTokenFile(valid); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("permissive file error=%v", err)
	}
	oversize := filepath.Join(directory, "oversize")
	if err := os.WriteFile(oversize, make([]byte, maxEnrollmentOfferBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustTokenFile(oversize); err == nil || !strings.Contains(err.Error(), "bounded") {
		t.Fatalf("oversize error=%v", err)
	}
}

func TestProtectedEnrollmentReadsReattestPermissionsAfterTheRead(t *testing.T) {
	t.Run("offer file", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "offer")
		if err := os.WriteFile(path, []byte("offer"), 0o600); err != nil {
			t.Fatal(err)
		}
		enrollmentReadCompletedForTest = func() {
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { enrollmentReadCompletedForTest = nil })
		if _, err := ReadTrustTokenFile(path); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("permission race error=%v", err)
		}
	})

	t.Run("pending directory", func(t *testing.T) {
		fixture := newEnrollmentFixture(t)
		candidate, err := createEnrollmentCandidate(context.Background(), fixture.request(fixture.offer("first")), fixture.dependencies())
		if err != nil {
			t.Fatal(err)
		}
		custody := filesystemEnrollmentCustody{}
		if err := custody.SavePending(fixture.pendingPath, candidate); err != nil {
			t.Fatal(err)
		}
		directory := filepath.Dir(fixture.pendingPath)
		enrollmentReadCompletedForTest = func() {
			if err := os.Chmod(directory, 0o750); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			enrollmentReadCompletedForTest = nil
			_ = os.Chmod(directory, 0o700)
		})
		if _, _, err := custody.LoadPending(fixture.pendingPath); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("directory permission race error=%v", err)
		}
	})
}

type enrollmentFixture struct {
	t                 *testing.T
	now               time.Time
	deploymentPath    string
	pendingPath       string
	remote            *fakeEnrollmentRemote
	serverFingerprint string
}

func newEnrollmentFixture(t *testing.T) enrollmentFixture {
	return newEnrollmentFixtureAt(t, time.Unix(1_800_000_000, 0))
}

func newEnrollmentFixtureAt(t *testing.T, now time.Time) enrollmentFixture {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	serverCertificate, _, err := incustls.GenerateMemCert(false, false)
	if err != nil {
		t.Fatal(err)
	}
	clientCertificate, clientPrivateKey, err := incustls.GenerateMemCert(true, false)
	if err != nil {
		t.Fatal(err)
	}
	deploymentPath := filepath.Join(directory, "deployment.json")
	if err := deployment.Save(deploymentPath, deployment.Config{Database: testEnrollmentDatabase()}); err != nil {
		t.Fatal(err)
	}
	certificate := string(serverCertificate)
	return enrollmentFixture{
		t: t, now: now, deploymentPath: deploymentPath, pendingPath: filepath.Join(directory, "incus-enrollment.json"),
		remote: &fakeEnrollmentRemote{
			serverCertificate: certificate, clientCertificate: string(clientCertificate), clientPrivateKey: string(clientPrivateKey),
			redeemErrors: map[string]error{},
		},
		serverFingerprint: certificateFingerprint(t, certificate),
	}
}

func (fixture enrollmentFixture) offer(secret string) string {
	return (&api.CertificateAddToken{
		ClientName: "dorf-controller", Fingerprint: fixture.serverFingerprint,
		Addresses: []string{"100.100.10.20:8443"}, Secret: secret, ExpiresAt: fixture.now.Add(15 * time.Minute),
	}).String()
}

func (fixture enrollmentFixture) request(token string) EnrollmentRequest {
	return EnrollmentRequest{DeploymentPath: fixture.deploymentPath, PendingPath: fixture.pendingPath, TrustToken: token}
}

func (fixture enrollmentFixture) dependencies() enrollmentDependencies {
	return enrollmentDependencies{remote: fixture.remote, custody: filesystemEnrollmentCustody{}, now: func() time.Time { return fixture.now }}
}

func certificateFingerprint(t *testing.T, certificate string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(certificate))
	if block == nil {
		t.Fatal("certificate did not decode")
	}
	digest := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(digest[:])
}

func testEnrollmentDatabase() deployment.Database {
	return deployment.Database{Host: "127.0.0.1", Port: 5432, Name: "dorf", User: "dorf", Password: "secret"}
}
