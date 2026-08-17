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
	AbsurdVersion       = "0.5.0"
	QueueName           = "dorf_jobs"
	SandboxProfileIncus = "incus"
	SandboxProfileE2B   = "e2b"
	defaultE2BTimeout   = 55 * time.Minute
)

type Config struct {
	DatabaseURL       string
	Harness           string
	SandboxProfile    string
	IncusImage        string
	IncusNetwork      string
	IncusDiskSize     string
	GatewayStatePath  string
	Workspace         string
	AppServerPort     int
	TurnTimeout       time.Duration
	EvidenceRoot      string
	GitHubMetadata    string
	GitHubPrivateKey  string
	GitHubAPIURL      string
	E2BAPIKey         string
	E2BTemplate       string
	E2BGatewayURL     string
	E2BSandboxTimeout time.Duration
	E2BAllowInternet  bool
}

func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve user home for provider gateway state: %w", err)
	}
	harness := value("DORF_HARNESS", "codex")
	if harness != "codex" && harness != "pi" {
		return Config{}, fmt.Errorf("DORF_HARNESS must be codex or pi")
	}
	sandboxProfile := value("DORF_SANDBOX_PROFILE", SandboxProfileIncus)
	if sandboxProfile != SandboxProfileIncus && sandboxProfile != SandboxProfileE2B {
		return Config{}, fmt.Errorf("DORF_SANDBOX_PROFILE must be incus or e2b")
	}
	cfg := Config{
		DatabaseURL:       value("DORF_DATABASE_URL", "postgresql:///dorf?host=/var/run/postgresql"),
		Harness:           harness,
		SandboxProfile:    sandboxProfile,
		IncusImage:        value("DORF_INCUS_IMAGE", "dorf"),
		IncusNetwork:      value("DORF_INCUS_NETWORK", "incusbr0"),
		IncusDiskSize:     value("DORF_INCUS_DISK_SIZE", "40GiB"),
		GatewayStatePath:  value("DORF_PROVIDER_GATEWAY_STATE", filepath.Join(dataHome(home), "dorf", "provider-gateway")),
		Workspace:         "/workspace/job",
		AppServerPort:     4500,
		TurnTimeout:       45 * time.Minute,
		EvidenceRoot:      value("DORF_EVIDENCE_ROOT", filepath.Join(home, ".local", "state", "dorf", "evidence")),
		GitHubMetadata:    value("DORF_GITHUB_APP_METADATA", filepath.Join(configHome(home), "dorf", "github-app", "app.json")),
		GitHubPrivateKey:  value("DORF_GITHUB_APP_PRIVATE_KEY", filepath.Join(configHome(home), "dorf", "github-app", "private-key.pem")),
		GitHubAPIURL:      value("DORF_GITHUB_API_URL", "https://api.github.com"),
		E2BAPIKey:         strings.TrimSpace(os.Getenv("E2B_API_KEY")),
		E2BTemplate:       strings.TrimSpace(os.Getenv("DORF_E2B_TEMPLATE")),
		E2BGatewayURL:     strings.TrimSpace(os.Getenv("DORF_E2B_PROVIDER_GATEWAY_URL")),
		E2BSandboxTimeout: defaultE2BTimeout,
	}
	if raw := strings.TrimSpace(os.Getenv("DORF_E2B_ALLOW_INTERNET")); raw != "" {
		allowed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("DORF_E2B_ALLOW_INTERNET must be true or false")
		}
		cfg.E2BAllowInternet = allowed
	}
	if raw := strings.TrimSpace(os.Getenv("DORF_E2B_SANDBOX_TIMEOUT")); raw != "" {
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 || duration%time.Second != 0 {
			return Config{}, fmt.Errorf("DORF_E2B_SANDBOX_TIMEOUT must be a positive whole-second Go duration")
		}
		cfg.E2BSandboxTimeout = duration
	}
	cfg.GatewayStatePath, err = filepath.Abs(cfg.GatewayStatePath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve Provider Gateway state locator: %w", err)
	}
	cfg.GatewayStatePath = filepath.Clean(cfg.GatewayStatePath)
	if !filepath.IsAbs(cfg.EvidenceRoot) {
		return Config{}, fmt.Errorf("DORF_EVIDENCE_ROOT must be an absolute deployment-owned path")
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
