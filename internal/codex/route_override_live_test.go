package codex

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This opt-in proof runs the production launch script with stock Codex and
// isolated native state. It does not call a model or use operator credentials.
func TestLiveCodexRouteLaunchOverrides(t *testing.T) {
	if os.Getenv("DORF_CODEX_ROUTE_OVERRIDE_LIVE") != "1" {
		t.Skip("set DORF_CODEX_ROUTE_OVERRIDE_LIVE=1 to run an isolated local Codex app-server")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(executable, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Log(strings.TrimSpace(string(version)))
	home := t.TempDir()
	nativeHome := filepath.Join(home, ".codex")
	if err := os.Mkdir(nativeHome, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(nativeHome, "config.toml")
	// Deliberately include a conflicting client provider selection: this experiment
	// must establish override precedence, not just the empty-config happy path.
	client := clientRouteConfig
	if err := os.WriteFile(configPath, []byte(client), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, routeURL := range []string{"http://127.0.0.1:1/v1", "http://127.0.0.1:2/v1"} {
		endpoint, stop := startOverrideServer(t, ctx, executable, home, nativeHome, routeURL)
		for connection := 0; connection < 2; connection++ {
			p := connectOverrideServer(t, ctx, endpoint)
			result, err := p.call(ctx, "config/read", map[string]any{"includeLayers": true})
			if err != nil {
				t.Fatal(err)
			}
			config, _ := result["config"].(map[string]any)
			if config["model_provider"] != "dorf" || config["model_reasoning_effort"] != "high" {
				t.Fatalf("effective selection/preferences = %#v", config)
			}
			providers, _ := config["model_providers"].(map[string]any)
			provider, _ := providers["dorf"].(map[string]any)
			url, _ := provider["base_url"].(string)
			if url != routeURL {
				t.Fatalf("effective provider URL = %q, want %q", url, routeURL)
			}
			servers, _ := config["mcp_servers"].(map[string]any)
			server, _ := servers["example"].(map[string]any)
			if server["command"] != "example-mcp" || server["enabled"] != false {
				t.Fatalf("effective MCP = %#v", server)
			}
			_ = p.connection.CloseNow()
			contents, err := os.ReadFile(configPath)
			if err != nil || string(contents) != client {
				t.Fatal("client configuration bytes changed")
			}
			t.Logf("effective provider, client MCP and preferences preserved on connection %d with route %s", connection+1, routeURL)
		}
		stop()
	}
}

func startOverrideServer(t *testing.T, ctx context.Context, executable, home, nativeHome, routeURL string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "ws://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string][]byte{routeKeyPath: []byte("synthetic-route-key\n"), routeOptionsPath: routeOptions(routeURL)} {
		local := strings.ReplaceAll(path, "/root/", home+"/")
		if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := appServerScript(endpoint, tokenSHA256("synthetic-control-token"), false)
	script = strings.ReplaceAll(script, "/root/", home+"/")
	script = strings.ReplaceAll(script, serverControlDir, home+"/control")
	command := exec.CommandContext(ctx, "bash", "-c", script)
	command.Dir = home
	command.Env = []string{"PATH=" + filepath.Dir(executable) + ":" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + nativeHome}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("launch: %v: %s", err, output)
	}
	pidBytes, err := os.ReadFile(home + "/control/codex-app-server.pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = process.Kill()
		if t.Failed() {
			contents, _ := os.ReadFile(home + "/control/codex-app-server.log")
			t.Logf("isolated app-server: %s", contents)
		}
	}
	t.Cleanup(stop)
	return endpoint, stop
}

func connectOverrideServer(t *testing.T, ctx context.Context, endpoint string) *protocol {
	t.Helper()
	var last error
	for ctx.Err() == nil {
		p, err := dialProtocol(ctx, endpoint, "synthetic-control-token", nil, nil)
		if err == nil {
			return p
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("connect isolated Codex app-server: %v", last)
	return nil
}
