package e2b

import (
	"context"
	"net/http"
	"net/url"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

// PauseOwned saves memory and disk. A lost acknowledgement is reconciled by
// inspecting the provider again; already-paused resources require no mutation.
func (c Client) PauseOwned(ctx context.Context, providerID string, owner Ownership) error {
	owned, err := c.InspectOwned(ctx, providerID, owner)
	if err != nil {
		return err
	}
	if owned.State == "paused" {
		return nil
	}
	// E2B returns 409 when its timeout policy already paused the Sandbox.
	return c.doJSONOneOf(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(providerID)+"/pause", nil,
		struct {
			KeepMemory bool `json:"memory"`
		}{KeepMemory: true}, []int{http.StatusNoContent, http.StatusConflict}, nil)
}

func (a Adapter) PauseOwned(ctx context.Context, owner provider.Ownership) error {
	identity := e2bOwnership(owner)
	owned, err := a.Client.FindOwned(ctx, identity)
	if err != nil || owned == nil {
		return err
	}
	return a.Client.PauseOwned(ctx, owned.ProviderID, identity)
}
