package terminal

import (
	"context"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

// Harness is the Sandbox-local agent boundary already consumed by Externals.
// Implementations own their native process, protocol, session history, and
// profile-specific Provider Route configuration.
type Harness interface {
	Name() string
	InstallRoute(context.Context, provider.Ownership, string, string, string) error
	RemoveRoute(context.Context, provider.Ownership) error

	StartInitialTurn(context.Context, provider.Ownership, string, string, core.HarnessInput, string, string, bool) (core.HarnessBinding, error)
	ReadInitialTurns(context.Context, provider.Ownership, string) (core.HarnessHistory, error)
	ReadTurns(context.Context, provider.Ownership, string) (core.HarnessHistory, error)
	StartTurn(context.Context, provider.Ownership, string, string, string, core.HarnessInput, string, string, bool) (core.HarnessBinding, error)
	SteerTurn(context.Context, provider.Ownership, string, string, string, core.HarnessInput) (string, error)
}

type InterruptibleHarness interface {
	InterruptTurn(context.Context, provider.Ownership, string, string) (core.HarnessBinding, error)
}

// ScopedHarness retains native resources only for the supplied operation.
// The bound Harness must not escape the callback.
type ScopedHarness interface {
	WithOperation(context.Context, provider.Ownership, string, func(context.Context, Harness) error) error
}
