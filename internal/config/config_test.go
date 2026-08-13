package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadUsesSetupOwnedXDGAppJSON(t *testing.T) {
	configHome := t.TempDir()
	metadata := filepath.Join(configHome, "dorf", "github-app", "app.json")
	if err := os.MkdirAll(filepath.Dir(metadata), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"app_id":"7","installation_id":"42"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("DORF_DATABASE_URL", "postgres://dorf-test")
	t.Setenv("DORF_GITHUB_APP_METADATA", "")
	t.Setenv("DORF_GITHUB_APP_PRIVATE_KEY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configHome, "dorf", "github-app", "app.json"); cfg.GitHubMetadata != want {
		t.Fatalf("GitHub metadata=%q want setup authority %q", cfg.GitHubMetadata, want)
	}
	if contents, err := os.ReadFile(cfg.GitHubMetadata); err != nil || !strings.Contains(string(contents), `"app_id":"7"`) {
		t.Fatalf("configured app.json contents=%q err=%v", contents, err)
	}
	if want := filepath.Join(configHome, "dorf", "github-app", "private-key.pem"); cfg.GitHubPrivateKey != want {
		t.Fatalf("GitHub private key=%q want %q", cfg.GitHubPrivateKey, want)
	}
}

func TestLoadResolvesProviderGatewayStateToAbsoluteLocator(t *testing.T) {
	t.Setenv("DORF_DATABASE_URL", "postgres://dorf-test")
	t.Setenv("DORF_PROVIDER_GATEWAY_STATE", "relative-gateway-state")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs("relative-gateway-state")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayStatePath != want || !filepath.IsAbs(cfg.GatewayStatePath) {
		t.Fatalf("gateway locator=%q want=%q", cfg.GatewayStatePath, want)
	}
}

func TestLoadUsesXDGDataHomeForProviderGateway(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("DORF_PROVIDER_GATEWAY_STATE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dataHome, "dorf", "provider-gateway"); cfg.GatewayStatePath != want {
		t.Fatalf("gateway state=%q want=%q", cfg.GatewayStatePath, want)
	}
}

func TestLoadSelectsOneConcreteHarnessProfile(t *testing.T) {
	t.Setenv("DORF_HARNESS", "pi")
	t.Setenv("DORF_INCUS_IMAGE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness != "pi" || cfg.IncusImage != "dorf-pi" {
		t.Fatalf("profile harness=%q image=%q", cfg.Harness, cfg.IncusImage)
	}
}

func TestLoadRejectsUnknownHarness(t *testing.T) {
	t.Setenv("DORF_HARNESS", "speculative")
	if _, err := Load(); err == nil {
		t.Fatal("expected unknown harness to be rejected")
	}
}

func TestLoadUsesHarnessIndependentTurnTimeout(t *testing.T) {
	t.Setenv("DORF_HARNESS", "pi")
	t.Setenv("DORF_TURN_TIMEOUT", "90s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TurnTimeout != 90*time.Second {
		t.Fatalf("turn timeout=%s want=1m30s", cfg.TurnTimeout)
	}
}
