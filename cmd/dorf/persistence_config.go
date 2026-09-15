package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
)

// This operator-owned file is deliberately separate from public profile
// definitions. It contains durable encryption custody and must be backed up
// independently of every Sandbox it protects.
type checkpointConfig struct {
	ID                   string   `json:"id"`
	ProfileName          string   `json:"profile_name"`
	ProfileRevision      string   `json:"profile_revision"`
	Endpoint             string   `json:"endpoint"`
	AccountID            string   `json:"account_id"`
	Bucket               string   `json:"bucket"`
	Prefix               string   `json:"prefix"`
	AccessKeyID          string   `json:"access_key_id"`
	SecretAccessKey      string   `json:"secret_access_key"`
	PasswordKey          []byte   `json:"password_key"`
	ResticPath           string   `json:"restic_path"`
	AdditionalPaths      []string `json:"additional_paths,omitempty"`
	IdleDelaySeconds     int      `json:"idle_delay_seconds,omitempty"`
	BackupTimeoutSeconds int      `json:"backup_timeout_seconds,omitempty"`
}

func (checkpointConfig) String() string   { return "checkpoint configuration (credentials redacted)" }
func (checkpointConfig) GoString() string { return "checkpoint configuration (credentials redacted)" }

func readCheckpointConfig(file string) (*checkpointConfig, error) {
	if file == "" {
		return nil, nil
	}
	if !filepath.IsAbs(file) {
		return nil, fmt.Errorf("checkpoint configuration requires an absolute private file path")
	}
	info, err := os.Lstat(file)
	if err != nil {
		return nil, fmt.Errorf("checkpoint configuration is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return nil, fmt.Errorf("checkpoint configuration must be a private regular file of at most 64 KiB")
	}
	input, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("checkpoint configuration is unavailable")
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("checkpoint configuration changed while opening")
	}
	var cfg checkpointConfig
	decoder := json.NewDecoder(io.LimitReader(input, 64<<10+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("invalid checkpoint configuration JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("checkpoint configuration must contain one JSON object")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *checkpointConfig) validate() error {
	if err := c.validateIdentity(); err != nil {
		return err
	}
	if err := c.repository().Validate(); err != nil {
		return err
	}
	if c.ResticPath == "" {
		c.ResticPath = persistence.DefaultResticPath
	}
	if c.IdleDelaySeconds == 0 {
		c.IdleDelaySeconds = int(persistence.DefaultIdleDelay / time.Second)
	}
	if c.BackupTimeoutSeconds == 0 {
		c.BackupTimeoutSeconds = 120
	}
	if c.IdleDelaySeconds < 1 || c.IdleDelaySeconds > 300 || c.BackupTimeoutSeconds < 1 || c.BackupTimeoutSeconds > 1800 {
		return fmt.Errorf("checkpoint timing is outside supported bounds")
	}
	if !filepath.IsAbs(c.ResticPath) || filepath.Clean(c.ResticPath) != c.ResticPath {
		return fmt.Errorf("checkpoint restic executable must be one absolute path")
	}
	return nil
}

func (c checkpointConfig) enabled(ref core.SandboxProfileRef) bool {
	return ref.Name == c.ProfileName && ref.Revision == c.ProfileRevision
}
func (c checkpointConfig) repository() persistence.R2Repository {
	return persistence.R2Repository{Endpoint: c.Endpoint, AccountID: c.AccountID, Bucket: c.Bucket, Prefix: c.Prefix,
		ParentAccessKeyID: c.AccessKeyID, ParentSecretAccessKey: c.SecretAccessKey, PasswordKey: c.PasswordKey}
}

func (c checkpointConfig) validateIdentity() error {
	if c.ID == "" || strings.ContainsAny(c.ID, "/\\ \t\n") || len(c.ID) > 128 || c.ProfileName == "" || len(c.ProfileRevision) != 64 || len(c.PasswordKey) < 32 {
		return fmt.Errorf("checkpoint configuration requires a logical repository ID, exact profile revision and durable encryption key")
	}
	if _, err := hex.DecodeString(c.ProfileRevision); err != nil {
		return fmt.Errorf("checkpoint profile revision must be a SHA-256 digest")
	}

	return nil
}
