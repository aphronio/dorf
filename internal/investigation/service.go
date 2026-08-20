package investigation

import (
	"context"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
)

const ActionRepositoryRestore core.ActionKind = "repository-restore"

type Store interface {
	RecordSandboxActionSuccess(context.Context, string) error
}

type Externals interface {
	RepositoryRestore(context.Context, core.Job, core.Sandbox, Source, []byte) error
	RepositoryRevision(context.Context, core.Job, string, string) (gitworkspace.Observation, error)
}

// VerifyRepositoryUnchanged proves the investigation left the admitted exact
// detached checkout clean before its report becomes a workflow fact.
func (s Service) VerifyRepositoryUnchanged(ctx context.Context, job core.Job, revision string) error {
	observation, err := s.externals.RepositoryRevision(ctx, job, "", revision)
	if err != nil {
		return err
	}
	if observation.Revision != revision || observation.ComparisonBase != revision || observation.Branch != "" {
		return fmt.Errorf("investigation changed the admitted repository checkout")
	}
	return nil
}

// Service composes shared Git workspace execution with the retained-source
// materialization owned only by codebase-investigation.
type Service struct {
	gitworkspace.Execution
	store      Store
	externals  Externals
	blobs      blob.Store
	claimCheck func(context.Context) error
}

func NewService(execution gitworkspace.Execution, store Store, externals Externals, blobs blob.Store, claimCheck func(context.Context) error) Service {
	return Service{Execution: execution, store: store, externals: externals, blobs: blobs, claimCheck: claimCheck}
}

func (s Service) BlobStore() blob.Store { return s.blobs }

// ExecuteRepositoryRestore reconciles a retained exact repository input and
// records the same scoped Action only after the provider checkout converges.
func (s Service) ExecuteRepositoryRestore(ctx context.Context, job core.Job, sandbox core.Sandbox, action core.Action, source Source) error {
	if sandbox.JobID != job.ID || action.JobID != job.ID || action.Scope != sandbox.ID || action.Kind != ActionRepositoryRestore ||
		source.JobID != job.ID || source.Kind != SourceGitBundle {
		return fmt.Errorf("repository restore does not belong to the exact investigation Job and Sandbox")
	}
	contents, err := s.blobs.ReadVerified(source.BundleDigest, source.BundleByteSize)
	if err != nil {
		return fmt.Errorf("read retained repository bundle: %w", err)
	}
	if err := s.externals.RepositoryRestore(ctx, job, sandbox, source, contents); err != nil {
		return err
	}
	if s.claimCheck == nil {
		return errors.New("durable executor claim check is not configured")
	}
	if err := s.claimCheck(ctx); err != nil {
		return err
	}
	return s.store.RecordSandboxActionSuccess(ctx, action.ID)
}
