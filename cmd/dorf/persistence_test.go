package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/telemetry"
)

func TestReadCheckpointConfigDefaultsAndMatchesExactProfile(t *testing.T) {
	cfg := validCheckpointConfig()
	cfg.IdleDelaySeconds = 0
	cfg.BackupTimeoutSeconds = 0
	cfg.ResticPath = ""
	file := writeCheckpointConfig(t, cfg, 0o600)

	loaded, err := readCheckpointConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.IdleDelaySeconds != 5 || loaded.BackupTimeoutSeconds != 120 {
		t.Fatalf("checkpoint timing defaults = idle %d, timeout %d", loaded.IdleDelaySeconds, loaded.BackupTimeoutSeconds)
	}
	if loaded.ResticPath == "" || !filepath.IsAbs(loaded.ResticPath) {
		t.Fatalf("restic path default = %q", loaded.ResticPath)
	}
	exact := core.SandboxProfileRef{Name: cfg.ProfileName, Revision: cfg.ProfileRevision}
	if !loaded.enabled(exact) {
		t.Fatal("exact configured profile was disabled")
	}
	for _, different := range []core.SandboxProfileRef{
		{Name: exact.Name + "-other", Revision: exact.Revision},
		{Name: exact.Name, Revision: strings.Repeat("b", 64)},
	} {
		if loaded.enabled(different) {
			t.Fatalf("different profile was enabled: %#v", different)
		}
	}
}

func TestReadCheckpointConfigRejectsUnsafeInputWithoutLeakingSecrets(t *testing.T) {
	const secret = "private-secret-material-that-must-not-appear"
	tests := []struct {
		name   string
		mode   os.FileMode
		mutate func(*checkpointConfig)
	}{
		{name: "group-readable file", mode: 0o640},
		{name: "non-hex profile revision", mode: 0o600, mutate: func(cfg *checkpointConfig) { cfg.ProfileRevision = strings.Repeat("z", 64) }},
		{name: "incomplete R2 repository", mode: 0o600, mutate: func(cfg *checkpointConfig) { cfg.Bucket = "" }},
		{name: "invalid timing", mode: 0o600, mutate: func(cfg *checkpointConfig) { cfg.IdleDelaySeconds = 301 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validCheckpointConfig()
			cfg.SecretAccessKey = secret
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			_, err := readCheckpointConfig(writeCheckpointConfig(t, cfg, tt.mode))
			if err == nil {
				t.Fatal("unsafe checkpoint configuration was accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("configuration error leaked secret: %v", err)
			}
		})
	}
}

func TestReadCheckpointConfigRejectsMalformedPresentFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "persistence.json")
	if err := os.WriteFile(file, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCheckpointConfig(file); err == nil || !strings.Contains(err.Error(), "invalid checkpoint configuration JSON") {
		t.Fatalf("malformed checkpoint configuration error=%v", err)
	}
}

func TestCheckpointConfigFormattingRedactsCredentials(t *testing.T) {
	cfg := validCheckpointConfig()
	const secret = "private-secret-material-that-must-not-appear"
	cfg.SecretAccessKey = secret
	for _, formatted := range []string{
		fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg),
		fmt.Sprint(&cfg), fmt.Sprintf("%+v", &cfg), fmt.Sprintf("%#v", &cfg),
	} {
		if strings.Contains(formatted, secret) || !strings.Contains(formatted, "redacted") {
			t.Fatalf("checkpoint configuration formatting was not redacted: %q", formatted)
		}
	}
}

func TestOrdinaryCheckpointStopsBeforeProviderPauseEligibility(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	lastActivity := now.Add(-10 * time.Second)
	ctx, cancel, err := checkpointCaptureContext(context.Background(), false, persistence.CaptureBoundary{LastActivityAt: lastActivity}, now)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	deadline, ok := ctx.Deadline()
	want := lastActivity.Add(core.SandboxIdleGracePeriod - checkpointPauseReserve)
	if !ok || !deadline.Equal(want) {
		t.Fatalf("ordinary checkpoint deadline = %v, %t; want %v", deadline, ok, want)
	}
}

func TestCheckpointPauseWindowDoesNotBoundRetainedOrCleanupCapture(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name        string
		keepRunning bool
		cleanup     bool
	}{
		{name: "retained", keepRunning: true},
		{name: "cleanup", cleanup: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel, err := checkpointCaptureContext(context.Background(), tt.keepRunning, persistence.CaptureBoundary{
				Cleanup: tt.cleanup, LastActivityAt: now.Add(-core.SandboxIdleGracePeriod),
			}, now)
			defer cancel()
			if err != nil {
				t.Fatal(err)
			}
			if deadline, ok := ctx.Deadline(); ok {
				t.Fatalf("capture received pause deadline %v", deadline)
			}
		})
	}
}

func TestExpiredCheckpointPauseWindowIsIneligibleBeforeCapture(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	boundary := persistence.CaptureBoundary{LastActivityAt: now.Add(-(core.SandboxIdleGracePeriod - checkpointPauseReserve))}
	ctx, cancel, err := checkpointCaptureContext(context.Background(), false, boundary, now)
	defer cancel()
	if !errors.Is(err, persistence.ErrCheckpointIneligible) {
		t.Fatalf("expired pause window error = %v", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		t.Fatalf("ineligible capture returned executable deadline %v", deadline)
	}
}

func TestCheckpointCaptureClassifiesSourceMutationWithoutPathDetails(t *testing.T) {
	var observed telemetry.Event
	capture := checkpointCapture{emit: func(event telemetry.Event) { observed = event }}
	capture.recordNativeFailure(persistence.CaptureBoundary{JobID: "job", SandboxID: "sandbox", ResourceID: "resource"}, "finish",
		&codex.PersistenceCaptureError{Class: codex.PersistenceSourceChangedCapture})
	if observed.Name != "dorf.checkpoint.native-rejected" {
		t.Fatalf("event name = %q", observed.Name)
	}
	if got := observed.Attributes["dorf.capture_failure"]; got != "source_changed_capture" {
		t.Fatalf("capture failure = %v", got)
	}
	if _, leaked := observed.Attributes["dorf.native_name"]; leaked {
		t.Fatal("native mutation filename leaked into telemetry")
	}
}

func validCheckpointConfig() checkpointConfig {
	return checkpointConfig{
		ID: "dorf-test", ProfileName: "codex-e2b", ProfileRevision: strings.Repeat("a", 64),
		Endpoint:  "https://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.r2.cloudflarestorage.com",
		AccountID: strings.Repeat("a", 32), Bucket: "dorf-checkpoints", Prefix: "dorf/checkpoints",
		AccessKeyID: "test-access-key", SecretAccessKey: "test-secret-access-key", PasswordKey: []byte(strings.Repeat("k", 32)),
		ResticPath: "/nix/var/nix/profiles/dorf-tools/bin/restic", IdleDelaySeconds: 5, BackupTimeoutSeconds: 120,
	}
}

func writeCheckpointConfig(t *testing.T, cfg checkpointConfig, mode os.FileMode) string {
	t.Helper()
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := os.WriteFile(file, encoded, mode); err != nil {
		t.Fatal(err)
	}
	return file
}
