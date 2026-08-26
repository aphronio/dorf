package config

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/deployment"
)

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

func TestLoadDoesNotLetAmbientStateOverrideXDGProviderGatewayAuthority(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("DORF_DATABASE_URL", "postgres://dorf-test")
	t.Setenv("DORF_PROVIDER_GATEWAY_STATE", filepath.Join(t.TempDir(), "ambient-gateway-state"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dataHome, "dorf", "provider-gateway")
	if cfg.GatewayStatePath != want {
		t.Fatalf("gateway locator=%q want=%q", cfg.GatewayStatePath, want)
	}
}

func TestLoadAcceptsOnlyExactComposeGatewayInternalOrigin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN", "http://provider-gateway:8317")
	cfg, err := Load()
	if err != nil || cfg.GatewayInternalOrigin != "http://provider-gateway:8317" {
		t.Fatalf("internal origin=%q err=%v", cfg.GatewayInternalOrigin, err)
	}
	for _, invalid := range []string{"https://provider-gateway:8317", "http://provider-gateway:8317/v1", "http://provider-gateway:8318"} {
		t.Setenv("DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN", invalid)
		if _, err := Load(); err == nil {
			t.Fatalf("accepted internal origin %q", invalid)
		}
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

func TestLoadUsesPersistedE2BCredentialForManagedDeployment(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("DORF_DATABASE_URL", "")
	t.Setenv("E2B_API_KEY", "ambient-must-not-win")
	path := filepath.Join(configHome, "dorf", "deployment.json")
	if err := deployment.Save(path, deployment.Config{
		Database: deployment.Database{
			Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret",
		},
		E2B: &deployment.E2B{APIKey: "persisted-managed-key"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseExternal || !strings.Contains(cfg.DatabaseURL, "127.0.0.1:54329/dorf") || cfg.E2BAPIKey != "persisted-managed-key" {
		t.Fatalf("managed config=%#v", cfg)
	}
}

func TestResolvePathsIsTheOneXDGHostLayout(t *testing.T) {
	configRoot := filepath.Join(t.TempDir(), "config")
	dataRoot := filepath.Join(t.TempDir(), "data")
	stateRoot := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("XDG_STATE_HOME", stateRoot)

	paths, err := ResolvePaths("")
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		ConfigDir: filepath.Join(configRoot, "dorf"), DataDir: filepath.Join(dataRoot, "dorf"),
		StateDir: filepath.Join(stateRoot, "dorf"), ComposeDir: filepath.Join(dataRoot, "dorf-compose"),
	}
	if paths != want {
		t.Fatalf("paths=%#v want=%#v", paths, want)
	}

	t.Setenv("DORF_DATABASE_URL", "postgres://dorf-test")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeploymentPath != filepath.Join(paths.ConfigDir, "deployment.json") ||
		cfg.GatewayStatePath != filepath.Join(paths.DataDir, "provider-gateway") ||
		cfg.BlobRoot != filepath.Join(paths.StateDir, "blobs") ||
		cfg.GitHubCredentials != filepath.Join(paths.ConfigDir, "integrations", "github", "credentials.json") {
		t.Fatalf("configuration did not use resolved paths: %#v", cfg)
	}
}

func TestCurrentOperatorPathsIgnoreAmbientHomeWithoutCompleteXDG(t *testing.T) {
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Clean(account.HomeDir)
	configRoot := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	paths, err := CurrentOperatorPaths()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		ConfigDir:  filepath.Join(configRoot, "dorf"),
		DataDir:    filepath.Join(home, ".local", "share", "dorf"),
		StateDir:   filepath.Join(home, ".local", "state", "dorf"),
		ComposeDir: filepath.Join(home, ".local", "share", "dorf-compose"),
	}
	if paths != want {
		t.Fatalf("managed paths=%#v want=%#v", paths, want)
	}
}

func TestResolvePathsRejectsRelativeXDGRoots(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative-config")
	if _, err := ResolvePaths(t.TempDir()); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative XDG configuration error=%v", err)
	}
}

func TestDatabaseURLOverrideDoesNotErasePersistedIncusAuthority(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("DORF_DATABASE_URL", "postgres://external-test")
	path := filepath.Join(configHome, "dorf", "deployment.json")
	want := deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	if err := deployment.Save(path, deployment.Config{
		Database: deployment.Database{
			Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret",
		},
		Incus: &want,
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DatabaseExternal || cfg.DatabaseURL != "postgres://external-test" || cfg.Incus == nil || *cfg.Incus != want {
		t.Fatalf("configuration lost non-database deployment authority: %#v", cfg)
	}
	cfg.Incus.Endpoint = "unix:///changed.socket"
	stored, found, err := deployment.Load(path)
	if err != nil || !found || stored.Incus == nil || *stored.Incus != want {
		t.Fatalf("configuration did not clone Incus authority: stored=%#v found=%t error=%v", stored, found, err)
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
