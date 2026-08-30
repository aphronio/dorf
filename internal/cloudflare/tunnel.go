package cloudflare

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	BinaryVersion        = "2026.8.2"
	ComposeControlOrigin = "http://control-api:8745"
	ComposeGatewayOrigin = "http://provider-gateway:8317"
	binarySHA256         = "fcfb02b575a52ca1af2e3267af4e1517bcdeb30ac48c834c69abaed3c0576ad2"
	binaryURL            = "https://github.com/cloudflare/cloudflared/releases/download/" + BinaryVersion + "/cloudflared-linux-amd64"
)

var hostnamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
var probeIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type CommandRunner interface {
	Run(context.Context, []string, io.Writer, io.Writer, string, ...string) error
	Output(context.Context, []string, string, ...string) (string, error)
}

type DNSResolver interface {
	LookupHost(context.Context, string) ([]string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, env []string, stdout, stderr io.Writer, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = mergeEnv(os.Environ(), env)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, stdout, stderr
	return command.Run()
}

func (ExecRunner) Output(ctx context.Context, env []string, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = mergeEnv(os.Environ(), env)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("%s: %w", detail, err)
	}
	return stdout.String(), nil
}

func mergeEnv(base, overrides []string) []string {
	replaced := make(map[string]struct{}, len(overrides))
	for _, item := range overrides {
		if key, _, ok := strings.Cut(item, "="); ok {
			replaced[key] = struct{}{}
		}
	}
	merged := make([]string, 0, len(base)+len(overrides))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if _, replace := replaced[key]; ok && replace {
			continue
		}
		merged = append(merged, item)
	}
	return append(merged, overrides...)
}

type Tunnel struct {
	StatePath          string
	Binary             string
	ReplaceExistingDNS bool
	Runner             CommandRunner
	HTTPClient         *http.Client
	Resolver           DNSResolver
	// binarySHA256 is a package-private test seam. Production always uses the
	// compiled cloudflared digest above.
	binarySHA256 string
}

type State struct {
	SchemaVersion         int    `json:"schema_version"`
	TunnelName            string `json:"tunnel_name"`
	TunnelID              string `json:"tunnel_id,omitempty"`
	Hostname              string `json:"hostname"`
	ModelHostname         string `json:"model_hostname,omitempty"`
	Origin                string `json:"origin"`
	CredentialPath        string `json:"credential_path,omitempty"`
	ConfigPath            string `json:"config_path,omitempty"`
	BinaryPath            string `json:"binary_path,omitempty"`
	ProbeID               string `json:"probe_id,omitempty"`
	DNSConfigured         bool   `json:"dns_configured"`
	ModelDNSConfigured    bool   `json:"model_dns_configured"`
	DNSReplacementPending bool   `json:"dns_replacement_pending,omitempty"`
}

type ComposeState struct {
	StatePath string
	Digest    string
}

type dnsRoute struct {
	hostname   string
	label      string
	configured *bool
}

func dnsRoutes(state *State) []dnsRoute {
	return []dnsRoute{
		{hostname: state.Hostname, label: "Cloudflare hostname", configured: &state.DNSConfigured},
		{hostname: state.ModelHostname, label: "Cloudflare model hostname", configured: &state.ModelDNSConfigured},
	}
}

func (s State) ProbeURL(hostname string) (string, error) {
	hostname, err := canonicalHostname(hostname)
	if err != nil {
		return "", err
	}
	if hostname != s.Hostname && hostname != s.ModelHostname {
		return "", fmt.Errorf("Cloudflare hostname %s is not owned by this Tunnel", hostname)
	}
	baseURL, err := ControlURL(hostname)
	if err != nil {
		return "", err
	}
	if !probeIDPattern.MatchString(s.ProbeID) {
		return "", fmt.Errorf("Cloudflare Tunnel deployment probe identity is invalid; rerun dorf setup")
	}
	return baseURL + "/.dorf/probe/" + s.ProbeID, nil
}

type listedTunnel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (t Tunnel) Current() (State, bool, error) {
	if strings.TrimSpace(t.StatePath) == "" {
		return State{}, false, fmt.Errorf("Cloudflare Tunnel state path is empty")
	}
	return t.load()
}

// ComposeState returns the exact prepared runtime authority. It never creates
// or repairs Tunnel state.
func (t Tunnel) ComposeState() (ComposeState, bool, error) {
	statePath := filepath.Clean(strings.TrimSpace(t.StatePath))
	if !filepath.IsAbs(statePath) || statePath == "/" || statePath != t.StatePath {
		return ComposeState{}, false, fmt.Errorf("Cloudflare Tunnel state path must be one clean absolute path")
	}
	state, found, err := t.load()
	if err != nil || !found {
		return ComposeState{}, false, err
	}
	if !state.DNSConfigured || !state.ModelDNSConfigured || state.ModelHostname == "" || state.TunnelID == "" || state.CredentialPath == "" || state.ConfigPath == "" || state.BinaryPath == "" {
		return ComposeState{}, false, nil
	}
	if err := t.attestPreparedRuntime(state); err != nil {
		return ComposeState{}, false, err
	}
	digest := sha256.New()
	for _, item := range []struct {
		name string
		path string
	}{{"state.json", t.statePath()}, {"credential", state.CredentialPath}, {"config", state.ConfigPath}, {"cloudflared", state.BinaryPath}} {
		path := filepath.Clean(item.path)
		if !pathWithin(statePath, path) {
			return ComposeState{}, false, fmt.Errorf("Cloudflare Tunnel %s path leaves its protected state directory", item.name)
		}
		contents, err := readProtectedRuntimeFile(path, 128<<20)
		if err != nil {
			return ComposeState{}, false, fmt.Errorf("attest Cloudflare Tunnel %s: %w", item.name, err)
		}
		fmt.Fprintf(digest, "%s\x00%d\x00", item.name, len(contents))
		_, _ = digest.Write(contents)
	}
	return ComposeState{StatePath: statePath, Digest: hex.EncodeToString(digest.Sum(nil))}, true, nil
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func readProtectedRuntimeFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() < 1 || info.Size() > limit {
		return nil, fmt.Errorf("must be one protected nonempty regular file")
	}
	return os.ReadFile(path)
}

// RunForeground runs the prepared Tunnel in the caller's foreground. Process
// supervision belongs to the caller (for example, Docker Compose).
func (t Tunnel) RunForeground(ctx context.Context, stdout, stderr io.Writer) error {
	if strings.TrimSpace(t.StatePath) == "" {
		return fmt.Errorf("Cloudflare Tunnel state path is empty")
	}
	state, found, err := t.load()
	if err != nil {
		return err
	}
	if !found || !state.DNSConfigured || !state.ModelDNSConfigured || strings.TrimSpace(state.ModelHostname) == "" || strings.TrimSpace(state.TunnelID) == "" || strings.TrimSpace(state.CredentialPath) == "" || strings.TrimSpace(state.ConfigPath) == "" || strings.TrimSpace(state.BinaryPath) == "" {
		return fmt.Errorf("Cloudflare Tunnel is not prepared; rerun dorf setup")
	}
	if err := t.attestPreparedRuntime(state); err != nil {
		return err
	}
	runner := t.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	if err := runner.Run(ctx, nil, stdout, stderr, state.BinaryPath, "--no-autoupdate", "--config", state.ConfigPath, "tunnel", "run"); err != nil {
		return fmt.Errorf("run Cloudflare Tunnel: %w", err)
	}
	return nil
}

// PrepareRuntimeBinary installs and verifies the current pinned cloudflared
// executable for an already retained Tunnel and converges its deterministic
// local config. It performs no browser, account, DNS, credential, or external
// ingress mutation. Approved update reconciliation calls it before deriving
// ComposeState.
func (t Tunnel) PrepareRuntimeBinary(ctx context.Context) (bool, error) {
	statePath := filepath.Clean(strings.TrimSpace(t.StatePath))
	if !filepath.IsAbs(statePath) || statePath == "/" || statePath != t.StatePath {
		return false, fmt.Errorf("Cloudflare Tunnel state path must be one clean absolute path")
	}
	state, found, err := t.load()
	if err != nil || !found {
		return false, err
	}
	if !state.DNSConfigured || !state.ModelDNSConfigured || state.ModelHostname == "" || state.TunnelID == "" || state.CredentialPath == "" || state.ConfigPath == "" {
		return false, nil
	}
	lock, err := os.OpenFile(filepath.Join(statePath, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	state, found, err = t.load()
	if err != nil || !found {
		return false, err
	}
	if !state.DNSConfigured || !state.ModelDNSConfigured || state.ModelHostname == "" || state.TunnelID == "" || state.CredentialPath == "" || state.ConfigPath == "" {
		return false, nil
	}
	if err := t.repairRetainedConfig(state); err != nil {
		return false, err
	}
	binary, err := t.ensureBinary(ctx)
	if err != nil {
		return false, err
	}
	if state.BinaryPath != binary {
		state.BinaryPath = binary
		if err := t.save(state); err != nil {
			return false, err
		}
	}
	if err := t.attestPreparedRuntime(state); err != nil {
		return false, err
	}
	return true, nil
}

func (t Tunnel) attestPreparedRuntime(state State) error {
	if err := t.attestRetainedConfig(state); err != nil {
		return err
	}
	return t.verifyPinnedBinary(state.BinaryPath)
}

func (t Tunnel) attestRetainedConfig(state State) error {
	if err := t.attestRetainedCredential(state); err != nil {
		return err
	}
	config, err := readProtectedRuntimeFile(state.ConfigPath, 1<<20)
	if err != nil {
		return fmt.Errorf("attest Cloudflare Tunnel config: %w", err)
	}
	actual := sha256.Sum256(config)
	expected := sha256.Sum256([]byte(t.config(state)))
	if actual != expected {
		return fmt.Errorf("Cloudflare Tunnel config checksum mismatch; rerun dorf setup")
	}
	return nil
}

func (t Tunnel) attestRetainedCredential(state State) error {
	statePath := filepath.Clean(strings.TrimSpace(t.StatePath))
	for _, item := range []struct {
		name string
		got  string
		want string
	}{
		{"credential", state.CredentialPath, filepath.Join(statePath, "credentials", state.TunnelID+".json")},
		{"config", state.ConfigPath, filepath.Join(statePath, "config.yml")},
	} {
		if item.got != item.want {
			return fmt.Errorf("Cloudflare Tunnel %s path does not match its exact retained path", item.name)
		}
		if !pathWithin(statePath, item.got) {
			return fmt.Errorf("Cloudflare Tunnel %s path leaves its protected state directory", item.name)
		}
	}
	if _, err := readProtectedRuntimeFile(state.CredentialPath, 16<<20); err != nil {
		return fmt.Errorf("attest exact Cloudflare Tunnel credential: %w", err)
	}
	return nil
}

func (t Tunnel) repairRetainedConfig(state State) error {
	if err := t.attestRetainedCredential(state); err != nil {
		return err
	}
	return writePrivate(state.ConfigPath, []byte(t.config(state)))
}

func (t Tunnel) verifyPinnedBinary(path string) error {
	statePath := filepath.Clean(strings.TrimSpace(t.StatePath))
	path = filepath.Clean(strings.TrimSpace(path))
	if !pathWithin(statePath, path) {
		return fmt.Errorf("Cloudflare Tunnel binary path leaves its protected state directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("attest cloudflared executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o100 == 0 {
		return fmt.Errorf("attest cloudflared executable: must be one protected owner-executable regular file")
	}
	raw, err := readProtectedRuntimeFile(path, 128<<20)
	if err != nil {
		return fmt.Errorf("attest cloudflared executable: %w", err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != t.expectedBinarySHA256() {
		return fmt.Errorf("cloudflared %s checksum mismatch; rerun dorf setup", BinaryVersion)
	}
	return nil
}

func (t Tunnel) expectedBinarySHA256() string {
	if strings.TrimSpace(t.binarySHA256) != "" {
		return strings.ToLower(strings.TrimSpace(t.binarySHA256))
	}
	return binarySHA256
}

func canonicalHostname(hostname string) (string, error) {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if !hostnamePattern.MatchString(hostname) || net.ParseIP(hostname) != nil {
		return "", fmt.Errorf("Cloudflare hostname must be one complete DNS hostname")
	}
	return hostname, nil
}

func GatewayURL(hostname string) (string, error) {
	hostname, err := canonicalHostname(hostname)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse("https://" + hostname + "/v1")
	if err != nil || parsed.Hostname() != hostname {
		return "", fmt.Errorf("Cloudflare hostname is invalid")
	}
	return parsed.String(), nil
}

func ControlURL(hostname string) (string, error) {
	hostname, err := canonicalHostname(hostname)
	if err != nil {
		return "", err
	}
	return "https://" + hostname, nil
}

// Prepare creates one locally managed, outbound-only Cloudflare Tunnel and
// retains only the credential and config needed to run that exact Tunnel. It
// does not install or start a host service.
func (t Tunnel) Prepare(ctx context.Context, controlHostname, modelHostname string, stdout, stderr io.Writer) (State, error) {
	return t.reconcile(ctx, controlHostname, modelHostname, stdout, stderr)
}

// A broad account certificate exists only while the external Tunnel and DNS
// route are being reconciled. The retained credential can run only the exact
// Tunnel recorded in state.
func (t Tunnel) reconcile(ctx context.Context, controlHostname, modelHostname string, stdout, stderr io.Writer) (State, error) {
	controlHostname, err := canonicalHostname(controlHostname)
	if err != nil {
		return State{}, fmt.Errorf("validate Cloudflare control hostname: %w", err)
	}
	modelHostname, err = canonicalHostname(modelHostname)
	if err != nil {
		return State{}, fmt.Errorf("validate Cloudflare model hostname: %w", err)
	}
	if controlHostname == modelHostname {
		return State{}, fmt.Errorf("Cloudflare control and model hostnames must be different")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return State{}, fmt.Errorf("guided Cloudflare Tunnel setup supports only x86_64 Linux")
	}
	if strings.TrimSpace(t.StatePath) == "" {
		return State{}, fmt.Errorf("Cloudflare Tunnel state path is empty")
	}
	preexisting, found, err := t.load()
	if err != nil {
		return State{}, err
	}
	if found && preexisting.ModelHostname == "" {
		return State{}, fmt.Errorf("Cloudflare Tunnel uses the retired single-origin hostname layout")
	}
	if t.Runner == nil {
		t.Runner = ExecRunner{}
	}
	if err := os.MkdirAll(t.StatePath, 0o700); err != nil {
		return State{}, err
	}
	if err := os.Chmod(t.StatePath, 0o700); err != nil {
		return State{}, err
	}
	lock, err := os.OpenFile(filepath.Join(t.StatePath, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return State{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return State{}, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	state, found, err := t.load()
	if err != nil {
		return State{}, err
	}
	if found {
		if state.Hostname != controlHostname {
			return State{}, fmt.Errorf("Dorf already owns Cloudflare hostname %s; refusing to replace it", state.Hostname)
		}
		if state.ModelHostname == "" {
			return State{}, fmt.Errorf("Cloudflare Tunnel uses the retired single-origin hostname layout")
		}
		if state.ModelHostname != modelHostname {
			return State{}, fmt.Errorf("Dorf already owns Cloudflare model hostname %s; refusing to replace it", state.ModelHostname)
		}
	} else {
		nonce, err := randomHex(4)
		if err != nil {
			return State{}, err
		}
		probeID, err := randomHex(16)
		if err != nil {
			return State{}, err
		}
		state = State{SchemaVersion: 1, TunnelName: "dorf-" + nonce, Hostname: controlHostname, ModelHostname: modelHostname, Origin: ComposeGatewayOrigin, ProbeID: probeID}
		if err := t.save(state); err != nil {
			return State{}, err
		}
	}
	if state.ProbeID == "" {
		state.ProbeID, err = randomHex(16)
		if err != nil {
			return state, err
		}
		if err := t.save(state); err != nil {
			return state, err
		}
	}
	if t.ReplaceExistingDNS && !state.DNSReplacementPending {
		state.DNSReplacementPending = true
		state.DNSConfigured = false
		state.ModelDNSConfigured = false
		if err := t.save(state); err != nil {
			return state, err
		}
	}
	for _, route := range dnsRoutes(&state) {
		if !*route.configured {
			continue
		}
		present, err := t.dnsRoutePresent(ctx, route.hostname)
		if err != nil {
			return state, err
		}
		if present {
			continue
		}
		*route.configured = false
		if err := t.save(state); err != nil {
			return state, err
		}
	}
	binary, err := t.ensureBinary(ctx)
	if err != nil {
		return state, err
	}
	state.BinaryPath = binary
	if err := t.save(state); err != nil {
		return state, err
	}
	managementHome := filepath.Join(t.StatePath, "management")
	cloudflaredHome := filepath.Join(managementHome, ".cloudflared")
	certificate := filepath.Join(cloudflaredHome, "cert.pem")
	env := []string{"HOME=" + managementHome}
	if state.TunnelID == "" || !state.DNSConfigured || !state.ModelDNSConfigured {
		if _, err := os.Stat(certificate); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(cloudflaredHome, 0o700); err != nil {
				return state, err
			}
			fmt.Fprintln(stdout, "Authorize Cloudflare in the browser, then return here.")
			if err := t.Runner.Run(ctx, env, stdout, stderr, binary, "tunnel", "login"); err != nil {
				return state, fmt.Errorf("authorize Cloudflare Tunnel: %w", err)
			}
			if _, err := os.Stat(certificate); err != nil {
				return state, fmt.Errorf("Cloudflare authorization did not produce its temporary account certificate")
			}
		} else if err != nil {
			return state, err
		}
	}
	if state.TunnelID == "" {
		if err := t.reconcileTunnelCredential(ctx, &state, binary, env, certificate, cloudflaredHome, stdout, stderr); err != nil {
			return state, err
		}
	}
	credential, err := os.Stat(state.CredentialPath)
	if err != nil {
		return state, fmt.Errorf("attest exact Cloudflare Tunnel credential: %w", err)
	}
	if !credential.Mode().IsRegular() || credential.Mode().Perm()&0o077 != 0 {
		return state, fmt.Errorf("Cloudflare Tunnel credential is not one protected regular file")
	}
	state.ConfigPath = filepath.Join(t.StatePath, "config.yml")
	if err := writePrivate(state.ConfigPath, []byte(t.config(state))); err != nil {
		return state, err
	}
	if _, err := t.Runner.Output(ctx, env, binary, "tunnel", "--config", state.ConfigPath, "ingress", "validate"); err != nil {
		return state, fmt.Errorf("validate Cloudflare Tunnel ingress: %w", err)
	}
	// Cloudflare atomically creates an absent record, accepts the existing
	// CNAME only when it already targets this exact Tunnel, and rejects every
	// foreign record unless reconciliation carries the operator's retained
	// replacement choice.
	for _, route := range dnsRoutes(&state) {
		if *route.configured {
			continue
		}
		routeArgs := []string{"tunnel", "--origincert", certificate, "route", "dns"}
		if state.DNSReplacementPending {
			routeArgs = append(routeArgs, "--overwrite-dns")
		}
		routeArgs = append(routeArgs, state.TunnelID, route.hostname)
		if err := t.Runner.Run(ctx, env, stdout, stderr, binary, routeArgs...); err != nil {
			return state, fmt.Errorf("route %s: %w", route.label, err)
		}
		*route.configured = true
		if err := t.save(state); err != nil {
			return state, err
		}
	}
	if state.DNSReplacementPending {
		state.DNSReplacementPending = false
		if err := t.save(state); err != nil {
			return state, err
		}
	}
	if err := os.RemoveAll(managementHome); err != nil {
		return state, fmt.Errorf("remove temporary Cloudflare account authority: %w", err)
	}
	return state, nil
}

func (t Tunnel) reconcileTunnelCredential(ctx context.Context, state *State, binary string, env []string, certificate, cloudflaredHome string, stdout, stderr io.Writer) error {
	matches, err := t.listByName(ctx, binary, env, certificate, state.TunnelName)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		if err := t.Runner.Run(ctx, env, stdout, stderr, binary, "tunnel", "--origincert", certificate, "create", state.TunnelName); err != nil {
			return fmt.Errorf("create Cloudflare Tunnel: %w", err)
		}
		matches, err = t.listByName(ctx, binary, env, certificate, state.TunnelName)
		if err != nil {
			return err
		}
	}
	if len(matches) != 1 || strings.TrimSpace(matches[0].ID) == "" {
		return fmt.Errorf("Cloudflare returned %d exact Tunnels named %s; refusing ambiguous ownership", len(matches), state.TunnelName)
	}
	state.TunnelID = matches[0].ID
	credentialSource := filepath.Join(cloudflaredHome, state.TunnelID+".json")
	credentialDir := filepath.Join(t.StatePath, "credentials")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		return err
	}
	state.CredentialPath = filepath.Join(credentialDir, state.TunnelID+".json")
	if _, err := os.Stat(state.CredentialPath); errors.Is(err, os.ErrNotExist) {
		if _, sourceErr := os.Stat(credentialSource); sourceErr == nil {
			if err := os.Rename(credentialSource, state.CredentialPath); err != nil {
				return fmt.Errorf("retain exact Cloudflare Tunnel credential: %w", err)
			}
		} else if errors.Is(sourceErr, os.ErrNotExist) {
			// A create response can be lost after Cloudflare commits the Tunnel
			// but before cloudflared writes its local credential. The temporary
			// account certificate can recover the exact named Tunnel without
			// creating a duplicate.
			if err := t.Runner.Run(ctx, env, stdout, stderr, binary, "tunnel", "--origincert", certificate, "token", "--cred-file", state.CredentialPath, state.TunnelID); err != nil {
				return fmt.Errorf("recover exact Cloudflare Tunnel credential: %w", err)
			}
		} else {
			return sourceErr
		}
	} else if err != nil {
		return err
	}
	if err := os.Chmod(state.CredentialPath, 0o600); err != nil {
		return err
	}
	return t.save(*state)
}

func (t Tunnel) dnsRoutePresent(ctx context.Context, hostname string) (bool, error) {
	resolver := t.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addresses, err := resolver.LookupHost(ctx, hostname)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return false, nil
		}
		return false, fmt.Errorf("inspect managed Cloudflare hostname %s: %w", hostname, err)
	}
	return len(addresses) > 0, nil
}

func (t Tunnel) listByName(ctx context.Context, binary string, env []string, certificate, name string) ([]listedTunnel, error) {
	output, err := t.Runner.Output(ctx, env, binary, "tunnel", "--origincert", certificate, "list", "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("list Cloudflare Tunnels: %w", err)
	}
	var listed []listedTunnel
	if err := json.Unmarshal([]byte(output), &listed); err != nil {
		return nil, fmt.Errorf("decode Cloudflare Tunnel inventory: %w", err)
	}
	var matches []listedTunnel
	for _, item := range listed {
		if item.Name == name {
			matches = append(matches, item)
		}
	}
	return matches, nil
}

func (t Tunnel) config(state State) string {
	return fmt.Sprintf("tunnel: %q\ncredentials-file: %q\nno-autoupdate: true\ningress:\n  - hostname: %q\n    path: ^/\\.dorf/probe/%s$\n    service: http_status:204\n  - hostname: %q\n    path: ^/\\.dorf/probe/%s$\n    service: http_status:204\n  - hostname: %q\n    path: ^/v1(/.*)?$\n    service: %q\n  - hostname: %q\n    path: ^/v1(/.*)?$\n    service: %q\n  - service: http_status:404\n", state.TunnelID, state.CredentialPath, state.Hostname, state.ProbeID, state.ModelHostname, state.ProbeID, state.Hostname, ComposeControlOrigin, state.ModelHostname, ComposeGatewayOrigin)
}

func randomHex(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (t Tunnel) ensureBinary(ctx context.Context) (string, error) {
	if strings.TrimSpace(t.Binary) != "" {
		if err := t.verifyPinnedBinary(t.Binary); err != nil {
			return "", err
		}
		return t.Binary, nil
	}
	destination := filepath.Join(t.StatePath, "bin", BinaryVersion, "cloudflared")
	if _, err := os.Lstat(destination); err == nil {
		if err := t.verifyPinnedBinary(destination); err != nil {
			return "", err
		}
		return destination, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	client := t.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, binaryURL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download cloudflared %s: %w", BinaryVersion, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download cloudflared %s: HTTP %d", BinaryVersion, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 128<<20))
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != t.expectedBinarySHA256() {
		return "", fmt.Errorf("cloudflared %s checksum mismatch", BinaryVersion)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", err
	}
	if err := writePrivateMode(destination, raw, 0o700); err != nil {
		return "", err
	}
	if err := t.verifyPinnedBinary(destination); err != nil {
		return "", err
	}
	return destination, nil
}

func (t Tunnel) statePath() string { return filepath.Join(t.StatePath, "state.json") }

func (t Tunnel) load() (State, bool, error) {
	path := t.statePath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() < 1 || info.Size() > 64<<10 {
		return State{}, false, fmt.Errorf("Cloudflare Tunnel state must be one protected nonempty regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return State{}, false, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, false, err
	}
	if state.SchemaVersion != 1 || state.TunnelName == "" || state.Hostname == "" || state.Origin == "" {
		return State{}, false, fmt.Errorf("Cloudflare Tunnel state is invalid")
	}
	if state.ModelHostname == "" && state.ModelDNSConfigured {
		return State{}, false, fmt.Errorf("Cloudflare Tunnel state has an invalid model hostname")
	}
	controlHostname, err := canonicalHostname(state.Hostname)
	if err != nil || controlHostname != state.Hostname {
		return State{}, false, fmt.Errorf("Cloudflare Tunnel state has an invalid control hostname")
	}
	if state.ModelHostname != "" {
		modelHostname, err := canonicalHostname(state.ModelHostname)
		if err != nil || modelHostname != state.ModelHostname || modelHostname == state.Hostname {
			return State{}, false, fmt.Errorf("Cloudflare Tunnel state has an invalid model hostname")
		}
	}
	return state, true, nil
}

func (t Tunnel) save(state State) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(t.statePath(), append(raw, '\n'))
}

func writePrivate(path string, raw []byte) error { return writePrivateMode(path, raw, 0o600) }

func writePrivateMode(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dorf-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
