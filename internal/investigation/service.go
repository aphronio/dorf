package investigation

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
)

const ActionRepositoryRestore core.ActionKind = "repository-restore"

type Store interface {
	Job(context.Context, string) (core.Job, error)
	JobExists(context.Context, string) (bool, error)
	AdmitInvestigation(context.Context, Admission) (core.Job, bool, error)
	NextWakeSequence(context.Context, string) (int64, error)
	CodebaseInvestigationSource(context.Context, string) (Source, error)
	Sandboxes(context.Context, string) ([]core.Sandbox, error)
	Actions(context.Context, string) ([]core.Action, error)
	CodebaseInvestigationDrafts(context.Context, string) ([]Draft, error)
	CodebaseInvestigationMessages(context.Context, string) ([]core.AgentMessageWork, error)
	SetWorkflowAttention(context.Context, string, string, string) error
	GetOrCreateSandboxAction(context.Context, string, core.ActionKind) (core.Action, error)
	RecordCodebaseInvestigationDraft(context.Context, string, core.Artifact) (Draft, bool, error)
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
	store     Store
	externals Externals
	blobs     blob.Store
}

func NewService(execution gitworkspace.Execution, store Store, externals Externals, blobs blob.Store) Service {
	return Service{Execution: execution, store: store, externals: externals, blobs: blobs}
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
	return s.Execution.ExecuteSandboxActionEffect(ctx, job.ID, action.ID, ActionRepositoryRestore, func(effectCtx context.Context, authoritativeJob core.Job, authoritativeSandbox core.Sandbox) error {
		if authoritativeJob.ID != job.ID || authoritativeSandbox.ID != sandbox.ID {
			return fmt.Errorf("repository restore authority changed before execution")
		}
		return s.externals.RepositoryRestore(effectCtx, authoritativeJob, authoritativeSandbox, source, contents)
	})
}
