package upgrade

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// PrepareCleanup resolves a possibly lost checkpoint acknowledgement before
// the normal cleanup path deletes its source. It removes every inactive owned
// resource, including a replacement reserved before a lost create response.
func (s Service) PrepareCleanup(ctx context.Context, jobID string) error {
	return s.cleanup(ctx, jobID, func(r Receipt, source core.Sandbox) error {
		if !r.QuiescedAt.IsZero() && r.Checkpoint.Reference == "" {
			if err := s.perform(ctx, r, "checkpoint-reconcile", func() error {
				checkpoint, err := s.Driver.Capture(ctx, source, r.ID)
				if err != nil {
					return err
				}
				return s.record(ctx, func() error { return s.Store.RecordUpgradeCheckpoint(ctx, r.ID, checkpoint) })
			}); err != nil {
				return err
			}
		}
		active, err := s.Store.Sandbox(ctx, r.SandboxID)
		if err != nil {
			return err
		}
		for _, id := range []string{r.SourceResourceID, r.DestinationResourceID} {
			if id == "" || id == active.ResourceID {
				continue
			}
			deleted, err := s.resourceDeleted(ctx, jobID, id)
			if err != nil {
				return err
			}
			if deleted {
				continue
			}
			owned, err := s.Store.SandboxResource(ctx, jobID, r.SandboxID, id)
			if err != nil {
				return err
			}
			if err := s.deleteResource(ctx, r, owned); err != nil {
				return err
			}
		}
		return nil
	})
}

// CompleteCleanup runs after current-resource deletion. E2B's backing snapshot
// can only be removed after its replacement VM has been deleted.
func (s Service) CompleteCleanup(ctx context.Context, jobID string) error {
	return s.cleanup(ctx, jobID, func(r Receipt, source core.Sandbox) error {
		if r.Checkpoint.Reference == "" || !r.CheckpointDeletedAt.IsZero() {
			return nil
		}
		return s.deleteCheckpoint(ctx, r, source)
	})
}
func (s Service) cleanup(ctx context.Context, jobID string, fn func(Receipt, core.Sandbox) error) error {
	return s.Store.WithJobFence(ctx, jobID, func() error {
		job, err := s.Store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if task, ok := absurd.TaskFromContext(ctx); ok && task.TaskID() != job.CurrentTaskID {
			return fmt.Errorf("upgrade cleanup no longer owns the Job task")
		}
		if job.AdmissionOpen || job.CleanupState != core.CleanupScheduled {
			return fmt.Errorf("upgrade cleanup requires the scheduled Job cleanup owner")
		}
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		receipts, err := s.Store.JobUpgrades(ctx, jobID)
		if err != nil {
			return err
		}
		for _, r := range receipts {
			source, err := s.Store.SandboxResource(ctx, jobID, r.SandboxID, r.SourceResourceID)
			if err != nil {
				return err
			}
			if err := fn(r, source); err != nil {
				return err
			}
		}
		return nil
	})
}
