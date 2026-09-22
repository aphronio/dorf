package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
)

func TestRouteOptionsAreLiteralArguments(t *testing.T) {
	home := t.TempDir()
	optionsPath := filepath.Join(home, "options")
	// Quotes and shell syntax must be data, not evaluation or argument splitting.
	options := routeOptions(`https://gateway.example/v1?value='"$(touch SHOULD_NOT_EXIST);\`)
	if err := os.WriteFile(optionsPath, options, 0600); err != nil {
		t.Fatal(err)
	}
	script := "set -e; " + strings.ReplaceAll(loadRouteOptions, routeOptionsPath, optionsPath) + `printf '%s\000' "${route_options[@]}"`
	command := exec.Command("bash", "-c", script)
	command.Dir = home
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	expected := append([]byte("-c\x00model_provider=\"dorf\"\x00-c\x00"), bytes.TrimSuffix(options, []byte("\n"))...)
	expected = append(expected, 0)
	if !bytes.Equal(output, expected) {
		t.Fatalf("route argument transport = %q, want %q", output, expected)
	}
	if _, err := os.Stat(filepath.Join(home, "SHOULD_NOT_EXIST")); !os.IsNotExist(err) {
		t.Fatal("route options evaluated shell syntax")
	}
}

func TestSDKEnvironmentFollowsNativeRouteWithoutCredentialsInArguments(t *testing.T) {
	home := t.TempDir()
	optionsPath := filepath.Join(home, "options")
	for _, routeURL := range []string{"https://gateway.example/v1", `https://replacement.example/v1?q='"$(touch SHOULD_NOT_EXIST)`} {
		if err := os.WriteFile(optionsPath, routeOptions(routeURL), 0600); err != nil {
			t.Fatal(err)
		}
		script := "set -e; " + strings.ReplaceAll(loadRouteOptions, routeOptionsPath, optionsPath) + `python3 -c 'import os,sys,json; print(json.dumps([os.environ["OPENAI_BASE_URL"],os.environ["OPENAI_API_KEY"],sys.argv[1:]]))' "${route_options[@]}"`
		command := exec.Command("bash", "-c", script)
		command.Dir = home
		command.Env = append(os.Environ(), "DORF_PROVIDER_ROUTE_KEY=synthetic-scoped-key", "OPENAI_API_KEY=wrong-key", "OPENAI_BASE_URL=https://wrong.example/v1")
		output, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		var received []json.RawMessage
		if err := json.Unmarshal(output, &received); err != nil || len(received) != 3 {
			t.Fatalf("invalid environment receipt: %v", err)
		}
		var url, key string
		_ = json.Unmarshal(received[0], &url)
		_ = json.Unmarshal(received[1], &key)
		if url != routeURL || key != "synthetic-scoped-key" {
			t.Fatal("SDK environment differs from the installed route")
		}
		if bytes.Contains(received[2], []byte("synthetic-scoped-key")) {
			t.Fatal("credential exposed in process arguments")
		}
		if _, err := os.Stat(filepath.Join(home, "SHOULD_NOT_EXIST")); !os.IsNotExist(err) {
			t.Fatal("route URL evaluated as shell code")
		}
	}
}

func TestRouteInstallAndRemovalNeverTouchNativeConfig(t *testing.T) {
	for _, contents := range []string{"", "invalid native TOML = [", "model_provider = \"dorf\"\n[model_providers.dorf]\nbase_url = \"https://old.example/v1\"\n"} {
		t.Run(contents, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".codex/config.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			owner := testOwner("route-preservation")
			agent := Agent{Sandbox: incus.Adapter{Sandbox: incustest.OwnedSandbox(&routeRunner{home: home}, incus.Config{Workspace: home}, owner)}}
			if err := agent.InstallRoute(context.Background(), owner, "https://gateway.example/v1", "synthetic-key", "unused"); err != nil {
				t.Fatal(err)
			}
			if err := agent.RemoveRoute(context.Background(), owner); err != nil {
				t.Fatal(err)
			}
			observed, err := os.ReadFile(path)
			if err != nil || string(observed) != contents {
				t.Fatal("native client configuration changed")
			}
		})
	}
}

func TestRouteLaunchCompatibilityAndMissingCredential(t *testing.T) {
	home := t.TempDir()
	script := "set -e; " + strings.ReplaceAll(loadRouteOptions, "/root/", home+"/") + `printf '%d' "${#route_options[@]}"`
	output, err := exec.Command("bash", "-c", script).Output()
	if err != nil || string(output) != "0" {
		t.Fatalf("legacy launch options = %q, %v", output, err)
	}
	// A removed credential must stop launch even if legacy provider settings remain.
	script = appServerScript("ws://127.0.0.1:1", tokenSHA256("synthetic-token"))
	script = strings.ReplaceAll(script, "/root/", home+"/")
	script = strings.ReplaceAll(script, serverControlDir, home+"/control")
	if err := exec.Command("bash", "-c", script).Run(); err == nil {
		t.Fatal("launch ignored missing route credential")
	}
	if _, err := os.Stat(home + "/control/codex-app-server.pid"); !os.IsNotExist(err) {
		t.Fatal("launch wrote PID despite missing credential")
	}
}
