package config

import (
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/deployment"
)

const (
	AbsurdVersion = "0.5.0"
	QueueName     = "dorf_jobs"
)

type Config struct {
	DatabaseURL           string
	DatabaseExternal      bool
	DeploymentPath        string
	GatewayStatePath      string
	GatewayInternalOrigin string
	Workspace             string
	AppServerPort         int
	TurnTimeout           time.Duration
	BlobRoot              string
	GitHubCredentials     string
	GitHubAPIURL          string
	E2BAPIKey             string
	Incus                 *deployment.Incus
}

// Paths is Dorf's one host-side filesystem layout. Containers mount these
// directories at Dorf's fixed in-image XDG layout; they do not reinterpret
// the host operator's environment.
type Paths struct {
	ConfigDir  string
	DataDir    string
	StateDir   string
	ComposeDir string
}

// CurrentOperatorPaths uses a complete explicit XDG layout as-is, including
// in numeric-UID containers without a passwd entry. Otherwise it derives every
// fallback from the current account's real home directory.
func CurrentOperatorPaths() (Paths, error) {
	if completeXDGLayout() {
		return ResolvePaths("")
	}
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		return Paths{}, fmt.Errorf("resolve current Dorf operator: %w", err)
	}
	return ResolvePaths(account.HomeDir)
}

func ResolvePaths(home string) (Paths, error) {
	home = filepath.Clean(home)
	if !completeXDGLayout() && (!filepath.IsAbs(home) || home == "/") {
		return Paths{}, fmt.Errorf("Dorf operator home must be one clean absolute path")
	}
	paths := Paths{
		ConfigDir:  filepath.Join(configHome(home), "dorf"),
		DataDir:    filepath.Join(dataHome(home), "dorf"),
		StateDir:   filepath.Join(stateHome(home), "dorf"),
		ComposeDir: filepath.Join(dataHome(home), "dorf-compose"),
	}
	for label, path := range map[string]string{
		"configuration": paths.ConfigDir,
		"data":          paths.DataDir,
		"state":         paths.StateDir,
		"Compose":       paths.ComposeDir,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return Paths{}, fmt.Errorf("Dorf %s directory must be one clean absolute path", label)
		}
	}
	return paths, nil
}

func completeXDGLayout() bool {
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			return false
		}
	}
	return true
}

func Load() (Config, error) {
	paths, err := CurrentOperatorPaths()
	if err != nil {
		return Config{}, err
	}
	deploymentPath := filepath.Join(paths.ConfigDir, "deployment.json")
	cfg := Config{
		DeploymentPath:        deploymentPath,
		GatewayStatePath:      filepath.Join(paths.DataDir, "provider-gateway"),
		GatewayInternalOrigin: strings.TrimSpace(os.Getenv("DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN")),
		Workspace:             "/workspace/job",
		AppServerPort:         4500,
		TurnTimeout:           45 * time.Minute,
		BlobRoot:              value("DORF_BLOB_ROOT", filepath.Join(paths.StateDir, "blobs")),
		GitHubCredentials:     value("DORF_GITHUB_CREDENTIALS", filepath.Join(paths.ConfigDir, "integrations", "github", "credentials.json")),
		GitHubAPIURL:          value("DORF_GITHUB_API_URL", "https://api.github.com"),
		E2BAPIKey:             strings.TrimSpace(os.Getenv("E2B_API_KEY")),
	}
	stored, found, loadErr := deployment.Load(deploymentPath)
	if loadErr != nil {
		return Config{}, loadErr
	}
	databaseOverride := strings.TrimSpace(os.Getenv("DORF_DATABASE_URL"))
	if found {
		if databaseOverride == "" && stored.E2B != nil {
			// A managed deployment runs from its persisted projection. Ambient
			// credentials may seed fresh setup or an explicitly manual external
			// database process, but cannot split host commands from Compose.
			cfg.E2BAPIKey = strings.TrimSpace(stored.E2B.APIKey)
		}
		if stored.Incus != nil {
			incus := *stored.Incus
			cfg.Incus = &incus
		}
	}
	if databaseOverride != "" {
		cfg.DatabaseURL = databaseOverride
		cfg.DatabaseExternal = true
	} else if found {
		cfg.DatabaseURL, err = stored.Database.URL()
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.GatewayInternalOrigin != "" {
		origin, parseErr := url.Parse(cfg.GatewayInternalOrigin)
		if parseErr != nil || origin.Scheme != "http" || origin.Hostname() == "" || origin.Port() != "8317" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Opaque != "" {
			return Config{}, fmt.Errorf("DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN must be an exact HTTP origin on port 8317")
		}
	}
	if !filepath.IsAbs(cfg.GitHubCredentials) {
		return Config{}, fmt.Errorf("DORF_GITHUB_CREDENTIALS must be an absolute deployment-owned path")
	}
	cfg.GitHubCredentials = filepath.Clean(cfg.GitHubCredentials)
	githubAPI, parseErr := url.Parse(cfg.GitHubAPIURL)
	if parseErr != nil || githubAPI.Scheme != "https" || githubAPI.Host == "" || githubAPI.User != nil || githubAPI.RawQuery != "" || githubAPI.ForceQuery || githubAPI.Fragment != "" || githubAPI.Opaque != "" {
		return Config{}, fmt.Errorf("DORF_GITHUB_API_URL must be an exact HTTPS URL without user info, query, or fragment")
	}
	if !filepath.IsAbs(cfg.BlobRoot) {
		return Config{}, fmt.Errorf("DORF_BLOB_ROOT must be an absolute deployment-owned path")
	}
	if raw := strings.TrimSpace(os.Getenv("DORF_TURN_TIMEOUT")); raw != "" {
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			return Config{}, fmt.Errorf("DORF_TURN_TIMEOUT must be a positive Go duration")
		}
		cfg.TurnTimeout = duration
	}
	if raw := strings.TrimSpace(os.Getenv("DORF_CODEX_APP_SERVER_PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1024 || port > 65535 {
			return Config{}, fmt.Errorf("DORF_CODEX_APP_SERVER_PORT must be between 1024 and 65535")
		}
		cfg.AppServerPort = port
	}
	return cfg, nil
}

func configHome(home string) string {
	if configured := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); configured != "" {
		return configured
	}
	return filepath.Join(home, ".config")
}

func dataHome(home string) string {
	if configured := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); configured != "" {
		return configured
	}
	return filepath.Join(home, ".local", "share")
}

func stateHome(home string) string {
	if configured := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); configured != "" {
		return configured
	}
	return filepath.Join(home, ".local", "state")
}

func value(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
