package gateway

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	BackendVersion  = "7.2.104"
	backendArchive  = "CLIProxyAPI_7.2.104_linux_amd64.tar.gz"
	backendSHA256   = "993babb37b6de831600f0eb31527ca0f938337e1d1f837d5cf846263affa9724"
	defaultPort     = 8317
	openAIModelsURL = "https://api.openai.com/v1/models"
)

var connectionName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Provision installs and starts the pinned local broker. The broker is the one
// concrete non-Go service in Dorf's supported model path; it alone refreshes
// the owner's upstream ChatGPT credential.
func (g Gateway) Provision(ctx context.Context, bind string) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("the supported Provider Gateway binary is Linux x86_64 only")
	}
	bridgeAddresses := []net.Addr(nil)
	parsed := net.ParseIP(strings.TrimSpace(bind))
	if parsed != nil && parsed.To4() != nil && !parsed.IsLoopback() {
		if strings.TrimSpace(g.PrivateBridge) == "" {
			return fmt.Errorf("non-loopback provider bind requires the configured private Incus bridge")
		}
		bridge, err := net.InterfaceByName(g.PrivateBridge)
		if err != nil {
			return fmt.Errorf("inspect private Incus bridge %s: %w", g.PrivateBridge, err)
		}
		bridgeAddresses, err = bridge.Addrs()
		if err != nil {
			return fmt.Errorf("inspect private Incus bridge %s addresses: %w", g.PrivateBridge, err)
		}
	}
	normalizedBind, allowRemote, err := validateProviderBind(bind, bridgeAddresses)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(g.StatePath, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(g.StatePath, 0o700); err != nil {
		return err
	}
	return g.lock(func() error {
		if err := g.ensureAuthority(); err != nil {
			return err
		}
		if err := g.ensureStateFiles(); err != nil {
			return err
		}
		executable := g.backendExecutable()
		if _, err := os.Stat(executable); errors.Is(err, os.ErrNotExist) {
			if err := g.installBackend(ctx, executable); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		verified := exec.CommandContext(ctx, executable, "-h")
		help, err := verified.CombinedOutput()
		if err != nil || !strings.Contains(string(help), BackendVersion) {
			return fmt.Errorf("Provider Gateway executable is not pinned version %s", BackendVersion)
		}
		configChanged, err := g.writeBrokerConfig(normalizedBind, allowRemote)
		if err != nil {
			return err
		}
		if g.probeBroker(ctx) == nil {
			if !configChanged {
				return nil
			}
			return g.restartBroker(ctx, executable)
		}
		if pid := g.brokerPID(); pid > 1 {
			if g.processMatches(pid, executable) {
				if configChanged {
					return g.restartBroker(ctx, executable)
				}
				return fmt.Errorf("Provider Gateway process %d is running but unavailable; inspect %s", pid, filepath.Join(g.StatePath, "broker.log"))
			}
			_ = os.Remove(filepath.Join(g.StatePath, "broker.pid"))
		}
		return g.startBroker(ctx, executable)
	})
}

func validateProviderBind(bind string, bridgeAddresses []net.Addr) (string, bool, error) {
	parsed := net.ParseIP(strings.TrimSpace(bind))
	if parsed == nil || parsed.To4() == nil || parsed.IsUnspecified() || parsed.IsMulticast() || parsed.IsLinkLocalUnicast() {
		return "", false, fmt.Errorf("provider bind address must be loopback or the configured private Incus bridge IPv4")
	}
	parsed = parsed.To4()
	if parsed.IsLoopback() {
		return parsed.String(), false, nil
	}
	if !parsed.IsPrivate() {
		return "", false, fmt.Errorf("provider bind address must not expose the broker on a public interface")
	}
	for _, address := range bridgeAddresses {
		var candidate net.IP
		switch value := address.(type) {
		case *net.IPNet:
			candidate = value.IP
		case *net.IPAddr:
			candidate = value.IP
		}
		if candidate != nil && candidate.To4() != nil && candidate.Equal(parsed) {
			return parsed.String(), true, nil
		}
	}
	return "", false, fmt.Errorf("provider bind address is not assigned to the configured private Incus bridge")
}

func (g Gateway) ConnectChatGPT(ctx context.Context, name, bind string, authorize func(string, string)) error {
	name = strings.TrimSpace(name)
	if !connectionName.MatchString(name) {
		return fmt.Errorf("provider connection name must be 1-64 safe characters")
	}
	if err := g.Provision(ctx, bind); err != nil {
		return err
	}
	connections, err := g.connections()
	if err != nil {
		return err
	}
	for _, existing := range connections {
		if existing.Name != name && existing.Provider != "deepseek" {
			return fmt.Errorf("provider connection %q already owns the deployment's unprefixed OpenAI route; configure one upstream authentication mode at a time", existing.Name)
		}
		if existing.Name != name {
			continue
		}
		if existing.Provider != "chatgpt" || existing.AuthMode != "subscription" {
			return fmt.Errorf("provider connection %q already has another authentication mode", name)
		}
		if err := g.enableWebSockets(ctx, existing.CredentialRef); err != nil {
			return err
		}
		return g.Check(ctx, name)
	}
	before, err := g.authSnapshot()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, g.backendExecutable(), "-config", filepath.Join(g.StatePath, "broker.yaml"), "-codex-device-login", "-no-browser")
	cmd.Dir = g.StatePath
	cmd.Stdin = nil
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ChatGPT device login: %w", err)
	}
	scanner := bufio.NewScanner(pipe)
	var url, code string
	success := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Codex device URL: ") {
			url = strings.TrimSpace(strings.TrimPrefix(line, "Codex device URL: "))
		}
		if strings.HasPrefix(line, "Codex device code: ") {
			code = strings.TrimSpace(strings.TrimPrefix(line, "Codex device code: "))
		}
		if url != "" && code != "" {
			authorize(url, code)
			url, code = "", ""
		}
		if line == "Codex device authentication successful!" {
			success = true
		}
	}
	waitErr := cmd.Wait()
	if scanErr := scanner.Err(); scanErr != nil {
		return fmt.Errorf("read ChatGPT device login: %w", scanErr)
	}
	if waitErr != nil || !success {
		return fmt.Errorf("ChatGPT device authentication did not complete")
	}
	after, err := g.authSnapshot()
	if err != nil {
		return err
	}
	changed := []string{}
	for path, stamp := range after {
		if before[path] != stamp {
			changed = append(changed, filepath.Base(path))
		}
	}
	if len(changed) != 1 {
		return fmt.Errorf("ChatGPT login changed %d credential files; refusing ambiguous connection", len(changed))
	}
	sort.Strings(changed)
	err = g.lock(func() error {
		current, readErr := g.connections()
		if readErr != nil {
			return readErr
		}
		for _, existing := range current {
			if existing.Name == name {
				return fmt.Errorf("provider connection %q was created concurrently", name)
			}
		}
		current = append(current, connection{Name: name, Provider: "chatgpt", AuthMode: "subscription", CredentialRef: changed[0]})
		return writePrivateJSON(filepath.Join(g.StatePath, "connections.json"), current)
	})
	if err != nil {
		return err
	}
	if err := g.enableWebSockets(ctx, changed[0]); err != nil {
		return err
	}
	return g.Check(ctx, name)
}

// ConnectOpenAIAPIKey reconciles one named OpenAI API-key connection. The
// upstream key remains in the protected Gateway state and is never returned to
// callers or copied into a Sandbox. Replaying the same name and key is
// idempotent; a validated key rotation keeps the stable connection identity.
func (g Gateway) ConnectOpenAIAPIKey(ctx context.Context, name, bind, apiKey string) error {
	name = strings.TrimSpace(name)
	apiKey = strings.TrimSpace(apiKey)
	if !connectionName.MatchString(name) {
		return fmt.Errorf("provider connection name must be 1-64 safe characters")
	}
	if apiKey == "" || len(apiKey) > 16<<10 {
		return fmt.Errorf("OpenAI API key must be nonempty and at most 16 KiB")
	}
	if err := g.validateOpenAIAPIKey(ctx, apiKey); err != nil {
		return err
	}
	if err := g.Provision(ctx, bind); err != nil {
		return err
	}
	if err := g.lock(func() error {
		_, err := g.recordOpenAIAPIKey(name, apiKey)
		return err
	}); err != nil {
		return err
	}
	// Re-run ordinary provisioning even when the credential record already
	// existed. This closes the process-loss window between durable credential
	// custody and writing/restarting the broker configuration.
	if err := g.Provision(ctx, bind); err != nil {
		return err
	}
	return g.Check(ctx, name)
}

func (g Gateway) validateOpenAIAPIKey(ctx context.Context, apiKey string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openAIModelsURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := g.UpstreamClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("validate OpenAI API key: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("OpenAI API key readiness check returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (g Gateway) recordOpenAIAPIKey(name, apiKey string) (bool, error) {
	connections, err := g.connections()
	if err != nil {
		return false, err
	}
	for _, existing := range connections {
		if existing.Name != name && existing.Provider != "deepseek" {
			return false, fmt.Errorf("provider connection %q already owns the deployment's unprefixed OpenAI route; configure one upstream authentication mode at a time", existing.Name)
		}
		if existing.Name != name {
			continue
		}
		if existing.Provider != "openai" || existing.AuthMode != "api_key" {
			return false, fmt.Errorf("provider connection %q already has another authentication mode", name)
		}
		credentialPath := filepath.Join(g.StatePath, "credentials", existing.CredentialRef)
		secret, err := os.ReadFile(credentialPath)
		if err != nil {
			return false, fmt.Errorf("read Provider Connection %q credential: %w", name, err)
		}
		if strings.TrimSpace(string(secret)) == apiKey {
			return false, nil
		}
		if err := writePrivateFile(credentialPath, []byte(apiKey+"\n"), 0o600); err != nil {
			return false, err
		}
		return true, nil
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return false, err
	}
	credentialRef := "openai-" + hex.EncodeToString(random) + ".key"
	if err := writePrivateFile(filepath.Join(g.StatePath, "credentials", credentialRef), []byte(apiKey+"\n"), 0o600); err != nil {
		return false, err
	}
	connections = append(connections, connection{Name: name, Provider: "openai", AuthMode: "api_key", CredentialRef: credentialRef})
	if err := writePrivateJSON(filepath.Join(g.StatePath, "connections.json"), connections); err != nil {
		_ = os.Remove(filepath.Join(g.StatePath, "credentials", credentialRef))
		return false, err
	}
	return true, nil
}

func (g Gateway) ensureAuthority() error {
	path := filepath.Join(g.StatePath, "authority.json")
	if _, err := os.Stat(path); err == nil {
		_, readErr := g.readAuthority()
		return readErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	guard, err := randomKey()
	if err != nil {
		return err
	}
	controlRaw := make([]byte, 24)
	if _, err := rand.Read(controlRaw); err != nil {
		return err
	}
	return writePrivateJSON(path, authority{GuardKey: "agw_guard_" + strings.TrimPrefix(guard, "agw_"), ManagementKey: "agw_control_" + hex.EncodeToString(controlRaw)})
}

func (g Gateway) ensureStateFiles() error {
	for _, dir := range []string{"auth", "credentials"} {
		if err := os.MkdirAll(filepath.Join(g.StatePath, dir), 0o700); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		name  string
		value any
	}{{"connections.json", []connection{}}, {"routes.json", []Route{}}} {
		path := filepath.Join(g.StatePath, item.name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if err := writePrivateJSON(path, item.value); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (g Gateway) backendExecutable() string {
	return filepath.Join(g.StatePath, "bin", BackendVersion, "cli-proxy-api")
}

func (g Gateway) installBackend(ctx context.Context, destination string) error {
	url := "https://github.com/router-for-me/CLIProxyAPI/releases/download/v" + BackendVersion + "/" + backendArchive
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	resp, err := (&http.Client{Timeout: 60 * time.Second, Transport: transport}).Do(req)
	if err != nil {
		return fmt.Errorf("download Provider Gateway %s: %w", BackendVersion, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download Provider Gateway %s: HTTP %d", BackendVersion, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(g.StatePath, ".broker-*.tar.gz")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, 256<<20)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != backendSHA256 {
		return fmt.Errorf("Provider Gateway release checksum mismatch")
	}
	archive, err := os.Open(tmpPath)
	if err != nil {
		return err
	}
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var contents []byte
	for {
		header, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nextErr
		}
		if header.Name == "cli-proxy-api" && header.Typeflag == tar.TypeReg {
			contents, err = io.ReadAll(io.LimitReader(tr, 128<<20))
			if err != nil {
				return err
			}
			break
		}
	}
	if len(contents) == 0 {
		return fmt.Errorf("Provider Gateway release omitted cli-proxy-api")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	return writePrivateFile(destination, contents, 0o700)
}

func (g Gateway) connections() ([]connection, error) {
	var records []connection
	if err := readJSON(filepath.Join(g.StatePath, "connections.json"), &records); err != nil {
		return nil, err
	}
	return records, nil
}

func (g Gateway) writeBrokerConfig(bind string, allowRemote bool) (bool, error) {
	auth, err := g.readAuthority()
	if err != nil {
		return false, err
	}
	routes, err := g.readRoutes()
	if err != nil {
		return false, err
	}
	connections, err := g.connections()
	if err != nil {
		return false, err
	}
	lines := []string{fmt.Sprintf("host: %q", bind), fmt.Sprintf("port: %d", defaultPort), fmt.Sprintf("auth-dir: %q", filepath.Join(g.StatePath, "auth")), "force-model-prefix: true", "api-keys:", fmt.Sprintf("  - %q", auth.GuardKey)}
	for _, route := range routes {
		lines = append(lines, fmt.Sprintf("  - %q", route.APIKey))
	}
	lines = append(lines, "remote-management:", fmt.Sprintf("  allow-remote: %t", allowRemote), fmt.Sprintf("  secret-key: %q", auth.ManagementKey), "  disable-control-panel: true", "debug: false", "logging-to-file: false", "usage-statistics-enabled: false")
	for _, record := range connections {
		if record.Provider != "openai" || record.AuthMode != "api_key" {
			continue
		}
		secret, err := os.ReadFile(filepath.Join(g.StatePath, "credentials", record.CredentialRef))
		if err != nil {
			return false, err
		}
		lines = append(lines, "codex-api-key:", fmt.Sprintf("  - api-key: %q", strings.TrimSpace(string(secret))), "    base-url: \"https://api.openai.com/v1\"")
	}
	path := filepath.Join(g.StatePath, "broker.yaml")
	raw := []byte(strings.Join(lines, "\n") + "\n")
	if current, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(current, raw) {
		return false, nil
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return false, readErr
	}
	return true, writePrivateFile(path, raw, 0o600)
}

func (g Gateway) startBroker(ctx context.Context, executable string) error {
	logFile, err := os.OpenFile(filepath.Join(g.StatePath, "broker.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "-config", filepath.Join(g.StatePath, "broker.yaml"), "-local-model")
	cmd.Dir = g.StatePath
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start Provider Gateway: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	_ = logFile.Close()
	if err := writePrivateFile(filepath.Join(g.StatePath, "broker.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := g.probeBroker(ctx); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("Provider Gateway did not become ready; inspect %s", filepath.Join(g.StatePath, "broker.log"))
}

func (g Gateway) restartBroker(ctx context.Context, executable string) error {
	pid := g.brokerPID()
	if pid > 1 && g.processMatches(pid, executable) {
		process, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("stop Provider Gateway process %d: %w", pid, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && g.processMatches(pid, executable) {
			time.Sleep(50 * time.Millisecond)
		}
		if g.processMatches(pid, executable) {
			return fmt.Errorf("Provider Gateway process %d did not stop", pid)
		}
	}
	_ = os.Remove(filepath.Join(g.StatePath, "broker.pid"))
	return g.startBroker(ctx, executable)
}

func (g Gateway) probeBroker(ctx context.Context) error {
	origin, err := g.origin()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/", nil)
	if err != nil {
		return err
	}
	response, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

func (g Gateway) brokerPID() int {
	raw, err := os.ReadFile(filepath.Join(g.StatePath, "broker.pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}
func (g Gateway) processMatches(pid int, executable string) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), executable) && strings.Contains(string(raw), filepath.Join(g.StatePath, "broker.yaml"))
}

func (g Gateway) authSnapshot() (map[string]int64, error) {
	result := map[string]int64{}
	entries, err := os.ReadDir(filepath.Join(g.StatePath, "auth"))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && regexp.MustCompile(`^codex-[^/]+\.json$`).MatchString(entry.Name()) {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			result[filepath.Join(g.StatePath, "auth", entry.Name())] = info.ModTime().UnixNano()
		}
	}
	return result, nil
}

func (g Gateway) enableWebSockets(ctx context.Context, credential string) error {
	auth, err := g.readAuthority()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"name": credential, "websockets": true})
	origin, err := g.origin()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, origin+"/v0/management/auth-files/fields", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+auth.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("enable Provider Gateway WebSockets: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Provider Gateway rejected WebSocket capability with HTTP %d", response.StatusCode)
	}
	return nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, append(data, '\n'), 0o600)
}
func writePrivateFile(path string, data []byte, mode os.FileMode) error {
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
	if _, err := tmp.Write(data); err != nil {
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
