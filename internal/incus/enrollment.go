package incus

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	incustls "github.com/lxc/incus/v7/shared/tls"
	"golang.org/x/sys/unix"

	"github.com/aphronio/dorf/internal/deployment"
)

const (
	MaxEnrollmentOfferLifetime = 30 * time.Minute
	enrollmentFetchTimeout     = 10 * time.Second
	maxPendingEnrollmentBytes  = 128 << 10
	pendingEnrollmentVersion   = 1
	maxEnrollmentOfferBytes    = 16 << 10
)

var tailscaleIPv4 = mustCIDR("100.64.0.0/10")
var tailscaleIPv6 = mustCIDR("fd7a:115c:a1e0::/48")
var enrollmentReadCompletedForTest func()

type EnrollmentRequest struct {
	DeploymentPath string
	PendingPath    string
	TrustToken     string
	Endpoint       string
}

type pendingEnrollment struct {
	Version           int    `json:"version"`
	TrustToken        string `json:"trust_token"`
	Endpoint          string `json:"endpoint"`
	ServerCertificate string `json:"server_certificate"`
	ClientCertificate string `json:"client_certificate"`
	ClientPrivateKey  string `json:"client_private_key"`
}

func (candidate pendingEnrollment) authority() deployment.Incus {
	return deployment.Incus{
		Endpoint:          candidate.Endpoint,
		ServerCertificate: candidate.ServerCertificate,
		ClientCertificate: candidate.ClientCertificate,
		ClientPrivateKey:  candidate.ClientPrivateKey,
	}
}

type enrollmentRemote interface {
	FetchServerCertificate(context.Context, string) (string, error)
	GenerateClientIdentity() (string, string, error)
	AuthenticateAndProve(context.Context, deployment.Incus) (bool, error)
	Redeem(context.Context, deployment.Incus, string, string) error
}

type enrollmentCustody interface {
	Lock(string) (func(), error)
	LoadPending(string) (pendingEnrollment, bool, error)
	SavePending(string, pendingEnrollment) error
	RemovePending(string, pendingEnrollment) error
	LoadAccepted(string) (*deployment.Incus, error)
	RetainAccepted(string, deployment.Incus) error
}

type enrollmentDependencies struct {
	remote  enrollmentRemote
	custody enrollmentCustody
	now     func() time.Time
}

// EnsureEnrollment converges one short-lived Incus offer into the Deployment's
// retained authority. PendingPath must be in an operator-only host directory.
func EnsureEnrollment(ctx context.Context, request EnrollmentRequest) (deployment.Incus, error) {
	return ensureEnrollmentWith(ctx, request, enrollmentDependencies{
		remote:  sdkEnrollmentRemote{},
		custody: filesystemEnrollmentCustody{},
		now:     time.Now,
	})
}

// ReadTrustTokenFile reads one protected offer file for a future setup caller.
// Standard input remains caller-owned and does not pass through this function.
func ReadTrustTokenFile(path string) (string, error) {
	contents, err := readProtectedEnrollmentFile(path, maxEnrollmentOfferBytes)
	if err != nil {
		return "", fmt.Errorf("read Incus trust offer: %w", err)
	}
	token := strings.TrimSpace(string(contents))
	if token == "" || strings.ContainsRune(token, '\x00') {
		return "", fmt.Errorf("Incus trust offer file is empty or invalid")
	}
	return token, nil
}

func ensureEnrollmentWith(ctx context.Context, request EnrollmentRequest, dependencies enrollmentDependencies) (deployment.Incus, error) {
	if strings.TrimSpace(request.DeploymentPath) == "" || strings.TrimSpace(request.PendingPath) == "" {
		return deployment.Incus{}, fmt.Errorf("Incus enrollment requires explicit deployment and pending paths")
	}
	unlock, err := dependencies.custody.Lock(request.DeploymentPath)
	if err != nil {
		return deployment.Incus{}, err
	}
	defer unlock()

	accepted, err := dependencies.custody.LoadAccepted(request.DeploymentPath)
	if err != nil {
		return deployment.Incus{}, err
	}
	candidate, pending, err := dependencies.custody.LoadPending(request.PendingPath)
	if err != nil {
		return deployment.Incus{}, err
	}
	if accepted != nil {
		if !pending {
			return *accepted, nil
		}
		if candidate.authority() != *accepted {
			return deployment.Incus{}, fmt.Errorf("pending Incus enrollment conflicts with retained deployment authority")
		}
		if err := dependencies.custody.RemovePending(request.PendingPath, candidate); err != nil {
			return deployment.Incus{}, err
		}
		return *accepted, nil
	}

	if !pending {
		candidate, err = createEnrollmentCandidate(ctx, request, dependencies)
		if err != nil {
			return deployment.Incus{}, err
		}
		if err := dependencies.custody.SavePending(request.PendingPath, candidate); err != nil {
			return deployment.Incus{}, err
		}
	}
	return finishEnrollment(ctx, request, candidate, dependencies)
}

func createEnrollmentCandidate(ctx context.Context, request EnrollmentRequest, dependencies enrollmentDependencies) (pendingEnrollment, error) {
	offer, endpoint, err := decodeEnrollmentOffer(request.TrustToken, request.Endpoint, dependencies.now())
	if err != nil {
		return pendingEnrollment{}, err
	}
	serverCertificate, err := dependencies.remote.FetchServerCertificate(ctx, endpoint)
	if err != nil {
		return pendingEnrollment{}, fmt.Errorf("fetch offered Incus server certificate: %w", err)
	}
	if err := attestServerFingerprint(serverCertificate, offer.Fingerprint); err != nil {
		return pendingEnrollment{}, err
	}
	clientCertificate, clientPrivateKey, err := dependencies.remote.GenerateClientIdentity()
	if err != nil {
		return pendingEnrollment{}, fmt.Errorf("generate Incus client identity: %w", err)
	}
	candidate := pendingEnrollment{
		Version: pendingEnrollmentVersion, TrustToken: strings.TrimSpace(request.TrustToken), Endpoint: endpoint,
		ServerCertificate: serverCertificate, ClientCertificate: clientCertificate, ClientPrivateKey: clientPrivateKey,
	}
	if err := validatePendingEnrollment(candidate); err != nil {
		return pendingEnrollment{}, err
	}
	return candidate, nil
}

func finishEnrollment(ctx context.Context, request EnrollmentRequest, candidate pendingEnrollment, dependencies enrollmentDependencies) (deployment.Incus, error) {
	authority := candidate.authority()
	trusted, err := dependencies.remote.AuthenticateAndProve(ctx, authority)
	if err != nil {
		return deployment.Incus{}, fmt.Errorf("authenticate pending Incus identity: %w", err)
	}
	if !trusted {
		candidate, err = redeemPendingEnrollment(ctx, request, candidate, dependencies)
		if err != nil {
			return deployment.Incus{}, err
		}
		authority = candidate.authority()
		trusted, err = dependencies.remote.AuthenticateAndProve(ctx, authority)
		if err != nil {
			return deployment.Incus{}, fmt.Errorf("prove redeemed Incus identity: %w", err)
		}
		if !trusted {
			return deployment.Incus{}, fmt.Errorf("Incus did not retain the redeemed client identity")
		}
	}
	if err := dependencies.custody.RetainAccepted(request.DeploymentPath, authority); err != nil {
		return deployment.Incus{}, err
	}
	if err := dependencies.custody.RemovePending(request.PendingPath, candidate); err != nil {
		return deployment.Incus{}, err
	}
	return authority, nil
}

func redeemPendingEnrollment(ctx context.Context, request EnrollmentRequest, candidate pendingEnrollment, dependencies enrollmentDependencies) (pendingEnrollment, error) {
	now := dependencies.now()
	offer, _, offerErr := decodeEnrollmentOffer(candidate.TrustToken, candidate.Endpoint, now)
	if offerErr == nil {
		if redeemErr := dependencies.remote.Redeem(ctx, candidate.authority(), offer.ClientName, candidate.TrustToken); redeemErr == nil {
			return candidate, nil
		} else {
			trusted, reconcileErr := dependencies.remote.AuthenticateAndProve(ctx, candidate.authority())
			if reconcileErr != nil {
				return pendingEnrollment{}, fmt.Errorf("reconcile failed Incus offer redemption: %w", reconcileErr)
			}
			if trusted {
				return candidate, nil
			}
			if strings.TrimSpace(request.TrustToken) == "" || strings.TrimSpace(request.TrustToken) == candidate.TrustToken {
				return pendingEnrollment{}, fmt.Errorf("redeem pending Incus offer: %w; issue a fresh offer for the retained client identity", redeemErr)
			}
		}
	}

	replacement, endpoint, err := decodeEnrollmentOffer(request.TrustToken, request.Endpoint, now)
	if err != nil {
		return pendingEnrollment{}, fmt.Errorf("pending Incus identity needs a fresh offer: %w", err)
	}
	if endpoint != candidate.Endpoint {
		return pendingEnrollment{}, fmt.Errorf("fresh Incus offer names a different endpoint")
	}
	if err := attestServerFingerprint(candidate.ServerCertificate, replacement.Fingerprint); err != nil {
		return pendingEnrollment{}, fmt.Errorf("fresh Incus offer names a different server: %w", err)
	}
	candidate.TrustToken = strings.TrimSpace(request.TrustToken)
	if err := dependencies.custody.SavePending(request.PendingPath, candidate); err != nil {
		return pendingEnrollment{}, err
	}
	if err := dependencies.remote.Redeem(ctx, candidate.authority(), replacement.ClientName, candidate.TrustToken); err != nil {
		return pendingEnrollment{}, fmt.Errorf("redeem fresh Incus offer for retained identity: %w", err)
	}
	return candidate, nil
}

func decodeEnrollmentOffer(raw, override string, now time.Time) (*api.CertificateAddToken, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", fmt.Errorf("Incus trust offer is required")
	}
	offer, err := incustls.CertificateTokenDecode(raw)
	if err != nil {
		return nil, "", fmt.Errorf("decode Incus trust offer: %w", err)
	}
	if offer.ExpiresAt.IsZero() || !offer.ExpiresAt.After(now) || offer.ExpiresAt.After(now.Add(MaxEnrollmentOfferLifetime)) {
		return nil, "", fmt.Errorf("Incus trust offer must expire within %s", MaxEnrollmentOfferLifetime)
	}
	endpoints := make(map[string]struct{})
	for _, address := range offer.Addresses {
		endpoint, err := enrollmentEndpoint(address)
		if err == nil {
			endpoints[endpoint] = struct{}{}
		}
	}
	if override != "" {
		endpoint, err := enrollmentEndpoint(override)
		if err != nil {
			return nil, "", fmt.Errorf("invalid Incus endpoint override: %w", err)
		}
		if _, ok := endpoints[endpoint]; !ok {
			return nil, "", fmt.Errorf("Incus endpoint override is not advertised by the trust offer")
		}
		return offer, endpoint, nil
	}
	if len(endpoints) != 1 {
		return nil, "", fmt.Errorf("Incus trust offer must advertise exactly one Tailscale address on TCP 8443 or setup must select one")
	}
	for endpoint := range endpoints {
		return offer, endpoint, nil
	}
	panic("unreachable endpoint selection")
}

func enrollmentEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty address")
	}
	hostPort, err := enrollmentHostPort(raw)
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil || port != "8443" {
		return "", fmt.Errorf("address must use TCP 8443")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !tailscaleIPv4.Contains(ip) && !tailscaleIPv6.Contains(ip) {
		return "", fmt.Errorf("address must be one Tailscale IP")
	}
	return "https://" + net.JoinHostPort(ip.String(), "8443"), nil
}

func enrollmentHostPort(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		return raw, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("address must be one HTTPS origin")
	}
	return parsed.Host, nil
}

func attestServerFingerprint(certificatePEM, fingerprint string) error {
	block, rest := pem.Decode([]byte(strings.TrimSpace(certificatePEM)))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return fmt.Errorf("offered Incus server certificate is invalid")
	}
	want := strings.ToLower(strings.TrimSpace(fingerprint))
	if len(want) != 64 {
		return fmt.Errorf("Incus trust offer has an invalid server fingerprint")
	}
	if _, err := hex.DecodeString(want); err != nil {
		return fmt.Errorf("Incus trust offer has an invalid server fingerprint")
	}
	digest := sha256.Sum256(block.Bytes)
	if hex.EncodeToString(digest[:]) != want {
		return fmt.Errorf("Incus server certificate does not match the trust offer fingerprint")
	}
	return nil
}

func validatePendingEnrollment(candidate pendingEnrollment) error {
	if candidate.Version != pendingEnrollmentVersion {
		return fmt.Errorf("pending Incus enrollment has unsupported version %d", candidate.Version)
	}
	offer, err := incustls.CertificateTokenDecode(candidate.TrustToken)
	if err != nil {
		return fmt.Errorf("decode pending Incus offer: %w", err)
	}
	if offer.ExpiresAt.IsZero() {
		return fmt.Errorf("pending Incus offer has no expiry")
	}
	endpoint, err := enrollmentEndpoint(candidate.Endpoint)
	if err != nil || endpoint != candidate.Endpoint {
		return fmt.Errorf("pending Incus enrollment has an invalid endpoint")
	}
	advertised := false
	for _, address := range offer.Addresses {
		if selected, selectErr := enrollmentEndpoint(address); selectErr == nil && selected == candidate.Endpoint {
			advertised = true
		}
	}
	if !advertised {
		return fmt.Errorf("pending Incus endpoint is not advertised by its offer")
	}
	if err := attestServerFingerprint(candidate.ServerCertificate, offer.Fingerprint); err != nil {
		return err
	}
	authority := candidate.authority()
	if err := authority.Validate(); err != nil {
		return fmt.Errorf("pending Incus authority is invalid: %w", err)
	}
	return nil
}

type sdkEnrollmentRemote struct{}

func (sdkEnrollmentRemote) FetchServerCertificate(ctx context.Context, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, enrollmentFetchTimeout)
	defer cancel()
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", parsed.Host)
	if err != nil {
		return "", err
	}
	tlsConnection := tls.Client(connection, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}) // #nosec G402 -- the offer fingerprint pins the fetched certificate before use.
	defer tlsConnection.Close()
	if err := tlsConnection.HandshakeContext(ctx); err != nil {
		return "", err
	}
	certificates := tlsConnection.ConnectionState().PeerCertificates
	if len(certificates) != 1 {
		return "", fmt.Errorf("Incus endpoint must present one exact server certificate")
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificates[0].Raw})), nil
}

func (sdkEnrollmentRemote) GenerateClientIdentity() (string, string, error) {
	certificate, key, err := incustls.GenerateMemCert(true, false)
	return string(certificate), string(key), err
}

func (sdkEnrollmentRemote) AuthenticateAndProve(ctx context.Context, authority deployment.Incus) (bool, error) {
	client, server, err := connectEnrollmentClient(ctx, authority)
	if err != nil {
		return false, err
	}
	defer client.Disconnect()
	if server.Auth != "trusted" {
		return false, nil
	}
	if !client.HasExtension(requiredPortForwardExtension) || !client.HasExtension("certificate_project") {
		return false, fmt.Errorf("Incus endpoint lacks required project or port-forward capabilities")
	}
	projectClient := client.UseProject(DefaultProject)
	project, _, err := projectClient.GetProject(DefaultProject)
	if err != nil {
		return false, fmt.Errorf("read required Incus project %s: %w", DefaultProject, err)
	}
	if err := attestPreparedEnrollmentProject(project); err != nil {
		return false, err
	}
	if _, _, err := projectClient.GetStoragePool(DefaultStoragePool); err != nil {
		return false, fmt.Errorf("read required Incus storage pool %s: %w", DefaultStoragePool, err)
	}
	network, _, err := projectClient.GetNetwork("incusbr0")
	if err != nil {
		return false, fmt.Errorf("read required Incus network incusbr0: %w", err)
	}
	if !network.Managed || network.Name != "incusbr0" {
		return false, fmt.Errorf("required Incus network incusbr0 is not managed")
	}
	_, outsideErr := client.UseProject(api.ProjectDefaultName).GetInstanceNames(api.InstanceTypeAny)
	if err := attestOutsideProjectDenial(outsideErr); err != nil {
		return false, err
	}
	return true, nil
}

func attestOutsideProjectDenial(err error) error {
	if err == nil {
		return fmt.Errorf("Incus client identity can access instances outside project %s", DefaultProject)
	}
	if !api.StatusErrorCheck(err, http.StatusForbidden) {
		return fmt.Errorf("prove Incus denial outside project %s: %w", DefaultProject, err)
	}
	return nil
}

func attestPreparedEnrollmentProject(project *api.Project) error {
	if project == nil || project.Name != DefaultProject {
		return fmt.Errorf("required Incus project %s is missing", DefaultProject)
	}
	required := map[string]string{
		"restricted":                      "true",
		"features.images":                 "true",
		"features.networks":               "false",
		"features.profiles":               "true",
		"features.storage.volumes":        "true",
		"restricted.networks.access":      "incusbr0",
		"restricted.storage-pools.access": DefaultStoragePool,
	}
	for key, value := range required {
		if project.Config[key] != value {
			return fmt.Errorf("Incus project %s requires %s=%s", DefaultProject, key, value)
		}
	}
	return nil
}

func (sdkEnrollmentRemote) Redeem(ctx context.Context, authority deployment.Incus, clientName, trustToken string) error {
	client, _, err := connectEnrollmentClient(ctx, authority)
	if err != nil {
		return err
	}
	defer client.Disconnect()
	return client.CreateCertificate(api.CertificatesPost{
		CertificatePut: api.CertificatePut{
			Name: clientName, Type: api.CertificateTypeClient, Certificate: authority.ClientCertificate,
		},
		TrustToken: trustToken,
	})
}

func connectEnrollmentClient(ctx context.Context, authority deployment.Incus) (incusclient.InstanceServer, *api.Server, error) {
	client, err := incusclient.ConnectIncusWithContext(ctx, authority.Endpoint, &incusclient.ConnectionArgs{
		TLSServerCert: authority.ServerCertificate, TLSClientCert: authority.ClientCertificate, TLSClientKey: authority.ClientPrivateKey,
		IdenticalCertificate: true, SkipGetEvents: true, Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	})
	if err != nil {
		return nil, nil, err
	}
	server, _, err := client.GetServer()
	if err != nil {
		client.Disconnect()
		return nil, nil, err
	}
	return client, server, nil
}

type filesystemEnrollmentCustody struct{}

func (filesystemEnrollmentCustody) Lock(path string) (func(), error) {
	directory, name, err := openEnrollmentDirectory(path)
	if err != nil {
		return nil, err
	}
	lockName := "." + name + ".lock"
	fd, err := unix.Openat(int(directory.Fd()), lockName, unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		directory.Close()
		return nil, fmt.Errorf("open Incus enrollment lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), lockName)
	info, err := lock.Stat()
	if err != nil || !protectedEnrollmentFile(info) {
		lock.Close()
		directory.Close()
		return nil, fmt.Errorf("Incus enrollment lock must be operator-owned mode 0600")
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		lock.Close()
		directory.Close()
		return nil, fmt.Errorf("lock Incus enrollment: %w", err)
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = lock.Close()
		_ = directory.Close()
	}, nil
}

func (filesystemEnrollmentCustody) LoadPending(path string) (pendingEnrollment, bool, error) {
	contents, err := readProtectedEnrollmentFile(path, maxPendingEnrollmentBytes)
	if errors.Is(err, unix.ENOENT) {
		return pendingEnrollment{}, false, nil
	}
	if err != nil {
		return pendingEnrollment{}, false, fmt.Errorf("read pending Incus enrollment: %w", err)
	}
	var candidate pendingEnrollment
	if err := json.Unmarshal(contents, &candidate); err != nil {
		return pendingEnrollment{}, false, fmt.Errorf("decode pending Incus enrollment: %w", err)
	}
	if err := validatePendingEnrollment(candidate); err != nil {
		return pendingEnrollment{}, false, err
	}
	return candidate, true, nil
}

func readProtectedEnrollmentFile(path string, maximum int64) ([]byte, error) {
	directory, file, name, before, err := openProtectedEnrollmentFile(path, maximum)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(contents)) > maximum {
		return nil, fmt.Errorf("protected Incus enrollment file exceeds its read bound")
	}
	if enrollmentReadCompletedForTest != nil {
		enrollmentReadCompletedForTest()
	}
	if err := reattestProtectedEnrollmentFile(directory, file, name, before); err != nil {
		return nil, err
	}
	return contents, nil
}

func openProtectedEnrollmentFile(path string, maximum int64) (*os.File, *os.File, string, os.FileInfo, error) {
	directory, name, err := openEnrollmentDirectory(path)
	if err != nil {
		return nil, nil, "", nil, err
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		directory.Close()
		return nil, nil, "", nil, fmt.Errorf("open protected Incus enrollment file: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	before, err := file.Stat()
	if err != nil || !protectedEnrollmentFile(before) || before.Size() > maximum {
		file.Close()
		directory.Close()
		return nil, nil, "", nil, fmt.Errorf("Incus enrollment input must be one operator-owned mode-0600 bounded regular file")
	}
	return directory, file, name, before, nil
}

func reattestProtectedEnrollmentFile(directory, file *os.File, name string, before os.FileInfo) error {
	after, err := file.Stat()
	var current unix.Stat_t
	if err != nil || !protectedEnrollmentFile(after) || !sameEnrollmentFile(before, after) || unix.Fstatat(int(directory.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!protectedEnrollmentStat(current) || !sameEnrollmentStat(before, current) || verifyEnrollmentDirectory(directory) != nil {
		return fmt.Errorf("protected Incus enrollment file changed while it was read")
	}
	return nil
}

func (filesystemEnrollmentCustody) SavePending(path string, candidate pendingEnrollment) error {
	if err := validatePendingEnrollment(candidate); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if len(contents) > maxPendingEnrollmentBytes {
		return fmt.Errorf("pending Incus enrollment exceeds 128 KiB")
	}
	directory, name, err := openEnrollmentDirectory(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	temporary, temporaryName, err := createEnrollmentTemporary(directory)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(directory.Fd()), temporaryName, 0) }()
	if written, err := temporary.Write(contents); err != nil || written != len(contents) {
		temporary.Close()
		return fmt.Errorf("write pending Incus enrollment")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync pending Incus enrollment: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close pending Incus enrollment: %w", err)
	}
	if err := unix.Renameat(int(directory.Fd()), temporaryName, int(directory.Fd()), name); err != nil {
		return fmt.Errorf("commit pending Incus enrollment: %w", err)
	}
	if err := unix.Fsync(int(directory.Fd())); err != nil {
		return fmt.Errorf("sync pending Incus enrollment directory: %w", err)
	}
	return nil
}

func (custody filesystemEnrollmentCustody) RemovePending(path string, expected pendingEnrollment) error {
	current, found, err := custody.LoadPending(path)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if current != expected {
		return fmt.Errorf("pending Incus enrollment changed before cleanup")
	}
	directory, name, err := openEnrollmentDirectory(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
		return fmt.Errorf("remove pending Incus enrollment: %w", err)
	}
	if err := unix.Fsync(int(directory.Fd())); err != nil {
		return fmt.Errorf("sync pending Incus enrollment cleanup: %w", err)
	}
	return nil
}

func (filesystemEnrollmentCustody) LoadAccepted(path string) (*deployment.Incus, error) {
	configuration, found, err := deployment.Load(path)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("Dorf deployment is not initialized")
	}
	return configuration.Incus, nil
}

func (filesystemEnrollmentCustody) RetainAccepted(path string, authority deployment.Incus) error {
	return deployment.RetainIncus(path, authority)
}

func openEnrollmentDirectory(path string) (*os.File, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." || filepath.Base(path) == string(filepath.Separator) {
		return nil, "", fmt.Errorf("pending Incus enrollment must use one clean absolute path")
	}
	directoryPath := filepath.Dir(path)
	directory, err := unix.Open(directoryPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open pending Incus enrollment directory: %w", err)
	}
	file := os.NewFile(uintptr(directory), directoryPath)
	info, err := file.Stat()
	if err != nil || !protectedEnrollmentDirectory(info) {
		file.Close()
		return nil, "", fmt.Errorf("pending Incus enrollment directory must be one operator-owned mode-0700 directory")
	}
	resolved, err := filepath.EvalSymlinks(directoryPath)
	if err != nil || resolved != directoryPath {
		file.Close()
		return nil, "", fmt.Errorf("pending Incus enrollment directory must not traverse symlinks")
	}
	return file, filepath.Base(path), nil
}

func createEnrollmentTemporary(directory *os.File) (*os.File, string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, "", err
		}
		name := ".incus-enrollment-" + hex.EncodeToString(random) + ".json"
		fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create temporary Incus enrollment: %w", err)
		}
		if err := unix.Fchmod(fd, 0o600); err != nil {
			unix.Close(fd)
			return nil, "", err
		}
		return os.NewFile(uintptr(fd), name), name, nil
	}
	return nil, "", fmt.Errorf("create unique temporary Incus enrollment")
}

func protectedEnrollmentDirectory(info os.FileInfo) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && info.Mode().Perm() == 0o700 && int(owner.Uid) == os.Geteuid() && int(owner.Gid) == os.Getegid()
}

func protectedEnrollmentFile(info os.FileInfo) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && int(owner.Uid) == os.Geteuid() && int(owner.Gid) == os.Getegid()
}

func protectedEnrollmentStat(info unix.Stat_t) bool {
	return info.Mode&unix.S_IFMT == unix.S_IFREG && info.Mode&0o777 == 0o600 && int(info.Uid) == os.Geteuid() && int(info.Gid) == os.Getegid()
}

func verifyEnrollmentDirectory(directory *os.File) error {
	info, err := directory.Stat()
	if err != nil || !protectedEnrollmentDirectory(info) {
		return fmt.Errorf("pending Incus enrollment directory changed while it was used")
	}
	return nil
}

func sameEnrollmentFile(a, b os.FileInfo) bool {
	left, lok := a.Sys().(*syscall.Stat_t)
	right, rok := b.Sys().(*syscall.Stat_t)
	return lok && rok && left.Dev == right.Dev && left.Ino == right.Ino && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}

func sameEnrollmentStat(info os.FileInfo, current unix.Stat_t) bool {
	initial, ok := info.Sys().(*syscall.Stat_t)
	return ok && initial.Dev == current.Dev && initial.Ino == current.Ino
}

func mustCIDR(value string) *net.IPNet {
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		panic(err)
	}
	return network
}
