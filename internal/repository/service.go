package repository

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/controlplane"
	"github.com/aphronio/dorf/internal/spine"
)

type ServiceStore interface {
	RecordSandboxActionSuccess(context.Context, string) error
}

type ServiceExternals interface {
	RepositoryClone(context.Context, spine.Job, spine.Sandbox, string, string, string) error
	RepositoryRevision(context.Context, spine.Job, string, string) (spine.RevisionObservation, error)
}

// Service composes Core execution with deterministic repository operations
// for repository-backed workflows.
type Service struct {
	controlplane.Execution
	store      ServiceStore
	externals  ServiceExternals
	claimCheck func(context.Context) error
}

func NewService(execution controlplane.Execution, store ServiceStore, externals ServiceExternals, claimCheck func(context.Context) error) Service {
	return Service{Execution: execution, store: store, externals: externals, claimCheck: claimCheck}
}

func (s Service) ObserveRevision(ctx context.Context, job spine.Job, branch, revision string) (spine.RevisionObservation, error) {
	return s.externals.RepositoryRevision(ctx, job, branch, revision)
}

// ExecuteRepositoryClone reconciles one workflow-owned remote Git input and
// records the scoped Action only after the checkout converges.
func (s Service) ExecuteRepositoryClone(ctx context.Context, job spine.Job, sandbox spine.Sandbox, action spine.Action, remote, revision, branch string) error {
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
