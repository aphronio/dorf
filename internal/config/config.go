package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	AbsurdVersion = "0.5.0"
	QueueName     = "dorf_jobs"
)

type Config struct {
	DatabaseURL      string
	IncusImage       string
	IncusNetwork     string
	IncusDiskSize    string
	GatewayStatePath string
	Workspace        string
	AppServerPort    int
	TurnTimeout      time.Duration
	EvidenceRoot     string
	GitHubMetadata   string
	GitHubPrivateKey string
	GitHubAPIURL     string
}

func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve user home for provider gateway state: %w", err)
	}
	cfg := Config{
		DatabaseURL:      strings.TrimSpace(os.Getenv("DORF_DATABASE_URL")),
		IncusImage:       value("DORF_INCUS_IMAGE", "dorf-codex"),
		IncusNetwork:     value("DORF_INCUS_NETWORK", "incusbr0"),
		IncusDiskSize:    value("DORF_INCUS_DISK_SIZE", "40GiB"),
		GatewayStatePath: value("DORF_PROVIDER_GATEWAY_STATE", filepath.Join(home, ".local", "state", "dorf", "provider-gateway")),
		Workspace:        "/workspace/job",
		AppServerPort:    4500,
		TurnTimeout:      45 * time.Minute,
		EvidenceRoot:     value("DORF_EVIDENCE_ROOT", filepath.Join(home, ".local", "state", "dorf", "evidence")),
		GitHubMetadata:   value("DORF_GITHUB_APP_METADATA", filepath.Join(configHome(home), "dorf", "github-app", "config.json")),
		GitHubPrivateKey: value("DORF_GITHUB_APP_PRIVATE_KEY", filepath.Join(configHome(home), "dorf", "github-app", "private-key.pem")),
		GitHubAPIURL:     value("DORF_GITHUB_API_URL", "https://api.github.com"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DORF_DATABASE_URL is required (use a local PostgreSQL DSN; Dorf does not start Docker or use a cloud account)")
	}
	if !filepath.IsAbs(cfg.EvidenceRoot) {
		return Config{}, fmt.Errorf("DORF_EVIDENCE_ROOT must be an absolute deployment-owned path")
	}
	if raw := strings.TrimSpace(os.Getenv("DORF_CODEX_TURN_TIMEOUT")); raw != "" {
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			return Config{}, fmt.Errorf("DORF_CODEX_TURN_TIMEOUT must be a positive Go duration")
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

func value(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
