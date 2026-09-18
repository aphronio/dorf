package terminal

import (
	"context"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

// Harness is the Sandbox-local agent boundary already consumed by Externals.
// Implementations own their native process, protocol, session history, and
// profile-specific Provider Route configuration.
type Harness interface {
	Name() string
	InstallRoute(context.Context, provider.Ownership, string, string, string) error
	RemoveRoute(context.Context, provider.Ownership) error
}
