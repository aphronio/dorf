package terminal

import (
	"context"
	"fmt"
	"net"
	"net/url"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/spine"
)

type Externals struct {
	Sandbox incus.Sandbox
	Gateway gateway.Gateway
	Agent   codex.Agent
}

func (e Externals) SandboxCreate(ctx context.Context, job spine.Job, _ spine.Action) (spine.Receipt, error) {
	id, err := e.Sandbox.ReconcileCreate(ctx, job.ID)
	return spine.Receipt{ExternalID: id}, err
}

func (e Externals) RepositoryClone(ctx context.Context, job spine.Job, _ spine.Action) (spine.Receipt, error) {
	name := e.Sandbox.Name(job.ID)
	err := e.Sandbox.ReconcileClone(ctx, name, job.Repository, job.Revision, job.Branch)
	return spine.Receipt{ExternalID: name + ":" + e.Sandbox.Config.Workspace}, err
}

func (e Externals) RouteCreate(ctx context.Context, job spine.Job, action spine.Action) (spine.Receipt, error) {
	baseURL, err := e.Gateway.BaseURL()
	if err != nil {
		return spine.Receipt{}, err
	}
	if err := requirePrivateRoute(baseURL); err != nil {
		return spine.Receipt{}, err
	}
	route, err := e.Gateway.ReconcileCreate(ctx, job.ProviderConnection, "sandbox:"+job.ID, action.ID)
	if err != nil {
		return spine.Receipt{}, err
	}
	if err := e.Sandbox.InstallRoute(ctx, e.Sandbox.Name(job.ID), route.BaseURL, route.APIKey); err != nil {
		return spine.Receipt{}, err
	}
	return spine.Receipt{ExternalID: route.ID}, nil
}

func (e Externals) AgentRun(ctx context.Context, job spine.Job, _ spine.Action, turn spine.Action) (spine.Receipt, spine.Receipt, error) {
	sessionID, outcome, err := e.Agent.ReconcileRun(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, job.SessionID, turn.ID, job.Goal, job.Model, job.ReasoningEffort)
	return spine.Receipt{ExternalID: sessionID}, spine.Receipt{ExternalID: outcome.ID, Outcome: outcome.Status}, err
}

func (e Externals) RouteRevoke(ctx context.Context, job spine.Job, _ spine.Action) (spine.Receipt, error) {
	id, err := e.Gateway.Revoke(ctx, "sandbox:"+job.ID)
	if err != nil {
		return spine.Receipt{}, err
	}
	_ = e.Sandbox.RemoveRoute(ctx, e.Sandbox.Name(job.ID))
	return spine.Receipt{ExternalID: id}, nil
}

func (e Externals) SandboxDelete(ctx context.Context, job spine.Job, _ spine.Action) (spine.Receipt, error) {
	name := e.Sandbox.Name(job.ID)
	err := e.Sandbox.Delete(ctx, name)
	return spine.Receipt{ExternalID: name}, err
}

func requirePrivateRoute(baseURL string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("provider route URL is invalid: %w", err)
	}
	address := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "http" || address == nil || address.To4() == nil || !address.IsPrivate() || address.IsLoopback() {
		return fmt.Errorf("provider route must use the private Incus bridge IPv4 address")
	}
	return nil
}
