package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	created        bool
	loseCredential bool
	id             string
	serviceUnit    string
	calls          []string
}

func (f *fakeRunner) Run(_ context.Context, env []string, _, _ io.Writer, name string, args ...string) error {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	home := envValue(env, "HOME")
	switch {
	case strings.HasSuffix(call, "tunnel login"):
		path := filepath.Join(home, ".cloudflared", "cert.pem")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("temporary broad authority"), 0o600)
	case strings.Contains(call, " create "):
		f.created = true
		if f.loseCredential {
			return nil
		}
		path := filepath.Join(home, ".cloudflared", f.id+".json")
		return os.WriteFile(path, []byte(`{"TunnelID":"`+f.id+`"}`), 0o600)
	case strings.Contains(call, " token ") && strings.Contains(call, " --cred-file "):
		parts := strings.Fields(call)
		for index, part := range parts {
			if part == "--cred-file" && index+1 < len(parts) {
				return os.WriteFile(parts[index+1], []byte(`{"TunnelID":"`+f.id+`"}`), 0o600)
			}
		}
		return fmt.Errorf("token command omitted --cred-file: %q", call)
	case strings.Contains(call, "install -m 0644"):
		parts := strings.Fields(call)
		if len(parts) < 3 {
			return fmt.Errorf("invalid install call %q", call)
		}
		raw, err := os.ReadFile(parts[len(parts)-2])
		f.serviceUnit = string(raw)
		return err
	default:
		return nil
	}
}

func (f *fakeRunner) Output(_ context.Context, _ []string, name string, args ...string) (string, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if strings.Contains(call, "tunnel") && strings.Contains(call, " list ") {
		if !f.created {
			return "[]", nil
		}
		raw, _ := json.Marshal([]listedTunnel{{ID: f.id, Name: tunnelNameFromCalls(f.calls)}})
		return string(raw), nil
	}
	if strings.Contains(call, "systemctl cat dorf-cloudflared.service") {
		if f.serviceUnit != "" {
			return f.serviceUnit, nil
		}
		return "", fmt.Errorf("not installed")
	}
	return "", nil
}

func TestGuidedTunnelReconciliationRetainsOnlyExactRunAuthority(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "cloudflared")
	if err := os.WriteFile(binary, []byte("fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{id: "11111111-2222-3333-4444-555555555555"}
	tunnel := Tunnel{StatePath: filepath.Join(root, "state"), Binary: binary, Origin: "http://127.0.0.1:8317", Runner: runner, RootPrefix: []string{}}
	state, err := tunnel.Reconcile(context.Background(), "dorf.example.com", io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Complete || !state.DNSConfigured || !state.ServiceInstalled || state.TunnelID != runner.id || !probeIDPattern.MatchString(state.ProbeID) {
		t.Fatalf("state=%#v", state)
	}
	if _, err := os.Stat(filepath.Join(tunnel.StatePath, "management")); !os.IsNotExist(err) {
		t.Fatalf("temporary account authority remains: %v", err)
	}
	credential, err := os.Stat(state.CredentialPath)
	if err != nil || credential.Mode().Perm() != 0o600 {
		t.Fatalf("credential=%v err=%v", credential, err)
	}
	config, err := os.ReadFile(state.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{`hostname: "dorf.example.com"`, "path: ^/\\.dorf/probe/" + state.ProbeID + "$", "service: http_status:204", "path: ^/v1(/.*)?$", `service: "http://127.0.0.1:8317"`, "service: http_status:404"} {
		if !strings.Contains(string(config), wanted) {
			t.Fatalf("config omitted %q:\n%s", wanted, config)
		}
	}
	for _, forbidden := range []string{"temporary broad authority", "cert.pem"} {
		if strings.Contains(string(config), forbidden) {
			t.Fatalf("config retained %q", forbidden)
		}
	}
	if !strings.Contains(runner.serviceUnit, fmt.Sprintf("User=%d", os.Getuid())) || !strings.Contains(runner.serviceUnit, "NoNewPrivileges=true") {
		t.Fatalf("service would execute user-owned tunnel state as root:\n%s", runner.serviceUnit)
	}
	if countCall(runner.calls, "tunnel login") != 1 || countCall(runner.calls, " create ") != 1 || countCall(runner.calls, " route dns ") != 1 {
		t.Fatalf("calls=%v", runner.calls)
	}
	if _, err := tunnel.Reconcile(context.Background(), "dorf.example.com", io.Discard, io.Discard); err != nil {
		t.Fatalf("idempotent reconciliation: %v", err)
	}
	if countCall(runner.calls, "tunnel login") != 1 || countCall(runner.calls, " create ") != 1 || countCall(runner.calls, " route dns ") != 1 || countCall(runner.calls, "install -m 0644") != 1 {
		t.Fatalf("replay repeated external creation: %v", runner.calls)
	}
	runner.serviceUnit = ""
	if _, err := tunnel.Reconcile(context.Background(), "dorf.example.com", io.Discard, io.Discard); err != nil {
		t.Fatalf("recover absent service: %v", err)
	}
	if countCall(runner.calls, "install -m 0644") != 2 {
		t.Fatalf("absent service was not restored: %v", runner.calls)
	}
	tunnel.Origin = "http://10.44.0.1:8317"
	updated, err := tunnel.Reconcile(context.Background(), "dorf.example.com", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("move Tunnel to private Incus origin: %v", err)
	}
	if updated.Origin != tunnel.Origin {
		t.Fatalf("origin=%q, want %q", updated.Origin, tunnel.Origin)
	}
	config, err = os.ReadFile(updated.ConfigPath)
	if err != nil || !strings.Contains(string(config), `service: "http://10.44.0.1:8317"`) {
		t.Fatalf("updated config=%s err=%v", config, err)
	}
	if countCall(runner.calls, "tunnel login") != 1 || countCall(runner.calls, " create ") != 1 || countCall(runner.calls, " route dns ") != 1 {
		t.Fatalf("origin update recreated Cloudflare authority: %v", runner.calls)
	}
	if countCall(runner.calls, "systemctl restart dorf-cloudflared.service") != 4 {
		t.Fatalf("each reconciliation must reload exact config: %v", runner.calls)
	}
}

func TestGuidedTunnelRecoversCredentialAfterAmbiguousCreate(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "cloudflared")
	if err := os.WriteFile(binary, []byte("fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{id: "11111111-2222-3333-4444-555555555555", loseCredential: true}
	tunnel := Tunnel{StatePath: filepath.Join(root, "state"), Binary: binary, Origin: "http://127.0.0.1:8317", Runner: runner, RootPrefix: []string{}}
	state, err := tunnel.Reconcile(context.Background(), "dorf.example.com", io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state.CredentialPath); err != nil {
		t.Fatalf("recovered credential: %v", err)
	}
	if countCall(runner.calls, " create ") != 1 || countCall(runner.calls, " token ") != 1 {
		t.Fatalf("calls=%v", runner.calls)
	}
}

func TestCloudflareHostnameContract(t *testing.T) {
	for _, invalid := range []string{"", "https://dorf.example.com", "dorf", "127.0.0.1", "dorf.example.com/path"} {
		if _, err := GatewayURL(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
	got, err := GatewayURL("dorf.example.com")
	if err != nil || got != "https://dorf.example.com/v1" {
		t.Fatalf("url=%q err=%v", got, err)
	}
}

func TestCloudflareDeploymentProbeURLRequiresExactStateIdentity(t *testing.T) {
	state := State{Hostname: "dorf.example.com", ProbeID: strings.Repeat("a", 32)}
	if got, err := state.ProbeURL(); err != nil || got != "https://dorf.example.com/.dorf/probe/"+state.ProbeID {
		t.Fatalf("probe URL=%q err=%v", got, err)
	}
	state.ProbeID = ""
	if _, err := state.ProbeURL(); err == nil || !strings.Contains(err.Error(), "rerun dorf setup") {
		t.Fatalf("missing identity error=%v", err)
	}
}

func TestCloudflareManagementHomeReplacesTheCallerHome(t *testing.T) {
	merged := mergeEnv([]string{"HOME=/home/operator", "PATH=/usr/bin"}, []string{"HOME=/private/management"})
	if got := envValue(merged, "HOME"); got != "/private/management" {
		t.Fatalf("HOME=%q env=%v", got, merged)
	}
	if countCall(merged, "HOME=") != 1 {
		t.Fatalf("duplicate HOME could leak the account certificate: %v", merged)
	}
}

func TestExecRunnerKeepsDiagnosticStderrOutOfMachineReadableOutput(t *testing.T) {
	output, err := (ExecRunner{}).Output(context.Background(), nil, "sh", "-c", `printf '[{"id":"one"}]'; printf '{"level":"info"}' >&2`)
	if err != nil {
		t.Fatal(err)
	}
	if output != `[{"id":"one"}]` {
		t.Fatalf("output=%q", output)
	}
	_, err = (ExecRunner{}).Output(context.Background(), nil, "sh", "-c", `printf 'useful failure' >&2; exit 7`)
	if err == nil || !strings.Contains(err.Error(), "useful failure") {
		t.Fatalf("error=%v", err)
	}
}

func TestGuidedTunnelRefusesForeignServiceUnit(t *testing.T) {
	runner := &fakeRunner{serviceUnit: "[Unit]\nDescription=Operator service\n"}
	_, err := (Tunnel{Runner: runner}).serviceOwned(context.Background(), "/owned/cloudflared", "/owned/config.yml")
	if err == nil || !strings.Contains(err.Error(), "without Dorf ownership") {
		t.Fatalf("foreign service error=%v", err)
	}
}

func envValue(env []string, key string) string {
	for _, value := range env {
		if strings.HasPrefix(value, key+"=") {
			return strings.TrimPrefix(value, key+"=")
		}
	}
	return ""
}

func tunnelNameFromCalls(calls []string) string {
	for _, call := range calls {
		parts := strings.Fields(call)
		for index, part := range parts {
			if part == "create" && index+1 < len(parts) {
				return parts[index+1]
			}
		}
	}
	return ""
}

func countCall(calls []string, fragment string) int {
	count := 0
	for _, call := range calls {
		if strings.Contains(call, fragment) {
			count++
		}
	}
	return count
}
