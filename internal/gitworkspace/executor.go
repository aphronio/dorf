package gitworkspace

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	policy "github.com/aphronio/dorf/internal/review"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type Operations interface {
	ReconcileClone(context.Context, provider.Ownership, string, string, string) error
	ObserveRevision(context.Context, provider.Ownership, string, string) (Observation, error)
	ChangeFacts(context.Context, provider.Ownership, string, string) (policy.ChangeFacts, error)
}

// Executor composes Core execution with deterministic Git workspace operations
// for Git-backed workflows.
type Executor struct {
	core.Execution
	workspace Operations
	ownership func(context.Context, string) (provider.Ownership, error)
}

func NewExecutor(execution core.Execution, workspace Operations, ownership func(context.Context, string) (provider.Ownership, error)) Executor {
	return Executor{Execution: execution, workspace: workspace, ownership: ownership}
}

func (s Executor) ObserveRevision(ctx context.Context, job core.Job, branch, revision string) (Observation, error) {
	owner, err := s.owner(ctx, core.MainSandboxName(job.ID))
	if err != nil {
		return Observation{}, err
	}
	return s.workspace.ObserveRevision(ctx, owner, branch, revision)
}

func (s Executor) ChangeFacts(ctx context.Context, job core.Job, baseRevision, revision string) (policy.ChangeFacts, error) {
	owner, err := s.owner(ctx, core.MainSandboxName(job.ID))
	if err != nil {
		return policy.ChangeFacts{}, err
	}
	return s.workspace.ChangeFacts(ctx, owner, baseRevision, revision)
}

// ExecuteRepositoryClone reconciles one workflow-owned remote Git input and
// records the scoped Action only after the checkout converges.
func (s Executor) ExecuteRepositoryClone(ctx context.Context, job core.Job, sandbox core.Sandbox, remote, revision, branch string) error {
	if sandbox.JobID != job.ID {
		return fmt.Errorf("repository clone does not belong to the exact Job and Sandbox")
	}
	return s.Execution.ExecuteSandboxActionEffect(ctx, job.ID, sandbox.ID, ActionRepositoryClone, func(effectCtx context.Context, authoritativeJob core.Job, authoritativeSandbox core.Sandbox) error {
		if authoritativeSandbox.JobID != authoritativeJob.ID || authoritativeSandbox.ID != core.MainSandboxName(authoritativeJob.ID) {
			return fmt.Errorf("repository clone requires the exact main Sandbox")
		}
		return s.workspace.ReconcileClone(effectCtx, ownershipMetadata(authoritativeSandbox), remote, revision, branch)
	})
}

func (s Executor) owner(ctx context.Context, sandboxID string) (provider.Ownership, error) {
	if s.ownership == nil {
		return provider.Ownership{}, fmt.Errorf("Sandbox ownership resolver is not configured")
	}
	return s.ownership(ctx, sandboxID)
}

func ownershipMetadata(sandbox core.Sandbox) provider.Ownership {
	return provider.Ownership{JobID: sandbox.JobID, SandboxID: sandbox.ID, OwnershipNonce: sandbox.OwnershipNonce}
}
