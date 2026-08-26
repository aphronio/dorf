package incus

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

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

func (a Adapter) PutFile(ctx context.Context, owner provider.Ownership, destination string, contents []byte) error {
	if err := a.Sandbox.AttestOwnership(ctx, owner); err != nil {
		return err
	}
	return provider.PutFileViaExec(ctx, owner, destination, contents, a.Exec)
}

func (a Adapter) ReadFile(ctx context.Context, owner provider.Ownership, relativePath string) ([]byte, error) {
	if err := a.Sandbox.AttestOwnership(ctx, owner); err != nil {
		return nil, err
	}
	return provider.ReadFileViaExec(ctx, owner, a.Workspace(), relativePath, a.Exec)
}

func (a Adapter) Exec(ctx context.Context, owner provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	return a.Sandbox.Exec(ctx, owner.SandboxID, input, args...)
}

func (a Adapter) Endpoint(ctx context.Context, owner provider.Ownership, port int) (provider.Endpoint, error) {
	return a.Sandbox.PortForwardEndpoint(ctx, owner, port)
}

func (a Adapter) ProviderRouteURL(_ context.Context) (string, error) {
	value := strings.TrimSpace(a.Config.ProviderGatewayURL)
	parsed, err := url.Parse(value)
	if value == "" || value != a.Config.ProviderGatewayURL || err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "/v1" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("Incus Provider Gateway URL must be exact HTTPS /v1 or HTTP /v1 on a private non-loopback IP")
	}
	switch parsed.Scheme {
	case "https":
		return parsed.String(), nil
	case "http":
		ip := net.ParseIP(parsed.Hostname())
		if !privateGatewayIP(ip) || ip.IsLoopback() {
			return "", fmt.Errorf("Incus HTTP Provider Gateway URL must use a private non-loopback IPv4 address")
		}
		return parsed.String(), nil
	default:
		return "", fmt.Errorf("Incus Provider Gateway URL must be exact HTTPS /v1 or HTTP /v1 on a private non-loopback IP")
	}
}

func privateGatewayIP(ip net.IP) bool {
	ipv4 := ip.To4()
	return ipv4 != nil && (ipv4.IsPrivate() || ipv4[0] == 100 && ipv4[1]&0xc0 == 64)
}

var _ provider.Sandbox = Adapter{}
