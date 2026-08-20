package controlplane_test

import (
	"github.com/aphronio/dorf/internal/controlplane"
	"github.com/aphronio/dorf/internal/spine"
)

var (
	_ controlplane.Execution           = spine.ExecutionService{}
	_ controlplane.RepositoryExecution = spine.RepositoryService{}
	_ controlplane.CleanupExecution    = spine.ExecutionService{}
)
