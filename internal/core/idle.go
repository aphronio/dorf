package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// SandboxIdleGracePeriod is the existing provider pause grace after activity.
const SandboxIdleGracePeriod = time.Minute

// SandboxIdleReconciliation applies the admitted idle policy. It never changes
// Turn outcomes or requests cleanup. The provider remains the power-state authority.
type SandboxIdleReconciliation interface {
	ReconcileIdleSandboxes(context.Context, string) error
}

// ReconcileIdle retries on the next durable wake instead of failing successful
// work when an optional power-saving operation is temporarily unavailable.
func ReconcileIdle(ctx context.Context, runtime any, jobID string) {
	idle, ok := runtime.(SandboxIdleReconciliation)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := idle.ReconcileIdleSandboxes(ctx, jobID); err != nil {
		slog.WarnContext(ctx, "Sandbox idle reconciliation will retry", "job_id", jobID, "error", err)
	}
}

func (s ExecutionService) ReconcileIdleSandboxes(ctx context.Context, jobID string) error {
	pauser, ok := s.externals.(interface {
		SandboxPause(context.Context, Job, Sandbox) error
	})
	if !ok {
		return nil
	}
	if jobID == "" {
		return fmt.Errorf("idle reconciliation requires a Job identity")
	}
	return s.store.WithJobFence(ctx, jobID, func() error {
		job, err := s.store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if job.ID != jobID {
			return fmt.Errorf("idle reconciliation changed Job identity")
		}
		if job.KeepRunning || !job.AdmissionOpen || job.CleanupState != CleanupPending {
			return nil
		}
		idle, err := s.store.SandboxIdleFor(ctx, jobID, SandboxIdleGracePeriod)
		if err != nil || !idle {
			return err
		}
		sandboxes, err := s.store.Sandboxes(ctx, jobID)
		if err != nil {
			return err
		}
		for _, sandbox := range sandboxes {
			if sandbox.JobID != jobID {
				return fmt.Errorf("idle Sandbox has a different Job owner")
			}
			if err := pauser.SandboxPause(ctx, job, sandbox); err != nil {
				return err
			}
		}
		return nil
	})
}

// SandboxActivityStore records access under the caller's Job effect fence.
// A missing finish timestamp starts a fresh grace period after process recovery.
type SandboxActivityStore interface {
	BeginSandboxActivity(context.Context, string) error
	FinishSandboxActivity(context.Context, string) error
}

func WithSandboxActivity(ctx context.Context, store SandboxActivityStore, jobID string, operation func() error) (err error) {
	if err := store.BeginSandboxActivity(ctx, jobID); err != nil {
		return err
	}
	defer func() {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, store.FinishSandboxActivity(finishCtx, jobID))
	}()
	return operation()
}
