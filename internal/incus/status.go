package incus

import (
	"context"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"strings"
)

func (s Sandbox) ObserveOwned(ctx context.Context, owner provider.Ownership) (provider.Status, error) {
	result := provider.Status{Provider: "incus", State: "unknown"}
	if err := validateOwnership(owner); err != nil {
		return result, err
	}
	client, err := s.open(ctx)
	if err != nil {
		return result, err
	}
	defer client.Close()
	present, err := s.ownedPresent(ctx, client, owner)
	if err != nil {
		return result, err
	}
	if !present {
		result.State = "missing"
		return result, nil
	}
	instance, err := client.Instance(ctx, owner.SandboxID)
	if err != nil {
		return result, err
	}
	if err := attestRequiredConfig(instance.Config, ownershipConfig(owner)); err != nil {
		return result, err
	}
	switch strings.ToLower(instance.Status) {
	case "running":
		result.State = "running"
	case "frozen":
		result.State = "paused"
	case "stopped":
		result.State = "stopped"
	}
	return result, nil
}
