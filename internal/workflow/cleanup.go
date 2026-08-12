package workflow

import (
	"context"
	"fmt"
	"sort"

	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
)

type cleanupTarget struct {
	Sandbox spine.Sandbox
	Kind    spine.ActionKind
}

func cleanupTargets(sandboxes []spine.Sandbox) []cleanupTarget {
	ordered := append([]spine.Sandbox(nil), sandboxes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	targets := make([]cleanupTarget, 0, 2*len(ordered))
	for _, sandbox := range ordered {
		targets = append(targets,
			cleanupTarget{Sandbox: sandbox, Kind: spine.ActionRouteRevoke},
			cleanupTarget{Sandbox: sandbox, Kind: spine.ActionSandboxDelete},
		)
	}
	return targets
}

func runCleanup(ctx context.Context, service spine.Service, store postgres.Store, jobID string) error {
	job, sandboxes, err := service.PrepareCleanup(ctx, jobID)
	if err != nil {
		return err
	}
	if job.CleanupState == spine.CleanupComplete {
		return nil
	}

	for _, target := range cleanupTargets(sandboxes) {
		action, err := store.GetOrCreateSandboxAction(ctx, target.Sandbox.ID, target.Kind)
		if err != nil {
			return err
		}
		if action.State == spine.ActionSucceeded {
			continue
		}

		detail := fmt.Sprintf("reconciling %s for Sandbox %s", target.Kind, target.Sandbox.ID)
		if err := store.SetCleanupAttention(ctx, jobID, detail); err != nil {
			return err
		}
		err = runActionStep(ctx, action.ID, func(workCtx context.Context) error {
			return service.ExecuteSandboxAction(workCtx, job, target.Sandbox, action)
		})
		if err != nil {
			_ = store.SetCleanupAttention(ctx, jobID, detail+": "+err.Error())
			return fmt.Errorf("reconcile %s for Sandbox %s: %w", target.Kind, target.Sandbox.ID, err)
		}
	}

	detail := "verifying no owned resource or non-cleanup Job claim remains unsettled"
	if err := store.SetCleanupAttention(ctx, jobID, detail); err != nil {
		return err
	}
	if err := store.CompleteCleanup(ctx, jobID); err != nil {
		_ = store.SetCleanupAttention(ctx, jobID, detail+": "+err.Error())
		return err
	}
	return nil
}
