package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

const reviewAttestationPath = "/tmp/dorf/review-attestation.json"

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

func (a Adapter) AttachReviewMetadata(ctx context.Context, owner provider.Ownership, review provider.ReviewMetadata) error {
	if review.JobID != owner.JobID || review.OwnershipNonce != owner.OwnershipNonce || review.AgentRunID == "" || review.Revision == "" {
		return fmt.Errorf("review Sandbox requires complete host-owned identity metadata")
	}
	if err := a.AttestOwnership(ctx, owner); err != nil {
		return err
	}
	payload, err := json.Marshal(review)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	result, err := a.Exec(ctx, owner, payload, "bash", "-lc", "umask 077; install -d -m 700 /tmp/dorf; cat > "+reviewAttestationPath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("attach review Sandbox metadata: %s", strings.TrimSpace(result.Stderr))
	}
	return a.AttestReview(ctx, owner, review)
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

func (a Adapter) AttestReview(ctx context.Context, owner provider.Ownership, review provider.ReviewMetadata) error {
	if review.JobID != owner.JobID || review.OwnershipNonce != owner.OwnershipNonce || review.AgentRunID == "" || review.Revision == "" {
		return provider.OwnershipErrorf("review Sandbox metadata does not match its durable owner")
	}
	if err := a.AttestOwnership(ctx, owner); err != nil {
		return err
	}
	result, err := a.Exec(ctx, owner, nil, "cat", reviewAttestationPath)
	if err != nil {
		return err
	}
	var observed provider.ReviewMetadata
	if result.ExitCode != 0 || json.Unmarshal([]byte(result.Stdout), &observed) != nil || observed != review {
		return provider.OwnershipErrorf("review Sandbox metadata is missing, foreign, stale, or ambiguous")
	}
	return nil
}

func (a Adapter) ReconcileClone(ctx context.Context, owner provider.Ownership, repository, revision, branch string) error {
	if err := a.AttestOwnership(ctx, owner); err != nil {
		return err
	}
	return reconcileClone(ctx, a, owner, repository, revision, branch)
}

func (a Adapter) Exec(ctx context.Context, owner provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	owned, err := a.Client.FindOwned(ctx, e2bOwnership(owner))
	if err != nil {
		return provider.Result{}, err
	}
	if owned == nil {
		return provider.Result{}, provider.OwnershipErrorf("E2B Sandbox metadata is missing, foreign, stale, or ambiguous")
	}
	connection, err := a.Client.ConnectEnvd(ctx, owned.ProviderID, a.Config.SandboxTimeout)
	if err != nil {
		return provider.Result{}, err
	}
	executor, err := NewExecutor(connection, a.Client.HTTPClient)
	if err != nil {
		return provider.Result{}, err
	}
	var stdout, stderr bytes.Buffer
	result, execErr := executor.Exec(ctx, ExecRequest{Argv: append([]string(nil), args...), Stdin: input, ProcessTimeout: a.Config.ProcessTimeout, Stdout: &stdout, Stderr: &stderr})
	observed := provider.Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: int(result.ExitCode)}
	var exit *ExitError
	if errors.As(execErr, &exit) {
		return observed, nil
	}
	return observed, execErr
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

func (a Adapter) ProviderRouteURL(_ context.Context, _ string) (string, error) {
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

func reconcileClone(ctx context.Context, runtime provider.Sandbox, owner provider.Ownership, repository, revision, branch string) error {
	workspace := runtime.Workspace()
	gitDir, err := runtime.Exec(ctx, owner, nil, "git", "-C", workspace, "rev-parse", "--git-dir")
	if err != nil {
		return err
	}
	if gitDir.ExitCode != 0 {
		clone, err := runtime.Exec(ctx, owner, nil, "git", "clone", "--no-checkout", repository, workspace)
		if err != nil {
			return err
		}
		if clone.ExitCode != 0 {
			return fmt.Errorf("clone repository: %s", strings.TrimSpace(clone.Stderr))
		}
	} else {
		remote, err := runtime.Exec(ctx, owner, nil, "git", "-C", workspace, "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		if remote.ExitCode != 0 || strings.TrimSpace(remote.Stdout) != repository {
			return fmt.Errorf("existing Sandbox clone origin does not match admitted repository")
		}
		fetch, err := runtime.Exec(ctx, owner, nil, "git", "-C", workspace, "fetch", "--prune", "origin")
		if err != nil {
			return err
		}
		if fetch.ExitCode != 0 {
			return fmt.Errorf("refresh existing Sandbox clone: %s", strings.TrimSpace(fetch.Stderr))
		}
	}
	checkout, err := runtime.Exec(ctx, owner, nil, "git", "-C", workspace, "checkout", "-B", branch, revision)
	if err != nil {
		return err
	}
	if checkout.ExitCode != 0 {
		return fmt.Errorf("checkout admitted Revision: %s", strings.TrimSpace(checkout.Stderr))
	}
	head, err := runtime.Exec(ctx, owner, nil, "git", "-C", workspace, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head.ExitCode != 0 || strings.TrimSpace(head.Stdout) != revision {
		return fmt.Errorf("Sandbox HEAD does not match admitted Revision %q", revision)
	}
	for _, identity := range [][2]string{{"user.name", "Dorf Agent"}, {"user.email", "dorf-agent@localhost"}} {
		configured, err := runtime.Exec(ctx, owner, nil, "git", "-C", workspace, "config", "--local", identity[0], identity[1])
		if err != nil {
			return err
		}
		if configured.ExitCode != 0 {
			return fmt.Errorf("configure repository-local agent commit identity: %s", strings.TrimSpace(configured.Stderr))
		}
	}
	return nil
}

var _ provider.Sandbox = Adapter{}
