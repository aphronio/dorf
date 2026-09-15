package incus

import (
	"context"
	"fmt"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type checkpointClient interface {
	CaptureOwnedCheckpoint(context.Context, string, string, map[string]string) error
	RestoreOwnedCheckpoint(context.Context, string, string, map[string]string) error
	DeleteOwnedCheckpoint(context.Context, string, string, map[string]string) error
}

func (s Sandbox) CaptureCheckpoint(ctx context.Context, owner provider.Ownership, key string) (provider.Checkpoint, error) {
	name, err := provider.OwnedCheckpointName(owner, key)
	if err != nil {
		return provider.Checkpoint{}, err
	}
	err = s.withCheckpointClient(ctx, func(client checkpointClient) error {
		return client.CaptureOwnedCheckpoint(ctx, owner.SandboxID, name, ownershipConfig(owner))
	})
	if err != nil {
		return provider.Checkpoint{}, err
	}
	return provider.Checkpoint{Key: key, Reference: name, SourceID: owner.SandboxID}, nil
}

func (s Sandbox) RestoreCheckpoint(ctx context.Context, source, destination provider.Ownership, checkpoint provider.Checkpoint) (string, error) {
	if source != destination {
		return "", provider.OwnershipErrorf("Incus restores the same resource owner")
	}
	if err := validateOwnedCheckpoint(source, checkpoint); err != nil {
		return "", err
	}
	err := s.withCheckpointClient(ctx, func(client checkpointClient) error {
		return client.RestoreOwnedCheckpoint(ctx, checkpoint.SourceID, checkpoint.Reference, ownershipConfig(source))
	})
	return checkpoint.SourceID, err
}

func (s Sandbox) DeleteCheckpoint(ctx context.Context, owner provider.Ownership, checkpoint provider.Checkpoint) error {
	if err := validateOwnedCheckpoint(owner, checkpoint); err != nil {
		return err
	}
	return s.withCheckpointClient(ctx, func(client checkpointClient) error {
		return client.DeleteOwnedCheckpoint(ctx, checkpoint.SourceID, checkpoint.Reference, ownershipConfig(owner))
	})
}

func validateOwnedCheckpoint(owner provider.Ownership, checkpoint provider.Checkpoint) error {
	name, err := provider.OwnedCheckpointName(owner, checkpoint.Key)
	if err != nil {
		return err
	}
	if checkpoint.SourceID != owner.SandboxID || checkpoint.Reference != name {
		return provider.OwnershipErrorf("Incus checkpoint does not match its exact resource owner")
	}
	return nil
}

func (s Sandbox) withCheckpointClient(ctx context.Context, fn func(checkpointClient) error) error {
	client, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	checkpoints, ok := client.(checkpointClient)
	if !ok {
		return fmt.Errorf("Incus client does not support verified checkpoints")
	}
	return fn(checkpoints)
}
