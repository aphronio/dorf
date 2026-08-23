package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadUsesOneSetupOwnedGitHubCredentialBundle(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("DORF_DATABASE_URL", "postgres://dorf-test")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configHome, "dorf", "integrations", "github", "credentials.json"); cfg.GitHubCredentials != want {
		t.Fatalf("GitHub credentials=%q want=%q", cfg.GitHubCredentials, want)
	}
}

func TestLoadRejectsRelativeGitHubCredentialsAndInexactAPIURL(t *testing.T) {
	t.Setenv("DORF_DATABASE_URL", "postgres://dorf-test")
	t.Setenv("DORF_GITHUB_CREDENTIALS", "relative-github/credentials.json")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must be an absolute") {
		t.Fatalf("relative credential error=%v", err)
	}
	t.Setenv("DORF_GITHUB_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv("DORF_GITHUB_API_URL", "https://example.test/api?")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "exact HTTPS URL") {
		t.Fatalf("inexact API URL error=%v", err)
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

func TestLoadKeepsOnlyE2BCredentialInHostConfiguration(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("E2B_API_KEY", "secret-test-key")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.E2BAPIKey != "secret-test-key" {
		t.Fatalf("profile selection leaked from environment: %#v", cfg)
	}
}

func TestLoadUsesNeutralBlobStoreForEvidenceAndRetainedInputs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DORF_DATABASE_URL", "postgres://dorf-test")
	t.Setenv("DORF_BLOB_ROOT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "dorf", "blobs"); cfg.BlobRoot != want {
		t.Fatalf("blob root=%q want=%q", cfg.BlobRoot, want)
	}
}

func TestLoadUsesPersistedDockerDatabase(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("DORF_DATABASE_URL", "")
	path := filepath.Join(configHome, "dorf", "deployment.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := `{"database":{"host":"127.0.0.1","port":54329,"name":"dorf","user":"dorf","password":"secret","image":"postgres:17.10-bookworm","image_id":"sha256:exact"},"e2b":{"api_key":"persisted-e2b-key"}}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseExternal || !strings.Contains(cfg.DatabaseURL, "127.0.0.1:54329/dorf") || cfg.E2BAPIKey != "persisted-e2b-key" {
		t.Fatalf("cfg=%#v", cfg)
	}
}

func TestLoadUsesHarnessIndependentTurnTimeout(t *testing.T) {
	t.Setenv("DORF_DATABASE_URL", "postgres://dorf-test")
	t.Setenv("DORF_TURN_TIMEOUT", "90s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TurnTimeout != 90*time.Second {
		t.Fatalf("turn timeout=%s want=1m30s", cfg.TurnTimeout)
	}
}
