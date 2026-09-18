package core

import (
	"context"
	"fmt"
	"sort"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type cleanupTarget struct {
	Sandbox Sandbox
	Kind    ActionKind
}

// RegisterCleanup installs the Core-owned resource cleanup task.
func (a Application) RegisterCleanup() {
	a.Tasks.MustRegister(absurd.Task(CleanupTaskName, func(ctx context.Context, params SessionTaskParams) (TaskResultV1, error) {
		if err := a.VerifyAttachedTask(ctx, params.SessionID, CleanupTaskName, params.PreviousTaskID); err != nil {
			return TaskResultV1{}, err
		}
		session, err := a.Store.Session(ctx, params.SessionID)
		if err != nil {
			return TaskResultV1{}, err
		}
		if a.CleanupRuntimes == nil {
			return TaskResultV1{}, fmt.Errorf("Sandbox runtime resolution is not configured")
		}
		runtime, err := a.CleanupRuntimes.ResolveCleanup(ctx, session.ProfileRef())
		if err != nil {
			return TaskResultV1{}, fmt.Errorf("resolve Sandbox profile %q: %w", session.SandboxProfile, err)
		}
		if runtime.SandboxProfile != session.ProfileRef() {
			detail := fmt.Sprintf("Session requires Sandbox profile %q, but this worker resolved %q", session.SandboxProfile, runtime.SandboxProfile)
			if attentionErr := a.Store.SetCleanupAttention(ctx, session.ID, detail); attentionErr != nil {
				return TaskResultV1{}, fmt.Errorf("%s; record profile mismatch attention: %w", detail, attentionErr)
			}
			return TaskResultV1{}, fmt.Errorf("%s", detail)
		}
		return absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (TaskResultV1, error) {
			if err := a.runCleanup(workCtx, runtime.Execution, params.SessionID); err != nil {
				return TaskResultV1{}, err
			}
			return TaskResultV1{SessionID: params.SessionID, Outcome: "cleanup-complete"}, nil
		})
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
}

func cleanupTargets(sandboxes []Sandbox) []cleanupTarget {
	ordered := append([]Sandbox(nil), sandboxes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	targets := make([]cleanupTarget, 0, 2*len(ordered))
	for _, sandbox := range ordered {
		targets = append(targets,
			cleanupTarget{Sandbox: sandbox, Kind: ActionRouteRevoke},
			cleanupTarget{Sandbox: sandbox, Kind: ActionSandboxDelete},
		)
	}
	return targets
}

// CurrentCleanupAction projects the next required cleanup mutation from the
// same ordered targets used by execution. It records no additional status.
func CurrentCleanupAction(sandboxes []Sandbox, actions []Action) (ActionKind, string, bool) {
	for _, target := range cleanupTargets(sandboxes) {
		if !HasSucceededAction(actions, target.Kind, target.Sandbox.ID) {
			return target.Kind, target.Sandbox.ID, true
		}
	}
	return "", "", false
}

func (a Application) runCleanup(ctx context.Context, service CleanupExecution, sessionID string) error {
	session, sandboxes, err := service.PrepareCleanup(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.CleanupState == CleanupComplete {
		return nil
	}
	if session.CleanupAttention != "" {
		if err := a.Store.SetCleanupAttention(ctx, sessionID, ""); err != nil {
			return err
		}
	}

	for _, target := range cleanupTargets(sandboxes) {
		detail := fmt.Sprintf("reconciling %s for Sandbox %s", target.Kind, target.Sandbox.ID)
		err := service.ExecuteSandboxAction(ctx, session.ID, target.Sandbox.ID, target.Kind)
		if err != nil {
			_ = a.Store.SetCleanupAttention(ctx, sessionID, detail+": "+err.Error())
			return fmt.Errorf("reconcile %s for Sandbox %s: %w", target.Kind, target.Sandbox.ID, err)
		}
	}

	detail := "verifying no owned resource or non-cleanup Session claim remains unsettled"
	if err := service.CompleteCleanup(ctx, sessionID); err != nil {
		_ = a.Store.SetCleanupAttention(ctx, sessionID, detail+": "+err.Error())
		return err
	}
	return nil
}
