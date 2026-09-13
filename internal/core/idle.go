package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

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
		// Use a bounded existence query, not the conversation's growing history.
		// Include queued and uncertain work; wakes alone are not authority.
		pending, err := s.store.HasPendingAgentRuns(ctx, jobID)
		if err != nil || pending {
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
