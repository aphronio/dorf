package repository_test

import (
	"context"

	"github.com/aphronio/dorf/internal/controlplane"
	"github.com/aphronio/dorf/internal/repository"
	"github.com/aphronio/dorf/internal/spine"
)

type stubExecution struct{ controlplane.Execution }

func (stubExecution) ExecuteRepositoryClone(context.Context, spine.Job, spine.Sandbox, spine.Action, string, string, string) error {
	return nil
}

var _ repository.Execution = stubExecution{}
