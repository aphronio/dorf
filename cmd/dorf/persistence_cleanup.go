package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/aphronio/dorf/internal/upgrade"
)

type checkpointExecution struct {
	upgrade.Execution
	resolver profileRuntimeResolver
}

func (e checkpointExecution) PrepareCleanup(ctx context.Context, sessionID string) (core.Session, []core.Sandbox, error) {
	session, sandboxes, err := e.Execution.PrepareCleanup(ctx, sessionID)
	if err != nil || session.CleanupState == core.CleanupComplete {
		return session, sandboxes, err
	}
	recovery, err := e.resolver.checkpointRecovery(ctx, session.ProfileRef())
	if err != nil {
		return session, sandboxes, err
	}
	if err := recovery.PrepareCleanup(ctx, sessionID); err != nil {
		return session, sandboxes, err
	}
	branch, branched, err := e.resolver.store.SessionCheckpointBranch(ctx, sessionID)
	if err != nil {
		return session, sandboxes, err
	}
	if branched && branch.ReadyAt.IsZero() {
		// A held branch has accepted no native input. Cleanup owns only its
		// destination resources and releases the hold at completion.
		return session, sandboxes, nil
	}
	for _, owned := range sandboxes {
		if err := e.captureBeforeCleanup(ctx, session, owned); err != nil {
			detail := "checkpoint before cleanup failed; source resource remains retained"
			_ = e.resolver.store.SetCleanupAttention(ctx, sessionID, detail)
			return session, sandboxes, fmt.Errorf("%s", detail)
		}
	}
	return session, sandboxes, nil
}

func (e checkpointExecution) captureBeforeCleanup(ctx context.Context, session core.Session, owned core.Sandbox) error {
	profile, err := e.resolver.store.SandboxProfileRevision(ctx, session.ProfileRef())
	if err != nil {
		return err
	}
	sandbox, err := sandboxForProfile(e.resolver.cfg, profile)
	if err != nil {
		return err
	}
	owner, err := e.resolver.store.Boundary(ctx, owned.ID, true)
	if err != nil {
		return err
	}
	service, err := e.resolver.checkpointService(ctx, owner)
	if err != nil {
		return err
	}
	present, err := sandbox.OwnedPresent(ctx, checkpointOwner(owned))
	if err != nil {
		return err
	}
	if !present {
		if e.resolver.emit != nil {
			e.resolver.emit(telemetry.Event{Name: "dorf.checkpoint.source-unavailable", At: time.Now(), Attributes: map[string]any{"dorf.session_id": session.ID, "dorf.sandbox_id": owned.ID, "dorf.resource_id": owned.ResourceID}})
		}
		return nil
	}
	// The same native readiness and mutation guard used by idle capture applies.
	// Cleanup can wait and retry if a remaining writer prevents coherent capture.
	_, err = service.Capture(ctx, owned.ID, true)
	return err
}
