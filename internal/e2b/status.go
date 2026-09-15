package e2b

import (
	"context"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func (a Adapter) ObserveOwned(ctx context.Context, owner provider.Ownership) (provider.Status, error) {
	result := provider.Status{Provider: "e2b", State: "unknown"}
	owned, err := a.Client.FindOwned(ctx, e2bOwnership(owner))
	if err != nil {
		return result, err
	}
	if owned == nil {
		result.State = "missing"
		return result, nil
	}
	result.ProviderID = owned.ProviderID
	switch owned.State {
	case "running", "paused":
		result.State = owned.State
	}
	return result, nil
}
