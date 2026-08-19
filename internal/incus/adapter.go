package incus

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

// Adapter exposes Incus through Dorf's provider-neutral Sandbox contract.
// The embedded Sandbox remains the provider-specific command implementation.
type Adapter struct{ Sandbox }

func (a Adapter) Workspace() string { return a.Config.Workspace }

func (a Adapter) ReconcileOwnedCreate(ctx context.Context, owner provider.Ownership) error {
	return a.Sandbox.ReconcileOwnedCreate(ctx, owner)
}

func (a Adapter) AttestOwnership(ctx context.Context, owner provider.Ownership) error {
	return a.Sandbox.AttestOwnership(ctx, owner)
}

func (a Adapter) AttachReviewMetadata(ctx context.Context, owner provider.Ownership, review provider.ReviewMetadata) error {
	return a.Sandbox.AttachReviewMetadata(ctx, owner, review)
}

func (a Adapter) OwnedPresent(ctx context.Context, owner provider.Ownership) (bool, error) {
	return a.Sandbox.OwnedPresent(ctx, owner)
}

func (a Adapter) DeleteOwned(ctx context.Context, owner provider.Ownership) error {
	return a.Sandbox.DeleteOwned(ctx, owner)
}

func (a Adapter) AttestReview(ctx context.Context, owner provider.Ownership, review provider.ReviewMetadata) error {
	if owner.JobID != review.JobID || owner.OwnershipNonce != review.OwnershipNonce {
		return fmt.Errorf("review Sandbox identity conflicts with its durable owner")
	}
	return a.Sandbox.AttestReview(ctx, owner.SandboxID, review)
}

func (a Adapter) ReconcileClone(ctx context.Context, owner provider.Ownership, repository, revision, branch string) error {
	if err := a.Sandbox.AttestOwnership(ctx, owner); err != nil {
		return err
	}
	return a.Sandbox.ReconcileClone(ctx, owner.SandboxID, repository, revision, branch)
}

func (a Adapter) PutFile(ctx context.Context, owner provider.Ownership, destination string, contents []byte) error {
	if err := a.Sandbox.AttestOwnership(ctx, owner); err != nil {
		return err
	}
	return provider.PutFileViaExec(ctx, owner, destination, contents, a.Exec)
}

func (a Adapter) Exec(ctx context.Context, owner provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	return a.Sandbox.Exec(ctx, owner.SandboxID, input, args...)
}

func (a Adapter) Endpoint(ctx context.Context, owner provider.Ownership, port int) (provider.Endpoint, error) {
	if port < 1 || port > 65535 {
		return provider.Endpoint{}, fmt.Errorf("Incus endpoint port must be between 1 and 65535")
	}
	if err := a.Sandbox.AttestOwnership(ctx, owner); err != nil {
		return provider.Endpoint{}, err
	}
	address, err := a.Sandbox.PrivateIPv4(ctx, owner.SandboxID)
	if err != nil {
		return provider.Endpoint{}, err
	}
	endpoint := "ws://" + net.JoinHostPort(address, strconv.Itoa(port))
	return provider.NewEndpoint(endpoint, endpoint, nil), nil
}

func (a Adapter) ProviderRouteURL(ctx context.Context, baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("provider route URL is invalid: %w", err)
	}
	bridgeIPv4, err := a.Sandbox.BridgeIPv4(ctx)
	if err != nil {
		return "", err
	}
	address := net.ParseIP(parsed.Hostname())
	bridge := net.ParseIP(bridgeIPv4)
	if parsed.Scheme != "http" || address == nil || address.To4() == nil || bridge == nil || bridge.To4() == nil || !bridge.IsPrivate() || bridge.IsLoopback() || !address.Equal(bridge) {
		return "", fmt.Errorf("provider route must use configured Incus bridge IPv4 %s", bridgeIPv4)
	}
	return baseURL, nil
}

var _ provider.Sandbox = Adapter{}
