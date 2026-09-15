package e2b

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type savedSnapshot struct {
	ID string `json:"snapshotID"`
}

func (c Client) findCheckpoint(ctx context.Context, sourceID, key string) (string, error) {
	query := url.Values{"sandboxID": {sourceID}, "name": {key}, "limit": {"100"}}
	var snapshots []savedSnapshot
	headers, err := c.doJSONWithHeaders(ctx, http.MethodGet, "/snapshots", query, nil, http.StatusOK, &snapshots)
	if err != nil {
		return "", err
	}
	if len(snapshots) > 1 || headers.Get("X-Next-Token") != "" {
		return "", provider.OwnershipErrorf("E2B checkpoint ownership is ambiguous")
	}
	if len(snapshots) == 0 {
		return "", nil
	}
	if snapshots[0].ID == "" {
		return "", fmt.Errorf("E2B checkpoint omitted its identity")
	}
	return snapshots[0].ID, nil
}

func (a Adapter) CaptureCheckpoint(ctx context.Context, owner provider.Ownership, key string) (provider.Checkpoint, error) {
	name, err := provider.OwnedCheckpointName(owner, key)
	if err != nil {
		return provider.Checkpoint{}, err
	}
	identity := e2bOwnership(owner)
	owned, err := a.Client.FindOwned(ctx, identity)
	if err != nil {
		return provider.Checkpoint{}, err
	}
	if owned == nil {
		return provider.Checkpoint{}, provider.OwnershipErrorf("checkpoint source is absent")
	}
	detail, err := a.Client.InspectOwned(ctx, owned.ProviderID, identity)
	if err != nil {
		return provider.Checkpoint{}, err
	}
	if detail.HasExternalVolumes {
		return provider.Checkpoint{}, fmt.Errorf("E2B checkpoint coverage for attached volumes is not verified")
	}
	reference, err := a.Client.findCheckpoint(ctx, owned.ProviderID, name)
	if err != nil {
		return provider.Checkpoint{}, err
	}
	if reference == "" {
		var saved savedSnapshot
		err = a.Client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(owned.ProviderID)+"/snapshots", nil,
			map[string]string{"name": name}, http.StatusCreated, &saved)
		if err != nil {
			return provider.Checkpoint{}, err
		}
		reference = saved.ID
	}
	if reference == "" {
		return provider.Checkpoint{}, fmt.Errorf("E2B checkpoint omitted its identity")
	}
	return provider.Checkpoint{Key: key, Reference: reference, SourceID: owned.ProviderID}, nil
}

func (a Adapter) RestoreCheckpoint(ctx context.Context, source, destination provider.Ownership, checkpoint provider.Checkpoint) (string, error) {
	if source.JobID != destination.JobID || source.SandboxID != destination.SandboxID || source.OwnershipNonce == destination.OwnershipNonce {
		return "", provider.OwnershipErrorf("E2B replacement requires the same logical Sandbox and a new resource owner")
	}
	if err := a.requireCheckpoint(ctx, source, checkpoint); err != nil {
		return "", err
	}
	if err := a.Client.PauseOwned(ctx, checkpoint.SourceID, e2bOwnership(source)); err != nil {
		return "", err
	}
	identity := e2bOwnership(destination)
	owned, err := a.Client.FindOwned(ctx, identity)
	if err != nil {
		return "", err
	}
	if owned != nil {
		return owned.ProviderID, nil
	}
	var allowed []string
	if a.Config.ProviderGatewayURL != "" {
		gateway, err := a.providerGatewayURL()
		if err != nil {
			return "", err
		}
		allowed = []string{gateway.Hostname()}
	}
	created, err := a.Client.Create(ctx, CreateRequest{Template: checkpoint.Reference, Timeout: a.Config.SandboxTimeout,
		Owner: identity, AllowedHostnames: allowed, AllowInternet: a.Config.AllowInternet})
	if err != nil {
		return "", err
	}
	return created.ProviderID, nil
}

func (a Adapter) requireCheckpoint(ctx context.Context, owner provider.Ownership, checkpoint provider.Checkpoint) error {
	name, err := provider.OwnedCheckpointName(owner, checkpoint.Key)
	if err != nil {
		return err
	}
	if checkpoint.SourceID == "" || checkpoint.Reference == "" {
		return provider.OwnershipErrorf("checkpoint lacks source or recovery identity")
	}
	reference, err := a.Client.findCheckpoint(ctx, checkpoint.SourceID, name)
	if err != nil {
		return err
	}
	if reference != checkpoint.Reference {
		return provider.OwnershipErrorf("checkpoint no longer matches its recorded source and operation")
	}
	return nil
}

func (a Adapter) DeleteCheckpoint(ctx context.Context, owner provider.Ownership, checkpoint provider.Checkpoint) error {
	name, err := provider.OwnedCheckpointName(owner, checkpoint.Key)
	if err != nil {
		return err
	}
	if checkpoint.SourceID == "" || checkpoint.Reference == "" {
		return provider.OwnershipErrorf("checkpoint lacks source or recovery identity")
	}
	reference, err := a.Client.findCheckpoint(ctx, checkpoint.SourceID, name)
	if err != nil {
		return err
	}
	if reference == "" {
		existing, err := a.Client.findCheckpoint(ctx, checkpoint.SourceID, checkpoint.Reference)
		if err != nil {
			return err
		}
		if existing != "" {
			return provider.OwnershipErrorf("checkpoint exists under a different resource owner")
		}
		return nil
	}
	if reference != checkpoint.Reference {
		return provider.OwnershipErrorf("checkpoint no longer matches its recorded source and operation")
	}
	return a.Client.doJSONOneOf(ctx, http.MethodDelete, "/templates/"+url.PathEscape(checkpoint.Reference), nil, nil,
		[]int{http.StatusNoContent, http.StatusNotFound}, nil)
}
