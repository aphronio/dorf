package hostclientconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	filename        = "host-client.json"
	maximumFileSize = 64 << 10

	HostOrigin = "http://127.0.0.1:8745"
)

type Config struct {
	Credential     string `json:"credential"`
	EnrollmentCode string `json:"enrollment_code,omitempty"`
}

func Path(stateDir string) string {
	return filepath.Join(stateDir, filename)
}

func Load(path string) (cfg Config, found bool, err error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("inspect Dorf host client configuration: %w", err)
	}
	directoryInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return Config{}, false, fmt.Errorf("inspect Dorf host client configuration directory: %w", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o700 {
		return Config{}, false, fmt.Errorf("Dorf host client configuration directory must be a directory with mode 0700")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return Config{}, false, fmt.Errorf("Dorf host client configuration must be a regular file with mode 0600")
	}
	if info.Size() > maximumFileSize {
		return Config{}, false, fmt.Errorf("Dorf host client configuration exceeds 64 KiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, false, fmt.Errorf("open Dorf host client configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maximumFileSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, false, fmt.Errorf("decode Dorf host client configuration")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Config{}, false, fmt.Errorf("decode Dorf host client configuration")
	}
	if cfg.Credential == "" {
		return Config{}, false, fmt.Errorf("Dorf host client credential is empty")
	}
	return cfg, true, nil
}

func Save(path string, cfg Config) error {
	if cfg.Credential == "" {
		return fmt.Errorf("Dorf host client credential is empty")
	}
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".host-client-*.json")
	if err != nil {
		return fmt.Errorf("create temporary Dorf host client configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect temporary Dorf host client configuration: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(cfg); err != nil {
		temporary.Close()
		return fmt.Errorf("encode Dorf host client configuration")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary Dorf host client configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary Dorf host client configuration: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit Dorf host client configuration: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync Dorf host client configuration directory: %w", err)
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create Dorf host client configuration directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect Dorf host client configuration directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Dorf host client configuration directory must be a directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect Dorf host client configuration directory: %w", err)
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
