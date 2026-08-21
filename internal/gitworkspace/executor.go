package gitworkspace

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
)

type GitExternals interface {
	RepositoryClone(context.Context, core.Job, core.Sandbox, string, string, string) error
	RepositoryRevision(context.Context, core.Job, string, string) (Observation, error)
}

// Executor composes Core execution with deterministic Git workspace operations
// for Git-backed workflows.
type Executor struct {
	core.Execution
	externals GitExternals
}

func NewExecutor(execution core.Execution, externals GitExternals) Executor {
	return Executor{Execution: execution, externals: externals}
}

func (s Executor) ObserveRevision(ctx context.Context, job core.Job, branch, revision string) (Observation, error) {
	return s.externals.RepositoryRevision(ctx, job, branch, revision)
}

// ExecuteRepositoryClone reconciles one workflow-owned remote Git input and
// records the scoped Action only after the checkout converges.
func (s Executor) ExecuteRepositoryClone(ctx context.Context, job core.Job, sandbox core.Sandbox, action core.Action, remote, revision, branch string) error {
	if action.Kind != ActionRepositoryClone {
		return fmt.Errorf("repository clone requires the exact repository-clone Action")
	}
	if sandbox.JobID != job.ID || action.JobID != job.ID || action.Scope != sandbox.ID {
		return fmt.Errorf("repository clone does not belong to the exact Job and Sandbox")
	}
	return s.Execution.ExecuteSandboxActionEffect(ctx, job.ID, action.ID, ActionRepositoryClone, func(effectCtx context.Context, authoritativeJob core.Job, authoritativeSandbox core.Sandbox) error {
		if authoritativeJob.ID != job.ID || authoritativeSandbox.ID != sandbox.ID {
			return fmt.Errorf("repository clone authority changed before execution")
		}
		return s.externals.RepositoryClone(effectCtx, authoritativeJob, authoritativeSandbox, remote, revision, branch)
	})
}
