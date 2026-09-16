package codex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
	"github.com/pelletier/go-toml/v2"
)

const clientRouteConfig = `# Client-owned configuration
model_reasoning_effort = "high"
[mcp_servers.example]
command = "example-mcp"
args = ["--read-only"]
enabled = false
[features]
web_search_request = false
`

// Execute the actual guest commands in a temporary home, not a shell-shaped mock.
type routeRunner struct{ home string }

func (r *routeRunner) Run(ctx context.Context, _ string, input []byte, args ...string) (incus.Result, error) {
	for i, arg := range args {
		if arg != "--" {
			continue
		}
		command := append([]string(nil), args[i+1:]...)
		for j := range command {
			command[j] = strings.ReplaceAll(command[j], "/root/", r.home+"/")
		}
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		result := incus.Result{Stdout: stdout.String(), Stderr: stderr.String()}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
			return result, nil
		}
		return result, err
	}
	return incus.Result{}, errors.New("expected guest exec")
}

func TestRouteLifecyclePreservesClientConfiguration(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(clientRouteConfig), 0600); err != nil {
		t.Fatal(err)
	}
	owner := testOwner("route-config")
	agent := Agent{Sandbox: incus.Adapter{Sandbox: incustest.OwnedSandbox(&routeRunner{home: home}, incus.Config{Workspace: home}, owner)}}
	for _, key := range []string{"synthetic-key", "synthetic-key", "replacement-key"} {
		if err := agent.InstallRoute(context.Background(), owner, "https://gateway.example/v1", key, "unused"); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasSuffix(contents, []byte(clientRouteConfig)) {
			t.Fatal("route installation changed client bytes")
		}
		var parsed map[string]any
		if err := toml.Unmarshal(contents, &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed["model_provider"] != "dorf" {
			t.Fatal("Dorf provider not selected")
		}
		credential, err := os.ReadFile(filepath.Join(home, ".config/dorf/provider-route.key"))
		if err != nil || string(credential) != key+"\n" {
			t.Fatal("route credential not updated")
		}
		info, err := os.Stat(filepath.Join(home, ".config/dorf/provider-route.key"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("route credential is not private")
		}
	}
	if err := agent.RemoveRoute(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != clientRouteConfig {
		t.Fatal("route removal changed client bytes")
	}
	if _, err := os.Stat(filepath.Join(home, ".config/dorf/provider-route.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("route credential remains")
	}
	if err := agent.RemoveRoute(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
}
