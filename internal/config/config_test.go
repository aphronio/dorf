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

func TestLoadUsesNeutralBlobStoreForEvidenceAndArtifacts(t *testing.T) {
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
