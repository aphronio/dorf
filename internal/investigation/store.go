package investigation

import (
	"context"

	"github.com/aphronio/dorf/internal/core"
)

type Store interface {
	Job(context.Context, string) (core.Job, error)
	NextWakeSequence(context.Context, string) (int64, error)
	CodebaseInvestigationSource(context.Context, string) (Source, error)
	Sandboxes(context.Context, string) ([]core.Sandbox, error)
	Actions(context.Context, string) ([]core.Action, error)
	CodebaseInvestigationMessages(context.Context, string) ([]MessageRecord, error)
	SetWorkflowAttention(context.Context, string, string, string) error
}
