package terminal

import (
	"context"

	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/spine"
)

// Harness is the Sandbox-local agent boundary already consumed by Externals.
// Implementations own their native process, protocol, session history, and
// profile-specific Provider Route configuration.
type Harness interface {
	Name() string
	InstallRoute(context.Context, string, string, string, string) error
	RemoveRoute(context.Context, string) error

	StartInitialTurn(context.Context, string, string, string, string, string, string) (spine.HarnessBinding, error)
	ReadInitialTurns(context.Context, string, string) (spine.HarnessHistory, error)
	ReadTurns(context.Context, string, string) (spine.HarnessHistory, error)
	StartTurn(context.Context, string, string, string, string, string, string, string) (spine.HarnessBinding, error)
	SteerTurn(context.Context, string, string, string, string, string) (string, error)
	WaitTurn(context.Context, string, string, string) (spine.HarnessBinding, error)

	StartStrictReviewTurn(context.Context, string, string, incus.ReviewMetadata, string, string, string, string) (spine.HarnessBinding, error)
	RecoverStrictReviewTurn(context.Context, string, string, incus.ReviewMetadata, string, string, string, string) (spine.HarnessBinding, error)
	WaitStrictReviewTurn(context.Context, string, string, incus.ReviewMetadata, string, string, string, string, string, string) (spine.HarnessBinding, error)
}
