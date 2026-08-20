package gitworkspace

import (
	"context"

	"github.com/aphronio/dorf/internal/controlplane"
	"github.com/aphronio/dorf/internal/spine"
)

const ActionRepositoryClone spine.ActionKind = "repository-clone"

// Execution is the Git workspace module's composition of Core execution with
// exact remote materialization. It is not part of the Core
// application contract.
type Execution interface {
	controlplane.Execution
	ExecuteRepositoryClone(context.Context, spine.Job, spine.Sandbox, spine.Action, string, string, string) error
}
