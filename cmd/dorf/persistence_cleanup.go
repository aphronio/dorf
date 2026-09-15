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

func (e checkpointExecution) PrepareCleanup(ctx context.Context, jobID string) (core.Job, []core.Sandbox, error) {
	job, sandboxes, err := e.Execution.PrepareCleanup(ctx, jobID)
	if err != nil || job.CleanupState == core.CleanupComplete {
		return job, sandboxes, err
	}
	recovery, err := e.resolver.checkpointRecovery(ctx, job.ProfileRef())
	if err != nil {
		return job, sandboxes, err
	}
	if err := recovery.PrepareCleanup(ctx, jobID); err != nil {
		return job, sandboxes, err
	}
	for _, owned := range sandboxes {
		if err := e.captureBeforeCleanup(ctx, job, owned); err != nil {
			detail := "checkpoint before cleanup failed; source resource remains retained"
			_ = e.resolver.store.SetCleanupAttention(ctx, jobID, detail)
			return job, sandboxes, fmt.Errorf("%s", detail)
		}
	}
	return job, sandboxes, nil
}

func (e checkpointExecution) captureBeforeCleanup(ctx context.Context, job core.Job, owned core.Sandbox) error {
	profile, err := e.resolver.store.SandboxProfileRevision(ctx, job.ProfileRef())
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
			e.resolver.emit(telemetry.Event{Name: "dorf.checkpoint.source-unavailable", At: time.Now(), Attributes: map[string]any{"dorf.job_id": job.ID, "dorf.sandbox_id": owned.ID, "dorf.resource_id": owned.ResourceID}})
		}
		return nil
	}
	// The same native readiness and mutation guard used by idle capture applies.
	// Cleanup can wait and retry if a remaining writer prevents coherent capture.
	_, err = service.Capture(ctx, owned.ID, true)
	return err
}
