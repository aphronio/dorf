package incus

import (
	"context"
	"fmt"
	"net/http"

	"github.com/lxc/incus/v7/shared/api"
)

func (c *sdkClient) checkpointSource(ctx context.Context, name string, required map[string]string) error {
	instance, _, err := c.serverFor(ctx).GetInstance(name)
	if err != nil {
		return fmt.Errorf("inspect checkpoint source: %w", err)
	}
	if err := attestRequiredConfig(instance.Config, required); err != nil {
		return err
	}
	if instance.Ephemeral {
		return fmt.Errorf("checkpoint requires a non-ephemeral Incus VM")
	}
	for _, device := range instance.ExpandedDevices {
		if device["type"] == "disk" && device["path"] != "" && device["path"] != "/" {
			return fmt.Errorf("Incus checkpoint does not cover attached custom volumes")
		}
	}
	return nil
}

func (c *sdkClient) ownedCheckpoint(ctx context.Context, name, key string, required map[string]string) (bool, error) {
	snapshot, _, err := c.serverFor(ctx).GetInstanceSnapshot(name, key)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Incus checkpoint: %w", err)
	}
	if snapshot.Stateful {
		return false, fmt.Errorf("package upgrade requires a filesystem checkpoint")
	}
	if err := attestRequiredConfig(snapshot.Config, required); err != nil {
		return false, err
	}
	return true, nil
}

func (c *sdkClient) stopForCheckpoint(ctx context.Context, name string, required map[string]string) error {
	server := c.serverFor(ctx)
	instance, etag, err := server.GetInstance(name)
	if err != nil {
		return err
	}
	if err := attestRequiredConfig(instance.Config, required); err != nil {
		return err
	}
	if !instance.IsActive() {
		return nil
	}
	op, err := server.UpdateInstanceState(name, api.InstanceStatePut{Action: "stop", Timeout: 60}, etag)
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}

func (c *sdkClient) CaptureOwnedCheckpoint(ctx context.Context, name, key string, required map[string]string) error {
	if err := c.checkpointSource(ctx, name, required); err != nil {
		return err
	}
	present, err := c.ownedCheckpoint(ctx, name, key, required)
	if err != nil {
		return err
	}
	if !present {
		if err := c.stopForCheckpoint(ctx, name, required); err != nil {
			return err
		}
		op, err := c.serverFor(ctx).CreateInstanceSnapshot(name, api.InstanceSnapshotsPost{Name: key})
		if err != nil {
			return err
		}
		if err := op.WaitContext(ctx); err != nil {
			return err
		}
	}
	return c.StartInstance(ctx, name)
}

func (c *sdkClient) RestoreOwnedCheckpoint(ctx context.Context, name, key string, required map[string]string) error {
	if err := c.checkpointSource(ctx, name, required); err != nil {
		return err
	}
	present, err := c.ownedCheckpoint(ctx, name, key, required)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("Incus recovery checkpoint is absent")
	}
	if err := c.stopForCheckpoint(ctx, name, required); err != nil {
		return err
	}
	server := c.serverFor(ctx)
	instance, etag, err := server.GetInstance(name)
	if err != nil {
		return err
	}
	if err := attestRequiredConfig(instance.Config, required); err != nil {
		return err
	}
	writable := instance.Writable()
	writable.Restore = name + "/" + key
	op, err := server.UpdateInstance(name, writable, etag)
	if err != nil {
		return err
	}
	if err := op.WaitContext(ctx); err != nil {
		return err
	}
	return c.StartInstance(ctx, name)
}

func (c *sdkClient) DeleteOwnedCheckpoint(ctx context.Context, name, key string, required map[string]string) error {
	present, err := c.ownedCheckpoint(ctx, name, key, required)
	if err != nil || !present {
		return err
	}
	op, err := c.serverFor(ctx).DeleteInstanceSnapshot(name, key)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}
