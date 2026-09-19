package incus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

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

func (a Adapter) OwnedPresent(ctx context.Context, owner provider.Ownership) (bool, error) {
	return a.Sandbox.OwnedPresent(ctx, owner)
}

func (a Adapter) DeleteOwned(ctx context.Context, owner provider.Ownership) error {
	return a.Sandbox.DeleteOwned(ctx, owner)
}

func (a Adapter) PutFile(ctx context.Context, owner provider.Ownership, destination string, contents []byte) error {
	return provider.PutFileViaExec(ctx, owner, destination, contents, a.Exec)
}

func (a Adapter) ReadFile(ctx context.Context, owner provider.Ownership, relativePath string) ([]byte, error) {
	return provider.ReadFileViaExec(ctx, owner, a.Workspace(), relativePath, a.Exec)
}

// Exec is a convenience form of the same bounded, cancellation-aware runner.
func (a Adapter) Exec(ctx context.Context, owner provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	result, err := a.Run(ctx, owner, provider.RunRequest{Args: args, Stdin: input, Timeout: provider.DefaultCommandTimeout})
	return result.Result, err
}

func (a Adapter) Run(ctx context.Context, owner provider.Ownership, command provider.RunRequest) (provider.RunResult, error) {
	if len(command.Args) == 0 || command.Timeout <= 0 || command.Timeout > 24*time.Hour {
		return provider.RunResult{}, fmt.Errorf("command requires arguments and a bounded positive timeout")
	}
	if err := validateOwnership(owner); err != nil {
		return provider.RunResult{}, err
	}
	client, err := a.Sandbox.open(ctx)
	if err != nil {
		return provider.RunResult{}, err
	}
	defer client.Close()
	if err := a.Sandbox.attestOwnership(ctx, client, owner); err != nil {
		return provider.RunResult{}, err
	}
	// Incus has no native process deadline. GNU timeout owns the process group
	// and forwards cancellation's SIGTERM, escalating to SIGKILL after one second.
	args := []string{"timeout", "--kill-after=1s", fmt.Sprintf("%gs", command.Timeout.Seconds())}
	if len(command.Env) > 0 {
		args = append(args, "env", "--")
		keys := make([]string, 0, len(command.Env))
		for key := range command.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			args = append(args, key+"="+command.Env[key])
		}
	}
	args = append(args, command.Args...)
	ctx, cancel := context.WithTimeout(ctx, command.Timeout+2*time.Second)
	defer cancel()
	result, err := client.Exec(ctx, owner.SandboxID, command.Stdin, command.MaxOutputBytes, args...)
	out := provider.RunResult{Result: result, Stopped: err == nil}
	var stopped *commandStoppedError
	if errors.As(err, &stopped) {
		out.Stopped = true
	}
	if err == nil && result.ExitCode == 124 {
		err = provider.ErrCommandTimeout
	}
	return out, err
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
