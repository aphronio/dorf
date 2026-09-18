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

func (e Execution) ReconcileSession(ctx context.Context, sessionID string) (core.SessionReconciliationProgress, error) {
	progress, err := absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (bool, error) { return e.Upgrades.Reconcile(workCtx, sessionID) })
	if err != nil {
		return core.SessionReconciliationIdle, err
	}
	if progress {
		return core.SessionReconciliationReady, nil
	}
	return e.ExecutionService.ReconcileSession(ctx, sessionID)
}
func (e Execution) PrepareCleanup(ctx context.Context, sessionID string) (core.Session, []core.Sandbox, error) {
	session, sandboxes, err := e.ExecutionService.PrepareCleanup(ctx, sessionID)
	if err != nil || session.CleanupState == core.CleanupComplete {
		return session, sandboxes, err
	}
	return session, sandboxes, e.Upgrades.PrepareCleanup(ctx, sessionID)
}
func (e Execution) CompleteCleanup(ctx context.Context, sessionID string) error {
	if err := e.Upgrades.CompleteCleanup(ctx, sessionID); err != nil {
		return err
	}
	return e.ExecutionService.CompleteCleanup(ctx, sessionID)
}
