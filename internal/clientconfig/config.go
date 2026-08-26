// Package clientconfig owns the single remote Deployment configuration used by
// the Dorf client. The file contains client authentication material and is
// therefore deliberately separate from the host's deployment.json.
package clientconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const filename = "client.json"

// Config is the complete version-one remote client configuration.
type Config struct {
	DeploymentURL string `json:"deployment_url"`
	Credential    string `json:"credential"`
}

// Path returns the client configuration path for home. XDG_CONFIG_HOME takes
// precedence when set, matching the rest of Dorf's configuration layout.
func Path(home string) string {
	root := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if !filepath.IsAbs(root) {
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "dorf", filename)
}

// NormalizeDeploymentURL validates and normalizes an exact HTTPS Deployment
// URL. A trailing slash is omitted so API paths have one canonical spelling.
func NormalizeDeploymentURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" || (parsed.EscapedPath() != "" && parsed.EscapedPath() != "/") {
		return "", fmt.Errorf("Deployment URL must be an exact HTTPS origin without user info, path, query, or fragment")
	}
	parsed.Path, parsed.RawPath = "", ""
	return parsed.String(), nil
}

// Load reads an owner-only client configuration. A missing file is reported as
// found=false; insecure, non-regular, or malformed files fail closed.
func Load(path string) (cfg Config, found bool, err error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("inspect Dorf client configuration: %w", err)
	}
	directoryInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return Config{}, false, fmt.Errorf("inspect Dorf client configuration directory: %w", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o700 {
		return Config{}, false, fmt.Errorf("Dorf client configuration directory must be a directory with mode 0700")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return Config{}, false, fmt.Errorf("Dorf client configuration must be a regular file with mode 0600")
	}
	if info.Size() > 64<<10 {
		return Config{}, false, fmt.Errorf("Dorf client configuration exceeds 64 KiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, false, fmt.Errorf("open Dorf client configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, false, fmt.Errorf("decode Dorf client configuration")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Config{}, false, fmt.Errorf("decode Dorf client configuration")
	}
	cfg.DeploymentURL, err = NormalizeDeploymentURL(cfg.DeploymentURL)
	if err != nil {
		return Config{}, false, err
	}
	if cfg.Credential == "" {
		return Config{}, false, fmt.Errorf("Dorf client credential is empty")
	}
	return cfg, true, nil
}

// Save atomically replaces path with an owner-only client configuration.
func Save(path string, cfg Config) error {
	normalized, err := NormalizeDeploymentURL(cfg.DeploymentURL)
	if err != nil {
		return err
	}
	if cfg.Credential == "" {
		return fmt.Errorf("Dorf client credential is empty")
	}
	cfg.DeploymentURL = normalized
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".client-*.json")
	if err != nil {
		return fmt.Errorf("create temporary Dorf client configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect temporary Dorf client configuration: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(cfg); err != nil {
		temporary.Close()
		return fmt.Errorf("encode Dorf client configuration")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary Dorf client configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary Dorf client configuration: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit Dorf client configuration: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync Dorf client configuration directory: %w", err)
	}
	return nil
}

// Remove removes the exact client configuration, refusing to follow links or
// remove another kind of filesystem object. It is idempotent when absent.
func Remove(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Dorf client configuration: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Dorf client configuration path must be a regular file")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove Dorf client configuration: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync Dorf client configuration directory: %w", err)
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create Dorf client configuration directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect Dorf client configuration directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Dorf client configuration directory must be a directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect Dorf client configuration directory: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
