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
	"strings"
	"syscall"
	"time"
)

const (
	BackendVersion          = "7.2.104"
	backendArchive          = "CLIProxyAPI_7.2.104_linux_amd64.tar.gz"
	backendArchiveSHA256    = "993babb37b6de831600f0eb31527ca0f938337e1d1f837d5cf846263affa9724"
	backendExecutableSHA256 = "6355d7424394f22293f9d9c8cb3b9ca0073734dc50e8b740bb2af5cea98aaf64"
	defaultPort             = 8317
	openAIModelsURL         = "https://api.openai.com/v1/models"
)

var connectionName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

type preparedBroker struct {
	executable    string
	configChanged bool
}

type ComposeState struct {
	StatePath      string
	Digest         string
	PublishAddress string
}

type composeLaunchState struct {
	PublishAddress string `json:"publish_address"`
}

type brokerLaunchState struct {
	SchemaVersion      int    `json:"schema_version"`
	BackendSHA256      string `json:"backend_sha256"`
	BrokerConfigSHA256 string `json:"broker_config_sha256"`
}

// ComposeState returns the exact prepared runtime authority when at least one
// AI connection exists. Empty preparation is not a desired service.
func (g Gateway) ComposeState() (ComposeState, bool, error) {
	statePath := filepath.Clean(strings.TrimSpace(g.StatePath))
	if !filepath.IsAbs(statePath) || statePath == "/" || statePath != g.StatePath {
		return ComposeState{}, false, fmt.Errorf("Provider Gateway state path must be one clean absolute path")
	}
	if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) {
		return ComposeState{}, false, nil
	} else if err != nil {
		return ComposeState{}, false, err
	}
	connections, err := g.connections()
	if errors.Is(err, os.ErrNotExist) {
		return ComposeState{}, false, nil
	}
	if err != nil {
		return ComposeState{}, false, err
	}
	if len(connections) == 0 {
		return ComposeState{}, false, nil
	}
	publishAddress, found, err := g.PreparedComposePublishAddress()
	if err != nil {
		return ComposeState{}, false, err
	}
	if !found {
		return ComposeState{}, false, nil
	}
	if err := g.attestPreparedBroker(); err != nil {
		return ComposeState{}, false, err
	}
	// Hash only stable launch inputs. Subscription auth files and route state
	// are intentionally live-managed by the broker; hashing them would make a
	// successful WebSocket enable or route mutation immediately look drifted.
	paths := []string{"broker.yaml", "compose.json", "launch.json", filepath.ToSlash(filepath.Join("bin", BackendVersion, "cli-proxy-api"))}
	for _, connection := range connections {
		if filepath.Base(connection.CredentialRef) != connection.CredentialRef || connection.CredentialRef == "." || connection.CredentialRef == "" {
			return ComposeState{}, false, fmt.Errorf("AI connection %q credential path is invalid", connection.Name)
		}
		switch connection.AuthMode {
		case "subscription", "api_key":
		default:
			return ComposeState{}, false, fmt.Errorf("AI connection %q authentication mode is invalid", connection.Name)
		}
	}
	sort.Strings(paths)
	digest := sha256.New()
	for _, relative := range paths {
		path := filepath.Join(statePath, filepath.FromSlash(relative))
		contents, err := readProtectedRuntimeFile(path, 256<<20)
		if err != nil {
			return ComposeState{}, false, fmt.Errorf("attest Provider Gateway runtime file %s: %w", relative, err)
		}
		fmt.Fprintf(digest, "%s\x00%d\x00", relative, len(contents))
		_, _ = digest.Write(contents)
	}
	return ComposeState{StatePath: statePath, Digest: hex.EncodeToString(digest.Sum(nil)), PublishAddress: publishAddress}, true, nil
}

// PreparedComposePublishAddress returns the retained host-side Gateway
// publication authority without requiring a configured AI connection or the
// current version's backend. Approved update reconciliation uses it to prepare
// the new pinned backend before deriving ComposeState.
func (g Gateway) PreparedComposePublishAddress() (string, bool, error) {
	statePath := filepath.Clean(strings.TrimSpace(g.StatePath))
	if !filepath.IsAbs(statePath) || statePath == "/" || statePath != g.StatePath {
		return "", false, fmt.Errorf("Provider Gateway state path must be one clean absolute path")
	}
	raw, err := readProtectedRuntimeFile(filepath.Join(statePath, "compose.json"), 16<<10)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("attest Provider Gateway Compose launch state: %w", err)
	}
	var launch composeLaunchState
	if err := json.Unmarshal(raw, &launch); err != nil {
		return "", false, fmt.Errorf("decode Provider Gateway Compose launch state: %w", err)
	}
	publishAddress, err := validateComposePublishAddress(launch.PublishAddress)
	if err != nil {
		return "", false, err
	}
	return publishAddress, true, nil
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

// Prepare installs and verifies the pinned broker and reconciles its protected
// state and broker.yaml. It deliberately does not start the broker.
func (g Gateway) Prepare(ctx context.Context, bind string) error {
	normalizedBind, allowRemote, err := g.prepareBind(bind)
	if err != nil {
		return err
	}
	return g.prepare(ctx, normalizedBind, allowRemote)
}

// PrepareContainer prepares the broker for Dorf's Compose bridge. The fixed
// wildcard is safe only behind that container boundary, so ordinary Prepare
// continues to reject it.
func (g Gateway) PrepareContainer(ctx context.Context, publishAddress string) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("the supported Provider Gateway binary is Linux x86_64 only")
	}
	publishAddress, err := validateComposePublishAddress(publishAddress)
	if err != nil {
		return err
	}
	if err := g.prepare(ctx, "0.0.0.0", true); err != nil {
		return err
	}
	return writePrivateJSON(filepath.Join(g.StatePath, "compose.json"), composeLaunchState{PublishAddress: publishAddress})
}

func validateComposePublishAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed := net.ParseIP(value)
	if parsed == nil || parsed.To4() == nil || parsed.IsUnspecified() || (!parsed.IsLoopback() && !parsed.IsPrivate() && !sharedIPv4(parsed)) {
		return "", fmt.Errorf("Provider Gateway Compose publish address must be one loopback or private IPv4 address")
	}
	return parsed.To4().String(), nil
}

func sharedIPv4(ip net.IP) bool {
	value := ip.To4()
	return value != nil && value[0] == 100 && value[1]&0xc0 == 0x40
}

func (g Gateway) prepare(ctx context.Context, bind string, allowRemote bool) error {
	if err := g.ensureStateDirectory(); err != nil {
		return err
	}
	return g.lock(func() error {
		_, err := g.prepareBroker(ctx, bind, allowRemote)
		return err
	})
}

// RunForeground runs an already prepared broker attached to the caller. It
// does not daemonize, rewrite broker.yaml, or install a backend. Canceling ctx
// forwards SIGTERM and waits for the broker before returning.
func (g Gateway) RunForeground(ctx context.Context, stdout, stderr io.Writer) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("the supported Provider Gateway binary is Linux x86_64 only")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.ensureStateDirectory(); err != nil {
		return err
	}
	executable, err := g.requirePreparedBroker()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "-config", filepath.Join(g.StatePath, "broker.yaml"), "-local-model")
	command.Dir = g.StatePath
	command.Stdin = nil
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Provider Gateway foreground process: %w", err)
	}
	return waitForeground(ctx, command)
}

func (g Gateway) prepareBind(bind string) (string, bool, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", false, fmt.Errorf("the supported Provider Gateway binary is Linux x86_64 only")
	}
	bridgeAddresses := []net.Addr(nil)
	parsed := net.ParseIP(strings.TrimSpace(bind))
	if parsed != nil && parsed.To4() != nil && !parsed.IsLoopback() {
		if strings.TrimSpace(g.PrivateBridge) == "" {
			return "", false, fmt.Errorf("non-loopback provider bind requires the configured private Incus bridge")
		}
		bridge, err := net.InterfaceByName(g.PrivateBridge)
		if err != nil {
			return "", false, fmt.Errorf("inspect private Incus bridge %s: %w", g.PrivateBridge, err)
		}
		bridgeAddresses, err = bridge.Addrs()
		if err != nil {
			return "", false, fmt.Errorf("inspect private Incus bridge %s addresses: %w", g.PrivateBridge, err)
		}
	}
	return validateProviderBind(bind, bridgeAddresses)
}

func (g Gateway) ensureStateDirectory() error {
	if err := os.MkdirAll(g.StatePath, 0o700); err != nil {
		return err
	}
	return os.Chmod(g.StatePath, 0o700)
}

func (g Gateway) prepareBroker(ctx context.Context, bind string, allowRemote bool) (preparedBroker, error) {
	if err := g.ensureAuthority(); err != nil {
		return preparedBroker{}, err
	}
	if err := g.ensureStateFiles(); err != nil {
		return preparedBroker{}, err
	}
	executable := g.backendExecutable()
	if _, err := os.Stat(executable); errors.Is(err, os.ErrNotExist) {
		if err := g.installBackend(ctx, executable); err != nil {
			return preparedBroker{}, err
		}
	} else if err != nil {
		return preparedBroker{}, err
	}
	if err := g.verifyBackend(ctx, executable); err != nil {
		return preparedBroker{}, err
	}
	configChanged, err := g.writeBrokerConfig(bind, allowRemote)
	if err != nil {
		return preparedBroker{}, err
	}
	if err := g.writeBrokerLaunchState(executable); err != nil {
		return preparedBroker{}, err
	}
	return preparedBroker{executable: executable, configChanged: configChanged}, nil
}

func (g Gateway) verifyBackend(ctx context.Context, executable string) error {
	if err := g.verifyBackendDigest(executable); err != nil {
		return err
	}
	verified := exec.CommandContext(ctx, executable, "-h")
	help, err := verified.CombinedOutput()
	if err != nil || !strings.Contains(string(help), BackendVersion) {
		return fmt.Errorf("Provider Gateway executable is not pinned version %s", BackendVersion)
	}
	return nil
}

func (g Gateway) requirePreparedBroker() (string, error) {
	executable := g.backendExecutable()
	if err := g.attestPreparedBroker(); err != nil {
		return "", err
	}
	return executable, nil
}

func (g Gateway) writeBrokerLaunchState(executable string) error {
	if err := g.verifyBackendDigest(executable); err != nil {
		return err
	}
	config, err := readProtectedRuntimeFile(filepath.Join(g.StatePath, "broker.yaml"), 1<<20)
	if err != nil {
		return fmt.Errorf("attest Provider Gateway broker.yaml: %w", err)
	}
	digest := sha256.Sum256(config)
	return writePrivateJSON(filepath.Join(g.StatePath, "launch.json"), brokerLaunchState{
		SchemaVersion:      1,
		BackendSHA256:      g.expectedBackendSHA256(),
		BrokerConfigSHA256: hex.EncodeToString(digest[:]),
	})
}

func (g Gateway) attestPreparedBroker() error {
	raw, err := readProtectedRuntimeFile(filepath.Join(g.StatePath, "launch.json"), 16<<10)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Provider Gateway launch authority is not prepared")
	}
	if err != nil {
		return fmt.Errorf("attest Provider Gateway launch authority: %w", err)
	}
	var launch brokerLaunchState
	if err := json.Unmarshal(raw, &launch); err != nil {
		return fmt.Errorf("decode Provider Gateway launch authority: %w", err)
	}
	if launch.SchemaVersion != 1 || launch.BackendSHA256 != g.expectedBackendSHA256() || len(launch.BrokerConfigSHA256) != sha256.Size*2 {
		return fmt.Errorf("Provider Gateway launch authority is invalid")
	}
	if err := g.verifyBackendDigest(g.backendExecutable()); err != nil {
		return err
	}
	config, err := readProtectedRuntimeFile(filepath.Join(g.StatePath, "broker.yaml"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Provider Gateway broker.yaml is not prepared")
	}
	if err != nil {
		return fmt.Errorf("attest Provider Gateway broker.yaml: %w", err)
	}
	digest := sha256.Sum256(config)
	if hex.EncodeToString(digest[:]) != launch.BrokerConfigSHA256 {
		return fmt.Errorf("Provider Gateway broker.yaml checksum mismatch; rerun dorf service reconcile")
	}
	return nil
}

func (g Gateway) verifyBackendDigest(executable string) error {
	info, err := os.Lstat(executable)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Provider Gateway backend is not prepared")
	}
	if err != nil {
		return fmt.Errorf("attest Provider Gateway executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o100 == 0 {
		return fmt.Errorf("attest Provider Gateway executable: must be one protected owner-executable regular file")
	}
	raw, err := readProtectedRuntimeFile(executable, 128<<20)
	if err != nil {
		return fmt.Errorf("attest Provider Gateway executable: %w", err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != g.expectedBackendSHA256() {
		return fmt.Errorf("Provider Gateway executable checksum mismatch; rerun dorf service reconcile")
	}
	return nil
}

func (g Gateway) expectedBackendSHA256() string {
	if strings.TrimSpace(g.backendSHA256) != "" {
		return strings.ToLower(strings.TrimSpace(g.backendSHA256))
	}
	return backendExecutableSHA256
}

func waitForeground(ctx context.Context, command *exec.Cmd) error {
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			return fmt.Errorf("Provider Gateway foreground process exited: %w", err)
		}
		return nil
	case <-ctx.Done():
		if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			_ = command.Process.Kill()
			<-waited
			return errors.Join(ctx.Err(), fmt.Errorf("stop Provider Gateway foreground process: %w", err))
		}
	}

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-waited:
		return ctx.Err()
	case <-timer.C:
		killErr := command.Process.Kill()
		<-waited
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return errors.Join(ctx.Err(), fmt.Errorf("kill Provider Gateway foreground process: %w", killErr))
		}
		return ctx.Err()
	}
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

func (g Gateway) ConnectChatGPT(ctx context.Context, name, publishAddress string, authorize func(string, string)) error {
	name = strings.TrimSpace(name)
	if !connectionName.MatchString(name) {
		return fmt.Errorf("AI connection name must be 1-64 safe characters")
	}
	if err := g.PrepareContainer(ctx, publishAddress); err != nil {
		return err
	}
	connections, err := g.connections()
	if err != nil {
		return err
	}
	for _, existing := range connections {
		if existing.Name != name && existing.Provider != "deepseek" {
			return fmt.Errorf("AI connection %q already owns the deployment's unprefixed OpenAI route; configure one upstream authentication mode at a time", existing.Name)
		}
		if existing.Name != name {
			continue
		}
		if existing.Provider != "chatgpt" || existing.AuthMode != "subscription" {
			return fmt.Errorf("AI connection %q already has another authentication mode", name)
		}
		return nil
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
				return fmt.Errorf("AI connection %q was created concurrently", name)
			}
		}
		current = append(current, connection{Name: name, Provider: "chatgpt", AuthMode: "subscription", CredentialRef: changed[0]})
		return writePrivateJSON(filepath.Join(g.StatePath, "connections.json"), current)
	})
	if err != nil {
		return err
	}
	return g.PrepareContainer(ctx, publishAddress)
}

// ConnectOpenAIAPIKey reconciles one named OpenAI API-key connection. The
// upstream key remains in the protected Gateway state and is never returned to
// callers or copied into a Sandbox. Replaying the same name and key is
// idempotent; changing the key for an existing name is rejected before any
// retained state is prepared.
func (g Gateway) ConnectOpenAIAPIKey(ctx context.Context, name, publishAddress string, apiKey string) error {
	name = strings.TrimSpace(name)
	apiKey = strings.TrimSpace(apiKey)
	if !connectionName.MatchString(name) {
		return fmt.Errorf("AI connection name must be 1-64 safe characters")
	}
	if apiKey == "" || len(apiKey) > 16<<10 {
		return fmt.Errorf("OpenAI API key must be nonempty and at most 16 KiB")
	}
	if _, _, err := g.inspectOpenAIAPIKey(name, apiKey); err != nil {
		return err
	}
	if err := g.validateOpenAIAPIKey(ctx, apiKey); err != nil {
		return err
	}
	if err := g.PrepareContainer(ctx, publishAddress); err != nil {
		return err
	}
	if err := g.lock(func() error {
		_, err := g.recordOpenAIAPIKey(name, apiKey)
		return err
	}); err != nil {
		return err
	}
	// Re-render broker.yaml after retaining or replaying the credential. Compose
	// observes the resulting digest and owns process recreation.
	return g.PrepareContainer(ctx, publishAddress)
}

// FinalizeConnection performs the live checks that require the Compose-owned
// Gateway process. Setup calls it only after reconciling the project.
func (g Gateway) FinalizeConnection(ctx context.Context, name string) error {
	record, err := g.requireConnection(strings.TrimSpace(name))
	if err != nil {
		return err
	}
	if record.AuthMode == "subscription" {
		if err := g.enableWebSockets(ctx, record.CredentialRef); err != nil {
			return err
		}
	}
	return g.Check(ctx, record.Name)
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
	connections, replayed, err := g.inspectOpenAIAPIKey(name, apiKey)
	if err != nil {
		return false, err
	}
	if replayed {
		return false, nil
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

func (g Gateway) inspectOpenAIAPIKey(name, apiKey string) ([]connection, bool, error) {
	connections, err := g.connections()
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for _, existing := range connections {
		if existing.Name != name && existing.Provider != "deepseek" {
			return nil, false, fmt.Errorf("AI connection %q already owns the deployment's unprefixed OpenAI route; configure one upstream authentication mode at a time", existing.Name)
		}
		if existing.Name != name {
			continue
		}
		if existing.Provider != "openai" || existing.AuthMode != "api_key" {
			return nil, false, fmt.Errorf("AI connection %q already has another authentication mode", name)
		}
		credentialPath := filepath.Join(g.StatePath, "credentials", existing.CredentialRef)
		secret, err := os.ReadFile(credentialPath)
		if err != nil {
			return nil, false, fmt.Errorf("read AI connection %q credential: %w", name, err)
		}
		if strings.TrimSpace(string(secret)) == apiKey {
			return connections, true, nil
		}
		return nil, false, fmt.Errorf("AI connection %q already retains a different OpenAI API key; key rotation is not supported", name)
	}
	return connections, false, nil
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
	if hex.EncodeToString(hash.Sum(nil)) != backendArchiveSHA256 {
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
	origin, err := g.internalDialOrigin()
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
