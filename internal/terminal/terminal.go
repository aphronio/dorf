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
	bridgeIPv4, err := e.Sandbox.BridgeIPv4(ctx)
	if err != nil {
		return spine.Receipt{}, err
	}
	if err := requireBridgeRoute(baseURL, bridgeIPv4); err != nil {
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

func (e Externals) AgentInitialTurn(ctx context.Context, job spine.Job, delivery spine.Delivery) (string, spine.NativeTurn, error) {
	return e.Agent.StartInitialTurn(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, delivery.AgentRun.ID, delivery.Message.Input, job.Model, job.ReasoningEffort)
}

func (e Externals) AgentTurns(ctx context.Context, job spine.Job, sessionID string) ([]spine.NativeTurn, error) {
	return e.Agent.ReadTurns(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, sessionID)
}

func (e Externals) AgentSubmit(ctx context.Context, job spine.Job, delivery spine.Delivery) (spine.NativeTurn, error) {
	return e.Agent.StartTurn(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, delivery.AgentRun.SessionID, delivery.AgentRun.ID, delivery.Message.Input, job.Model, job.ReasoningEffort)
}

func (e Externals) AgentWait(ctx context.Context, job spine.Job, sessionID, turnID string) (spine.NativeTurn, error) {
	return e.Agent.WaitTurn(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, sessionID, turnID)
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

func requireBridgeRoute(baseURL, bridgeIPv4 string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("provider route URL is invalid: %w", err)
	}
	address := net.ParseIP(parsed.Hostname())
	bridge := net.ParseIP(bridgeIPv4)
	if parsed.Scheme != "http" || address == nil || address.To4() == nil || bridge == nil || bridge.To4() == nil || !bridge.IsPrivate() || bridge.IsLoopback() || !address.Equal(bridge) {
		return fmt.Errorf("provider route must use configured Incus bridge IPv4 %s", bridgeIPv4)
	}
	return nil
}
