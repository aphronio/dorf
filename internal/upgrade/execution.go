package upgrade

import (
	"context"
	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
)

type Execution struct {
	core.ExecutionService
	Upgrades Service
}

func (e Execution) ReconcileJobAgent(ctx context.Context, jobID string) (core.AgentReconciliationProgress, error) {
	progress, err := absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (bool, error) { return e.Upgrades.Reconcile(workCtx, jobID) })
	if err != nil {
		return core.AgentReconciliationIdle, err
	}
	if progress {
		return core.AgentReconciliationReady, nil
	}
	return e.ExecutionService.ReconcileJobAgent(ctx, jobID)
}
func (e Execution) PrepareCleanup(ctx context.Context, jobID string) (core.Job, []core.Sandbox, error) {
	job, sandboxes, err := e.ExecutionService.PrepareCleanup(ctx, jobID)
	if err != nil || job.CleanupState == core.CleanupComplete {
		return job, sandboxes, err
	}
	return job, sandboxes, e.Upgrades.PrepareCleanup(ctx, jobID)
}
func (e Execution) CompleteCleanup(ctx context.Context, jobID string) error {
	if err := e.Upgrades.CompleteCleanup(ctx, jobID); err != nil {
		return err
	}
	return e.ExecutionService.CompleteCleanup(ctx, jobID)
}
