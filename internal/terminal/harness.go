package terminal

import (
	"context"

	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/spine"
)

// Harness is the Sandbox-local agent boundary already consumed by Externals.
// Implementations own their native process, protocol, session history, and
// profile-specific Provider Route configuration.
type Harness interface {
	Name() string
	InstallRoute(context.Context, provider.Ownership, string, string, string) error
	RemoveRoute(context.Context, provider.Ownership) error

	StartInitialTurn(context.Context, provider.Ownership, string, string, string, string, string) (spine.HarnessBinding, error)
	ReadInitialTurns(context.Context, provider.Ownership, string) (spine.HarnessHistory, error)
	ReadTurns(context.Context, provider.Ownership, string) (spine.HarnessHistory, error)
	StartTurn(context.Context, provider.Ownership, string, string, string, string, string, string) (spine.HarnessBinding, error)
	SteerTurn(context.Context, provider.Ownership, string, string, string, string) (string, error)
	WaitTurn(context.Context, provider.Ownership, string, string) (spine.HarnessBinding, error)

	StartStrictReviewTurn(context.Context, provider.Ownership, string, provider.ReviewMetadata, string, string, string, string) (spine.HarnessBinding, error)
	RecoverStrictReviewTurn(context.Context, provider.Ownership, string, provider.ReviewMetadata, string, string, string, string) (spine.HarnessBinding, error)
	WaitStrictReviewTurn(context.Context, provider.Ownership, string, provider.ReviewMetadata, string, string, string, string, string, string) (spine.HarnessBinding, error)
}
