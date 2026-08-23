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

	StartInitialTurn(context.Context, provider.Ownership, string, string, string, string, string) (core.HarnessBinding, error)
	ReadInitialTurns(context.Context, provider.Ownership, string) (core.HarnessHistory, error)
	ReadTurns(context.Context, provider.Ownership, string) (core.HarnessHistory, error)
	StartTurn(context.Context, provider.Ownership, string, string, string, string, string, string) (core.HarnessBinding, error)
	SteerTurn(context.Context, provider.Ownership, string, string, string, string) (string, error)

	StartStrictReviewTurn(context.Context, provider.Ownership, string, provider.ReviewMetadata, string, string, string, string) (core.HarnessBinding, error)
	RecoverStrictReviewTurn(context.Context, provider.Ownership, string, provider.ReviewMetadata, string, string, string, string) (core.HarnessBinding, error)
	ReadStrictReviewTurn(context.Context, provider.Ownership, string, provider.ReviewMetadata, string, string, string, string, string, string) (core.HarnessBinding, error)
}
