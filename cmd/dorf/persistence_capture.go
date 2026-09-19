package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
)

type checkpointCapture struct {
	config  checkpointConfig
	store   postgres.Store
	sandbox provider.Sandbox
	agent   codex.Agent
	emit    func(telemetry.Event)
}

const checkpointPauseReserve = 15 * time.Second

func (r profileRuntimeResolver) checkpointService(ctx context.Context, boundary persistence.CaptureBoundary) (persistence.Service, error) {
	cfg, err := readCheckpointConfig(r.cfg.PersistenceFile)
	if err != nil {
		return persistence.Service{}, err
	}
	ref := core.SandboxProfileRef{Name: boundary.ProfileName, Revision: boundary.ProfileRevision}
	if cfg == nil || !cfg.enabled(ref) {
		return persistence.Service{}, fmt.Errorf("checkpoint profile is not configured")
	}
	profile, err := r.store.SandboxProfileRevision(ctx, ref)
	if err != nil {
		return persistence.Service{}, err
	}
	if profile.Harness != codex.Harness || profile.Provider != core.SandboxProviderE2B {
		return persistence.Service{}, fmt.Errorf("checkpoint capture requires the configured Codex E2B profile")
	}
	sandbox, err := sandboxForProfile(r.cfg, profile)
	if err != nil {
		return persistence.Service{}, err
	}
	capture := checkpointCapture{config: *cfg, store: r.store, sandbox: sandbox,
		agent: codex.Agent{Sandbox: sandbox, Port: r.cfg.AppServerPort, Timeout: r.cfg.TurnTimeout, Observations: r.observations}, emit: r.emit}
	return persistence.Service{Store: r.store, Driver: capture, IdleDelay: time.Duration(cfg.IdleDelaySeconds) * time.Second, Claim: absurdruntime.RequireClaim, Emit: r.emit}, nil
}

func (c checkpointCapture) Capture(ctx context.Context, b persistence.CaptureBoundary) (persistence.Reference, error) {
	session, err := c.store.Session(ctx, b.SessionID)
	if err != nil {
		return persistence.Reference{}, err
	}
	ctx, cancel, err := checkpointCaptureContext(ctx, session.KeepRunning, b, time.Now())
	if err != nil {
		return persistence.Reference{}, err
	}
	defer cancel()
	ctx, stopCapture := context.WithTimeout(ctx, time.Duration(c.config.BackupTimeoutSeconds)*time.Second)
	defer stopCapture()
	owned, err := c.store.Sandbox(ctx, b.SandboxID)
	if err != nil {
		return persistence.Reference{}, err
	}
	if owned.SessionID != b.SessionID || owned.ResourceID != b.ResourceID {
		return persistence.Reference{}, persistence.ErrCheckpointSuperseded
	}
	owner := checkpointOwner(owned)
	if scoped, ok := c.sandbox.(provider.ScopedAccess); ok {
		// Keep the capability alive for bounded remote cancellation even when
		// incoming work cancels capture. This scope holds no Session effect lock.
		accessCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(c.config.BackupTimeoutSeconds)*time.Second+15*time.Second)
		defer stop()
		var reference persistence.Reference
		err := scoped.WithAccess(accessCtx, owner, func(sandbox provider.Sandbox) error {
			c.sandbox, c.agent.Sandbox = sandbox, sandbox
			var err error
			reference, err = c.captureOwned(ctx, b, owner)
			return err
		})
		return reference, err
	}
	return c.captureOwned(ctx, b, owner)
}

func checkpointCaptureContext(ctx context.Context, keepRunning bool, boundary persistence.CaptureBoundary, now time.Time) (context.Context, context.CancelFunc, error) {
	if keepRunning || boundary.Cleanup {
		return ctx, func() {}, nil
	}
	// Leave time for both remote restic stop and native guard cleanup before
	// the existing provider pause policy becomes eligible.
	deadline := boundary.LastActivityAt.Add(core.SandboxIdleGracePeriod - checkpointPauseReserve)
	if !now.Before(deadline) {
		return ctx, func() {}, persistence.ErrCheckpointIneligible
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	return bounded, cancel, nil
}

func (c checkpointCapture) captureOwned(ctx context.Context, b persistence.CaptureBoundary, owner provider.Ownership) (persistence.Reference, error) {
	driver := c.restic()
	// Initialization uses the same scoped repository, independently of capture.
	// It is idempotent and does not add a checkpoint reference.
	if err := driver.InitializeRepository(ctx, owner); err != nil {
		return persistence.Reference{}, err
	}
	session, err := c.store.Session(ctx, b.SessionID)
	if err != nil {
		return persistence.Reference{}, err
	}
	guard, err := c.agent.BeginPersistenceCapture(ctx, owner, c.sandbox.Workspace(), session.ThreadID, c.config.AdditionalPaths)
	if err != nil {
		c.recordNativeFailure(b, "prepare", err)
		return persistence.Reference{}, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = c.agent.CancelPersistenceCapture(cleanupCtx, owner, guard)
	}()
	result, err := driver.Backup(ctx, owner, guard.Paths, guard.Excludes)
	c.record(b, result)
	if err != nil {
		return persistence.Reference{}, err
	}
	if err := c.agent.FinishPersistenceCapture(ctx, owner, guard); err != nil {
		c.recordNativeFailure(b, "finish", err)
		return persistence.Reference{}, err
	}
	return persistence.Reference{Repository: c.config.ID, SnapshotID: result.SnapshotID}, nil
}

func (c checkpointCapture) restic() persistence.Driver {
	return persistence.Driver{Sandbox: c.sandbox, Repository: c.config.repository(), ResticPath: c.config.ResticPath,
		OperationTimeout: time.Duration(c.config.BackupTimeoutSeconds) * time.Second}
}

func (c checkpointCapture) record(b persistence.CaptureBoundary, r persistence.Result) {
	if c.emit == nil {
		return
	}
	attributes := map[string]any{
		"dorf.session_id": b.SessionID, "dorf.sandbox_id": b.SandboxID, "dorf.resource_id": b.ResourceID,
		"duration_ms": r.Duration.Milliseconds(), "dorf.data_added_known": r.DataAddedKnown,
		"dorf.files_new": r.FilesNew, "dorf.files_changed": r.FilesChanged,
		"dorf.cancelled": r.Cancelled, "dorf.remote_stopped": r.RemoteStopped, "dorf.remote_stop_ms": r.RemoteStopDuration.Milliseconds(),
	}
	if r.DataAddedKnown {
		attributes["dorf.data_added_logical_bytes"] = r.DataAdded
		attributes["dorf.data_added_packed_bytes"] = r.DataAddedPacked
	}
	c.emit(telemetry.Event{Name: "dorf.checkpoint.storage", At: time.Now(), Attributes: attributes})
}

func (c checkpointCapture) recordNativeFailure(b persistence.CaptureBoundary, phase string, err error) {
	if c.emit == nil {
		return
	}
	class := "native_verification_failed"
	var rejected *codex.PersistenceCaptureError
	if errors.As(err, &rejected) {
		class = rejected.Class
	}
	c.emit(telemetry.Event{Name: "dorf.checkpoint.native-rejected", At: time.Now(), Attributes: map[string]any{
		"dorf.session_id": b.SessionID, "dorf.sandbox_id": b.SandboxID, "dorf.resource_id": b.ResourceID,
		"dorf.capture_phase": phase, "dorf.capture_failure": class,
	}})
}

func checkpointOwner(owned core.Sandbox) provider.Ownership {
	return provider.Ownership{SessionID: owned.SessionID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce}
}
