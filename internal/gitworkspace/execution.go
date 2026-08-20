package gitworkspace

import (
	"context"

	"github.com/aphronio/dorf/internal/core"
)

const ActionRepositoryClone core.ActionKind = "repository-clone"

// Execution is the Git workspace module's composition of Core execution with
// exact remote materialization. It is not part of the Core
// application contract.
type Execution interface {
	core.Execution
	ExecuteRepositoryClone(context.Context, core.Job, core.Sandbox, core.Action, string, string, string) error
}
