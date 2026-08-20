package core_test

import (
	"github.com/aphronio/dorf/internal/core"
)

var (
	_ core.Execution        = core.ExecutionService{}
	_ core.CleanupExecution = core.ExecutionService{}
)
