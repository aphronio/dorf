package investigation

import (
	"context"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
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

type Service struct {
	gitworkspace.Execution
}

func NewService(execution gitworkspace.Execution) Service {
	return Service{Execution: execution}
}
