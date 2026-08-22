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
	CodebaseInvestigationMessages(context.Context, string) ([]MessageRecord, error)
	SetWorkflowAttention(context.Context, string, string, string) error
}

type RetainedRestorer interface {
	Reconcile(context.Context, core.Job, core.Sandbox, Source, []byte) error
}

// Service composes shared Git workspace execution with the retained-source
// materialization owned only by codebase-investigation.
type Service struct {
	gitworkspace.Execution
	restore RetainedRestorer
	blobs   blob.Store
}

func NewService(execution gitworkspace.Execution, restore RetainedRestorer, blobs blob.Store) Service {
	return Service{Execution: execution, restore: restore, blobs: blobs}
}

// ExecuteRepositoryRestore reconciles a retained exact repository input and
// records the same scoped Action only after the provider checkout converges.
func (s Service) ExecuteRepositoryRestore(ctx context.Context, job core.Job, sandbox core.Sandbox, source Source) error {
	if sandbox.JobID != job.ID || source.JobID != job.ID || source.Kind != SourceGitBundle {
		return fmt.Errorf("repository restore does not belong to the exact investigation Job and Sandbox")
	}
	contents, err := s.blobs.ReadVerified(source.BundleDigest, source.BundleByteSize)
	if err != nil {
		return fmt.Errorf("read retained repository bundle: %w", err)
	}
	return s.Execution.ExecuteSandboxActionEffect(ctx, job.ID, sandbox.ID, ActionRepositoryRestore, func(effectCtx context.Context, authoritativeJob core.Job, authoritativeSandbox core.Sandbox) error {
		return s.restore.Reconcile(effectCtx, authoritativeJob, authoritativeSandbox, source, contents)
	})
}
