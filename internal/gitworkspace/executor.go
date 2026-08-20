package gitworkspace

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
)

type ActionStore interface {
	RecordSandboxActionSuccess(context.Context, string) error
}

type GitExternals interface {
	RepositoryClone(context.Context, core.Job, core.Sandbox, string, string, string) error
	RepositoryRevision(context.Context, core.Job, string, string) (Observation, error)
}

// Executor composes Core execution with deterministic Git workspace operations
// for Git-backed workflows.
type Executor struct {
	core.Execution
	store      ActionStore
	externals  GitExternals
	claimCheck func(context.Context) error
}

func NewExecutor(execution core.Execution, store ActionStore, externals GitExternals, claimCheck func(context.Context) error) Executor {
	return Executor{Execution: execution, store: store, externals: externals, claimCheck: claimCheck}
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
	if err := s.externals.RepositoryClone(ctx, job, sandbox, remote, revision, branch); err != nil {
		return err
	}
	if s.claimCheck == nil {
		return fmt.Errorf("durable executor claim check is not configured")
	}
	if err := s.claimCheck(ctx); err != nil {
		return err
	}
	return s.store.RecordSandboxActionSuccess(ctx, action.ID)
}
