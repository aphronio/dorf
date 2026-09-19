package main

import (
	"context"
	"errors"
	"time"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
)

// Read worker-owned configuration through the existing private reader. No
// provider command, native thread read, activity update or backup is triggered.
func (r profileRuntimeResolver) workspace(ctx context.Context, session core.Session) (persistence.Workspace, error) {
	profile, err := r.store.SandboxProfileRevision(ctx, session.ProfileRef())
	if err != nil {
		return persistence.Workspace{}, err
	}
	sandbox, err := sandboxForProfile(r.cfg, profile)
	if err != nil {
		return persistence.Workspace{}, err
	}
	cfg, err := readCheckpointConfig(r.cfg.PersistenceFile)
	if err != nil {
		return persistence.Workspace{}, err
	}
	result := persistence.Workspace{Path: sandbox.Workspace()}
	result.BackupEnabled = cfg != nil && cfg.enabled(session.ProfileRef()) && profile.Harness == codex.Harness && profile.Provider == core.SandboxProviderE2B
	if result.BackupEnabled {
		result.IdleDelaySeconds = &cfg.IdleDelaySeconds
	}
	checkpoint, err := r.store.LastCheckpoint(ctx, core.MainSandboxName(session.ID))
	if err != nil && !errors.Is(err, persistence.ErrCheckpointNotFound) {
		return persistence.Workspace{}, err
	}
	if err == nil {
		result.LastSuccessfulCheckpointAt = &checkpoint.PublishedAt
	}
	result.ObservedAt = time.Now().UTC()
	return result, nil
}
