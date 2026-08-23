package config

import (
	"fmt"
	"net/url"
	"os"
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
	DatabaseURL       string
	DatabaseExternal  bool
	DeploymentPath    string
	GatewayStatePath  string
	Workspace         string
	AppServerPort     int
	TurnTimeout       time.Duration
	BlobRoot          string
	GitHubCredentials string
	GitHubAPIURL      string
	E2BAPIKey         string
}

func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve user home for provider gateway state: %w", err)
	}
	deploymentPath := deployment.Path(home)
	cfg := Config{
		DeploymentPath:    deploymentPath,
		GatewayStatePath:  value("DORF_PROVIDER_GATEWAY_STATE", filepath.Join(dataHome(home), "dorf", "provider-gateway")),
		Workspace:         "/workspace/job",
		AppServerPort:     4500,
		TurnTimeout:       45 * time.Minute,
		BlobRoot:          value("DORF_BLOB_ROOT", filepath.Join(home, ".local", "state", "dorf", "blobs")),
		GitHubCredentials: value("DORF_GITHUB_CREDENTIALS", filepath.Join(configHome(home), "dorf", "integrations", "github", "credentials.json")),
		GitHubAPIURL:      value("DORF_GITHUB_API_URL", "https://api.github.com"),
		E2BAPIKey:         strings.TrimSpace(os.Getenv("E2B_API_KEY")),
	}
	if raw := strings.TrimSpace(os.Getenv("DORF_DATABASE_URL")); raw != "" {
		cfg.DatabaseURL = raw
		cfg.DatabaseExternal = true
	} else {
		stored, found, loadErr := deployment.Load(deploymentPath)
		if loadErr != nil {
			return Config{}, loadErr
		}
		if found {
			cfg.DatabaseURL, err = stored.Database.URL()
			if err != nil {
				return Config{}, err
			}
			if cfg.E2BAPIKey == "" && stored.E2B != nil {
				cfg.E2BAPIKey = strings.TrimSpace(stored.E2B.APIKey)
			}
		}
	}
	cfg.GatewayStatePath, err = filepath.Abs(cfg.GatewayStatePath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve Provider Gateway state locator: %w", err)
	}
	cfg.GatewayStatePath = filepath.Clean(cfg.GatewayStatePath)
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

func value(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
