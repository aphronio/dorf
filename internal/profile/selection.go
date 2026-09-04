package profile

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
)

type SelectionStore interface {
	SandboxProfile(context.Context, string) (core.SandboxProfile, error)
	DefaultSandboxProfile(context.Context) (core.SandboxProfile, error)
}

// SelectVerified resolves an explicit or default profile eligible for new work.
// Already-admitted work resolves its pinned profile without this eligibility check.
func SelectVerified(ctx context.Context, store SelectionStore, name string) (core.SandboxProfile, error) {
	var selected core.SandboxProfile
	var err error
	if name == "" {
		selected, err = store.DefaultSandboxProfile(ctx)
	} else {
		selected, err = store.SandboxProfile(ctx, name)
	}
	if err != nil {
		return core.SandboxProfile{}, err
	}
	if !selected.BaseVerified() {
		return core.SandboxProfile{}, fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup", selected.Name, core.BaseProfileContract)
	}
	return selected, nil
}
