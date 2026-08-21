package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type cleanupTarget struct {
	Sandbox Sandbox
	Kind    ActionKind
}

// RegisterCleanup installs the Core-owned resource cleanup task.
func (a Application) RegisterCleanup() {
	a.Tasks.MustRegister(absurd.Task(CleanupTaskName, func(ctx context.Context, params JobTaskParams) (TaskResultV1, error) {
		if err := a.VerifyAttachedTask(ctx, params.JobID, CleanupTaskName, params.PreviousTaskID); err != nil {
			return TaskResultV1{}, err
		}
		job, err := a.Store.Job(ctx, params.JobID)
		if err != nil {
			return TaskResultV1{}, err
		}
		if a.CleanupRuntimes == nil {
			return TaskResultV1{}, fmt.Errorf("Sandbox runtime resolution is not configured")
		}
		runtime, err := a.CleanupRuntimes.ResolveCleanup(ctx, job.SandboxProfile)
		if err != nil {
			return TaskResultV1{}, fmt.Errorf("resolve Sandbox profile %q: %w", job.SandboxProfile, err)
		}
		if strings.TrimSpace(runtime.SandboxProfile) != job.SandboxProfile {
			detail := fmt.Sprintf("Job requires Sandbox profile %q, but this worker resolved %q", job.SandboxProfile, strings.TrimSpace(runtime.SandboxProfile))
			if attentionErr := a.Store.SetCleanupAttention(ctx, job.ID, detail); attentionErr != nil {
				return TaskResultV1{}, fmt.Errorf("%s; record profile mismatch attention: %w", detail, attentionErr)
			}
			return TaskResultV1{}, fmt.Errorf("%s", detail)
		}
		return absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (TaskResultV1, error) {
			if err := a.runCleanup(workCtx, runtime.Execution, params.JobID); err != nil {
				return TaskResultV1{}, err
			}
			return TaskResultV1{JobID: params.JobID, Outcome: "cleanup-complete"}, nil
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
		if !actionSucceeded(actions, target.Kind, target.Sandbox.ID) {
			return target.Kind, target.Sandbox.ID, true
		}
	}
	return "", "", false
}

func (a Application) runCleanup(ctx context.Context, service CleanupExecution, jobID string) error {
	job, sandboxes, err := service.PrepareCleanup(ctx, jobID)
	if err != nil {
		return err
	}
	if job.CleanupState == CleanupComplete {
		return nil
	}
	if job.CleanupAttention != "" {
		if err := a.Store.SetCleanupAttention(ctx, jobID, ""); err != nil {
			return err
		}
	}

	for _, target := range cleanupTargets(sandboxes) {
		action, err := a.Store.GetOrCreateSandboxAction(ctx, target.Sandbox.ID, target.Kind)
		if err != nil {
			return err
		}
		if action.State == ActionSucceeded {
			continue
		}

		detail := fmt.Sprintf("reconciling %s for Sandbox %s", target.Kind, target.Sandbox.ID)
		err = absurdruntime.RunActionStep(ctx, action.ID, func(workCtx context.Context) error {
			return service.ExecuteSandboxAction(workCtx, job.ID, action.ID)
		})
		if err != nil {
			_ = a.Store.SetCleanupAttention(ctx, jobID, detail+": "+err.Error())
			return fmt.Errorf("reconcile %s for Sandbox %s: %w", target.Kind, target.Sandbox.ID, err)
		}
	}

	detail := "verifying no owned resource or non-cleanup Job claim remains unsettled"
	task, ok := absurd.TaskFromContext(ctx)
	if !ok {
		return absurd.ErrNoTaskContext
	}
	if err := a.Store.CompleteCleanup(ctx, jobID, task.TaskID()); err != nil {
		_ = a.Store.SetCleanupAttention(ctx, jobID, detail+": "+err.Error())
		return err
	}
	return nil
}

func actionSucceeded(actions []Action, kind ActionKind, sandboxID string) bool {
	for _, action := range actions {
		if action.Kind == kind && action.Scope == sandboxID && action.State == ActionSucceeded {
			return true
		}
	}
	return false
}
