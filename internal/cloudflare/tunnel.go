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
	BinaryVersion = "2026.8.2"
	binarySHA256  = "fcfb02b575a52ca1af2e3267af4e1517bcdeb30ac48c834c69abaed3c0576ad2"
	binaryURL     = "https://github.com/cloudflare/cloudflared/releases/download/" + BinaryVersion + "/cloudflared-linux-amd64"
)

var hostnamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

type CommandRunner interface {
	Run(context.Context, []string, io.Writer, io.Writer, string, ...string) error
	Output(context.Context, []string, string, ...string) (string, error)
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
	StatePath  string
	Binary     string
	Origin     string
	Runner     CommandRunner
	HTTPClient *http.Client
	RootPrefix []string
}

type State struct {
	SchemaVersion    int    `json:"schema_version"`
	TunnelName       string `json:"tunnel_name"`
	TunnelID         string `json:"tunnel_id,omitempty"`
	Hostname         string `json:"hostname"`
	Origin           string `json:"origin"`
	CredentialPath   string `json:"credential_path,omitempty"`
	ConfigPath       string `json:"config_path,omitempty"`
	DNSConfigured    bool   `json:"dns_configured"`
	ServiceInstalled bool   `json:"service_installed"`
	Complete         bool   `json:"complete"`
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

func GatewayURL(hostname string) (string, error) {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if !hostnamePattern.MatchString(hostname) || net.ParseIP(hostname) != nil {
		return "", fmt.Errorf("Cloudflare hostname must be one complete lowercase DNS hostname")
	}
	parsed, err := url.Parse("https://" + hostname + "/v1")
	if err != nil || parsed.Hostname() != hostname {
		return "", fmt.Errorf("Cloudflare hostname is invalid")
	}
	return parsed.String(), nil
}

func validateOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "8317" {
		return "", fmt.Errorf("Cloudflare Tunnel origin must be one private HTTP address on port 8317")
	}
	host := net.ParseIP(parsed.Hostname())
	if host == nil || (!host.IsLoopback() && !host.IsPrivate()) {
		return "", fmt.Errorf("Cloudflare Tunnel origin must be loopback or a private host address")
	}
	return parsed.String(), nil
}

// Reconcile creates one locally managed, outbound-only Cloudflare Tunnel. A
// broad account certificate exists only while the external Tunnel and DNS
// route are being reconciled. The retained credential can run only the exact
// Tunnel recorded in state.
func (t Tunnel) Reconcile(ctx context.Context, hostname string, stdout, stderr io.Writer) (State, error) {
	gatewayURL, err := GatewayURL(hostname)
	if err != nil {
		return State{}, err
	}
	hostname = strings.TrimSuffix(strings.TrimPrefix(gatewayURL, "https://"), "/v1")
	origin, err := validateOrigin(t.Origin)
	if err != nil {
		return State{}, err
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return State{}, fmt.Errorf("guided Cloudflare Tunnel setup supports only x86_64 Linux")
	}
	if strings.TrimSpace(t.StatePath) == "" {
		return State{}, fmt.Errorf("Cloudflare Tunnel state path is empty")
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
	if found && state.Hostname != hostname {
		return State{}, fmt.Errorf("Dorf already owns Cloudflare hostname %s; refusing to replace it", state.Hostname)
	}
	if found {
		// The Gateway moves from loopback to the private Incus bridge when a
		// local profile is added to a cloud-only deployment. The Tunnel identity
		// and public hostname remain stable; only its local origin changes.
		state.Origin = origin
	}
	if !found {
		nonce := make([]byte, 4)
		if _, err := rand.Read(nonce); err != nil {
			return State{}, err
		}
		state = State{SchemaVersion: 1, TunnelName: "dorf-" + hex.EncodeToString(nonce), Hostname: hostname, Origin: origin}
		if err := t.save(state); err != nil {
			return State{}, err
		}
	}
	binary, err := t.ensureBinary(ctx)
	if err != nil {
		return state, err
	}
	managementHome := filepath.Join(t.StatePath, "management")
	cloudflaredHome := filepath.Join(managementHome, ".cloudflared")
	certificate := filepath.Join(cloudflaredHome, "cert.pem")
	env := []string{"HOME=" + managementHome}
	if state.TunnelID == "" {
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
		matches, err := t.listByName(ctx, binary, env, certificate, state.TunnelName)
		if err != nil {
			return state, err
		}
		if len(matches) == 0 {
			if err := t.Runner.Run(ctx, env, stdout, stderr, binary, "tunnel", "--origincert", certificate, "create", state.TunnelName); err != nil {
				return state, fmt.Errorf("create Cloudflare Tunnel: %w", err)
			}
			matches, err = t.listByName(ctx, binary, env, certificate, state.TunnelName)
			if err != nil {
				return state, err
			}
		}
		if len(matches) != 1 || strings.TrimSpace(matches[0].ID) == "" {
			return state, fmt.Errorf("Cloudflare returned %d exact Tunnels named %s; refusing ambiguous ownership", len(matches), state.TunnelName)
		}
		state.TunnelID = matches[0].ID
		credentialSource := filepath.Join(cloudflaredHome, state.TunnelID+".json")
		credentialDir := filepath.Join(t.StatePath, "credentials")
		if err := os.MkdirAll(credentialDir, 0o700); err != nil {
			return state, err
		}
		state.CredentialPath = filepath.Join(credentialDir, state.TunnelID+".json")
		if _, err := os.Stat(state.CredentialPath); errors.Is(err, os.ErrNotExist) {
			if _, sourceErr := os.Stat(credentialSource); sourceErr == nil {
				if err := os.Rename(credentialSource, state.CredentialPath); err != nil {
					return state, fmt.Errorf("retain exact Cloudflare Tunnel credential: %w", err)
				}
			} else if errors.Is(sourceErr, os.ErrNotExist) {
				// A create response can be lost after Cloudflare commits the Tunnel
				// but before cloudflared writes its local credential. The temporary
				// account certificate can recover the exact named Tunnel without
				// creating a duplicate.
				if err := t.Runner.Run(ctx, env, stdout, stderr, binary, "tunnel", "--origincert", certificate, "token", "--cred-file", state.CredentialPath, state.TunnelID); err != nil {
					return state, fmt.Errorf("recover exact Cloudflare Tunnel credential: %w", err)
				}
			} else {
				return state, sourceErr
			}
		} else if err != nil {
			return state, err
		}
		if err := os.Chmod(state.CredentialPath, 0o600); err != nil {
			return state, err
		}
		if err := t.save(state); err != nil {
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
	if !state.DNSConfigured {
		if _, err := os.Stat(certificate); err != nil {
			return state, fmt.Errorf("repair Cloudflare DNS requires authorization again: %w", err)
		}
		if err := t.Runner.Run(ctx, env, stdout, stderr, binary, "tunnel", "--origincert", certificate, "route", "dns", "--overwrite-dns", state.TunnelID, state.Hostname); err != nil {
			return state, fmt.Errorf("route Cloudflare hostname: %w", err)
		}
		state.DNSConfigured = true
		if err := t.save(state); err != nil {
			return state, err
		}
	}
	servicePath := filepath.Join(t.StatePath, "dorf-cloudflared.service")
	if err := writePrivate(servicePath, []byte(t.service(binary, state.ConfigPath))); err != nil {
		return state, err
	}
	owned, err := t.serviceOwned(ctx, binary, state.ConfigPath)
	if err != nil {
		return state, err
	}
	if !owned {
		if err := t.runRoot(ctx, stdout, stderr, "install", "-m", "0644", servicePath, "/etc/systemd/system/dorf-cloudflared.service"); err != nil {
			return state, fmt.Errorf("install Cloudflare Tunnel service: %w", err)
		}
		if err := t.runRoot(ctx, stdout, stderr, "systemctl", "daemon-reload"); err != nil {
			return state, fmt.Errorf("reload Cloudflare Tunnel service: %w", err)
		}
	}
	if err := t.runRoot(ctx, stdout, stderr, "systemctl", "enable", "dorf-cloudflared.service"); err != nil {
		return state, fmt.Errorf("enable Cloudflare Tunnel service: %w", err)
	}
	// Restart on every reconciliation. This is intentionally stronger than
	// checking the unit alone: after process loss, the config file may already
	// contain the desired origin while the live process still has the old one.
	if err := t.runRoot(ctx, stdout, stderr, "systemctl", "restart", "dorf-cloudflared.service"); err != nil {
		return state, fmt.Errorf("start Cloudflare Tunnel service: %w", err)
	}
	state.ServiceInstalled, state.Complete = true, true
	if err := t.save(state); err != nil {
		return state, err
	}
	if err := os.RemoveAll(managementHome); err != nil {
		return state, fmt.Errorf("remove temporary Cloudflare account authority: %w", err)
	}
	return state, nil
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

func (t Tunnel) serviceOwned(ctx context.Context, binary, config string) (bool, error) {
	output, err := t.Runner.Output(ctx, nil, "systemctl", "cat", "dorf-cloudflared.service")
	if err != nil {
		return false, nil
	}
	if !strings.Contains(output, "Description=Dorf Cloudflare Tunnel") {
		return false, fmt.Errorf("dorf-cloudflared.service exists without Dorf ownership; refusing to replace it")
	}
	for _, exact := range []string{
		binary,
		config,
		fmt.Sprintf("User=%d", os.Getuid()),
		fmt.Sprintf("Group=%d", os.Getgid()),
		"NoNewPrivileges=true",
	} {
		if !strings.Contains(output, exact) {
			return false, nil
		}
	}
	return true, nil
}

func (t Tunnel) runRoot(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	var prefix []string
	if t.RootPrefix != nil {
		prefix = append([]string{}, t.RootPrefix...)
	} else if os.Geteuid() != 0 {
		prefix = []string{"sudo"}
	}
	if len(prefix) == 0 {
		return t.Runner.Run(ctx, nil, stdout, stderr, name, args...)
	}
	return t.Runner.Run(ctx, nil, stdout, stderr, prefix[0], append(append(prefix[1:], name), args...)...)
}

func (t Tunnel) config(state State) string {
	return fmt.Sprintf("tunnel: %q\ncredentials-file: %q\nno-autoupdate: true\ningress:\n  - hostname: %q\n    path: ^/v1(/.*)?$\n    service: %q\n  - service: http_status:404\n", state.TunnelID, state.CredentialPath, state.Hostname, state.Origin)
}

func (t Tunnel) service(binary, config string) string {
	return fmt.Sprintf(`[Unit]
Description=Dorf Cloudflare Tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=%d
Group=%d
NoNewPrivileges=true
ExecStart=%q --no-autoupdate --config %q tunnel run
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
`, os.Getuid(), os.Getgid(), binary, config)
}

func (t Tunnel) ensureBinary(ctx context.Context) (string, error) {
	if strings.TrimSpace(t.Binary) != "" {
		return t.Binary, nil
	}
	destination := filepath.Join(t.StatePath, "bin", BinaryVersion, "cloudflared")
	if raw, err := os.ReadFile(destination); err == nil {
		hash := sha256.Sum256(raw)
		if hex.EncodeToString(hash[:]) != binarySHA256 {
			return "", fmt.Errorf("installed cloudflared %s checksum mismatch", BinaryVersion)
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
	if hex.EncodeToString(hash[:]) != binarySHA256 {
		return "", fmt.Errorf("cloudflared %s checksum mismatch", BinaryVersion)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", err
	}
	if err := writePrivateMode(destination, raw, 0o700); err != nil {
		return "", err
	}
	return destination, nil
}

func (t Tunnel) statePath() string { return filepath.Join(t.StatePath, "state.json") }

func (t Tunnel) load() (State, bool, error) {
	raw, err := os.ReadFile(t.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
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
