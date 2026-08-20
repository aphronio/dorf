package gitworkspace_test

import (
	"context"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
)

type stubExecution struct{ core.Execution }

func (stubExecution) ExecuteRepositoryClone(context.Context, core.Job, core.Sandbox, core.Action, string, string, string) error {
	return nil
}

var _ gitworkspace.Execution = stubExecution{}
