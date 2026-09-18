package e2b

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type AdapterConfig struct {
	Template           string
	Workspace          string
	SandboxTimeout     time.Duration
	ProcessTimeout     time.Duration
	ProviderGatewayURL string
	AllowInternet      bool
}

// Adapter exposes only E2B capabilities already earned by live proofs. Remote
// Provider Gateway admission requires one exact deployment-supplied HTTPS URL.
type Adapter struct {
	Client Client
	Config AdapterConfig
}

func e2bOwnership(owner provider.Ownership) Ownership {
	return Ownership{JobID: owner.JobID, SandboxID: owner.SandboxID, OwnershipNonce: owner.OwnershipNonce}
}

func (a Adapter) Workspace() string { return a.Config.Workspace }

func (a Adapter) ReconcileOwnedCreate(ctx context.Context, owner provider.Ownership) error {
	identity := e2bOwnership(owner)
	var allowedHostnames []string
	if strings.TrimSpace(a.Config.ProviderGatewayURL) != "" {
		gatewayURL, err := a.providerGatewayURL()
		if err != nil {
			return err
		}
		allowedHostnames = []string{gatewayURL.Hostname()}
	}
	present, err := a.Client.FindOwned(ctx, identity)
	if err != nil {
		return err
	}
	if present == nil {
		if _, err := a.Client.Create(ctx, CreateRequest{Template: a.Config.Template, Timeout: a.Config.SandboxTimeout, Owner: identity, AllowedHostnames: allowedHostnames, AllowInternet: a.Config.AllowInternet}); err != nil {
			// Create is deliberately attempted once. A later durable retry uses
			// FindOwned before deciding whether another mutation is safe.
			return err
		}
	}
	if err := a.AttestOwnership(ctx, owner); err != nil {
		return err
	}
	check, err := a.Exec(ctx, owner, nil, "bash", "-lc", "test ! -e /root/.codex/auth.json && test ! -e /root/.pi/agent/auth.json && test ! -e /root/.config/dorf/provider-route.key && test ! -e /root/.codex/config.toml && test ! -e /root/.pi/agent/models.json")
	if err != nil {
		return err
	}
	if check.ExitCode != 0 {
		return fmt.Errorf("Sandbox is not credential-free before its scoped route")
	}
	workspace, err := a.Exec(ctx, owner, nil, "mkdir", "-p", a.Config.Workspace)
	if err != nil {
		return err
	}
	if workspace.ExitCode != 0 {
		return fmt.Errorf("prepare Sandbox workspace: %s", strings.TrimSpace(workspace.Stderr))
	}
	return nil
}

func (a Adapter) AttestOwnership(ctx context.Context, owner provider.Ownership) error {
	identity := e2bOwnership(owner)
	owned, err := a.Client.FindOwned(ctx, identity)
	if err != nil {
		return err
	}
	if owned == nil {
		return provider.OwnershipErrorf("E2B Sandbox metadata is missing, foreign, stale, or ambiguous")
	}
	_, err = a.Client.InspectOwned(ctx, owned.ProviderID, identity)
	return err
}

func (a Adapter) OwnedPresent(ctx context.Context, owner provider.Ownership) (bool, error) {
	owned, err := a.Client.FindOwned(ctx, e2bOwnership(owner))
	return owned != nil, err
}

func (a Adapter) DeleteOwned(ctx context.Context, owner provider.Ownership) error {
	identity := e2bOwnership(owner)
	owned, err := a.Client.FindOwned(ctx, identity)
	if err != nil || owned == nil {
		return err
	}
	return a.Client.DeleteOwned(ctx, owned.ProviderID, identity)
}

func (a Adapter) PutFile(ctx context.Context, owner provider.Ownership, destination string, contents []byte) error {
	return provider.PutFileViaExec(ctx, owner, destination, contents, a.Exec)
}

func (a Adapter) ReadFile(ctx context.Context, owner provider.Ownership, relativePath string) ([]byte, error) {
	return provider.ReadFileViaExec(ctx, owner, a.Workspace(), relativePath, a.Exec)
}

// Exec is the convenience form of Run; all commands share cancellation and stop confirmation.
func (a Adapter) Exec(ctx context.Context, owner provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	timeout := a.Config.ProcessTimeout
	if timeout <= 0 {
		timeout = provider.DefaultCommandTimeout
	}
	result, err := a.Run(ctx, owner, provider.RunRequest{Args: args, Stdin: input, Timeout: timeout})
	return result.Result, err
}

func providerExecResult(result ExecResult, stdout, stderr string, execErr error) (provider.Result, error) {
	observed := provider.Result{Stdout: stdout, Stderr: stderr, ExitCode: int(result.ExitCode)}
	var exit *ExitError
	if errors.As(execErr, &exit) && ordinaryProcessExit(exit.Result) {
		return observed, nil
	}
	return observed, execErr
}

func ordinaryProcessExit(result ExecResult) bool {
	if !result.Exited {
		return false
	}
	return result.RemoteError == "" || (result.ExitCode != 0 && result.RemoteError == fmt.Sprintf("exit status %d", result.ExitCode))
}

func (a Adapter) Endpoint(ctx context.Context, owner provider.Ownership, port int) (provider.Endpoint, error) {
	owned, err := a.Client.FindOwned(ctx, e2bOwnership(owner))
	if err != nil {
		return provider.Endpoint{}, err
	}
	if owned == nil {
		return provider.Endpoint{}, provider.OwnershipErrorf("E2B Sandbox metadata is missing, foreign, stale, or ambiguous")
	}
	endpoint, err := a.Client.ConnectEndpoint(ctx, owned.ProviderID, port, a.Config.SandboxTimeout)
	if err != nil {
		return provider.Endpoint{}, err
	}
	return provider.NewEndpoint(endpoint.ListenURL, endpoint.DialURL, endpoint.Headers()), nil
}

func (a Adapter) ProviderRouteURL(_ context.Context) (string, error) {
	if strings.TrimSpace(a.Config.ProviderGatewayURL) == "" {
		return "", &provider.UnsupportedError{Capability: "remote-provider-gateway-route"}
	}
	parsed, err := a.providerGatewayURL()
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

// Validate checks the immutable runtime-profile inputs without contacting E2B
// or mutating provider state.
func (a Adapter) Validate() error {
	if strings.TrimSpace(a.Config.Template) == "" {
		return fmt.Errorf("E2B Sandbox requires a pinned template reference")
	}
	if a.Config.SandboxTimeout <= 0 || a.Config.SandboxTimeout%time.Second != 0 {
		return fmt.Errorf("E2B Sandbox timeout must be a positive whole number of seconds")
	}
	if strings.TrimSpace(a.Config.Workspace) == "" {
		return fmt.Errorf("E2B Sandbox requires a workspace")
	}
	_, err := a.providerGatewayURL()
	return err
}

func (a Adapter) providerGatewayURL() (*url.URL, error) {
	parsed, err := url.Parse(a.Config.ProviderGatewayURL)
	if err != nil {
		return nil, fmt.Errorf("E2B Provider Gateway URL is invalid: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/v1" {
		return nil, fmt.Errorf("E2B Provider Gateway URL must be an exact HTTPS /v1 endpoint")
	}
	return parsed, nil
}

var _ provider.Sandbox = Adapter{}
