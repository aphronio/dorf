package terminal

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gateway"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type Externals struct {
	Sandbox   provider.Sandbox
	Blobs     blob.Store
	Gateway   gateway.Gateway
	Agent     Harness
	Ownership func(context.Context, string) (provider.Ownership, error)
}

func (e Externals) Harness() string { return e.Agent.Name() }

func (e Externals) ReadSandboxFile(ctx context.Context, job core.Job, owned core.Sandbox, relativePath string) ([]byte, error) {
	if owned.JobID != job.ID {
		return nil, fmt.Errorf("Sandbox file read requires the exact Job owner")
	}
	return e.Sandbox.ReadFile(ctx, ownershipMetadata(owned), relativePath)
}

func (e Externals) SandboxCreate(ctx context.Context, job core.Job, sandbox core.Sandbox) error {
	if sandbox.JobID != job.ID {
		return fmt.Errorf("Sandbox does not belong to exact Job %s", job.ID)
	}
	owner := ownershipMetadata(sandbox)
	if err := e.Sandbox.ReconcileOwnedCreate(ctx, owner); err != nil {
		return err
	}
	if job.AgentsMD != "" {
		return e.Sandbox.PutFile(ctx, owner, filepath.Join(e.Sandbox.Workspace(), "AGENTS.md"), []byte(job.AgentsMD))
	}
	return nil
}

func ownershipMetadata(sandbox core.Sandbox) provider.Ownership {
	return provider.Ownership{JobID: sandbox.JobID, SandboxID: sandbox.ID, OwnershipNonce: sandbox.OwnershipNonce}
}

func (e Externals) RouteCreate(ctx context.Context, job core.Job, sandbox core.Sandbox, expected core.Route) error {
	if sandbox.JobID != job.ID || expected.SandboxID != sandbox.ID || expected.ID == "" {
		return fmt.Errorf("provider Route does not belong to exact Job Sandbox")
	}
	if err := e.Sandbox.AttestOwnership(ctx, ownershipMetadata(sandbox)); err != nil {
		return err
	}
	baseURL, err := e.Sandbox.ProviderRouteURL(ctx)
	if err != nil {
		return err
	}
	route, err := e.Gateway.ReconcileCreate(ctx, job.ProviderConnection, routeConsumer(sandbox), expected.ID)
	if err != nil {
		return err
	}
	if route.ID != expected.ID {
		return fmt.Errorf("provider Gateway returned a foreign Route identity")
	}
	if err := e.Gateway.RequireModel(ctx, baseURL, route.APIKey, job.Model); err != nil {
		return err
	}
	if err := e.Agent.InstallRoute(ctx, ownershipMetadata(sandbox), baseURL, route.APIKey, job.Model); err != nil {
		return err
	}
	return nil
}

func (e Externals) SteerHistory(ctx context.Context, _ core.Job, sandboxID, threadID string) (core.HarnessHistory, error) {
	owner, err := e.owner(ctx, sandboxID)
	if err != nil {
		return core.HarnessHistory{}, err
	}
	return e.Agent.ReadTurns(ctx, owner, threadID)
}

func (e Externals) AgentSteer(ctx context.Context, job core.Job, delivery core.Delivery) (string, error) {
	if delivery.AgentRun.JobID != job.ID || delivery.AgentRun.MessageID != delivery.Message.ID {
		return "", fmt.Errorf("steer requires the exact Message and Job-owned AgentRun")
	}
	owner, err := e.owner(ctx, delivery.AgentRun.SandboxID)
	if err != nil {
		return "", err
	}
	if owner.JobID != job.ID {
		return "", fmt.Errorf("steer requires the exact Job-owned Sandbox")
	}
	input, err := e.messageInput(ctx, owner, delivery.Message.ID, delivery.Message.Input, delivery.Message.Attachments)
	if err != nil {
		return "", err
	}
	return e.Agent.SteerTurn(ctx, owner, delivery.AgentRun.ThreadID, delivery.Message.TargetTurnID, delivery.AgentRun.ID, input)
}

func (e Externals) RouteRevoke(ctx context.Context, job core.Job, sandbox core.Sandbox, route core.Route) error {
	if sandbox.JobID != job.ID || route.SandboxID != sandbox.ID || route.ID == "" {
		return fmt.Errorf("Route cleanup has no exact Job-owned identity")
	}
	if err := e.Gateway.RevokeExact(ctx, routeConsumer(sandbox), route.ID); err != nil {
		return err
	}
	present, presentErr := e.Sandbox.OwnedPresent(ctx, ownershipMetadata(sandbox))
	if presentErr != nil {
		return presentErr
	}
	if present {
		if err := e.Agent.RemoveRoute(ctx, ownershipMetadata(sandbox)); err != nil {
			return err
		}
	}
	return nil
}

func (e Externals) SandboxDelete(ctx context.Context, job core.Job, sandbox core.Sandbox) error {
	if sandbox.JobID != job.ID || sandbox.ID == "" {
		return fmt.Errorf("Sandbox cleanup has no exact Job-owned identity")
	}
	return e.Sandbox.DeleteOwned(ctx, ownershipMetadata(sandbox))
}

func routeConsumer(sandbox core.Sandbox) string { return "sandbox:" + sandbox.ID }

func (e Externals) owner(ctx context.Context, sandboxID string) (provider.Ownership, error) {
	if e.Ownership == nil {
		return provider.Ownership{}, fmt.Errorf("Sandbox ownership resolver is not configured")
	}
	return e.Ownership(ctx, sandboxID)
}

var (
	_ core.Externals = Externals{}
)

func (e Externals) WriteSandboxFile(ctx context.Context, job core.Job, owned core.Sandbox, name string, contents []byte, ifAbsent bool) error {
	if owned.JobID != job.ID {
		return fmt.Errorf("Sandbox file write requires the exact Job owner")
	}
	return provider.WriteFileViaExec(ctx, ownershipMetadata(owned), e.Sandbox.Workspace(), name, contents, ifAbsent, e.Sandbox.Exec)
}

func (e Externals) ExecSandbox(ctx context.Context, job core.Job, owned core.Sandbox, command provider.Command) (provider.CommandResult, error) {
	if owned.JobID != job.ID {
		return provider.CommandResult{}, fmt.Errorf("Sandbox command requires the exact Job owner")
	}
	if err := command.Validate(); err != nil {
		return provider.CommandResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, command.Timeout()+2*time.Second)
	defer cancel()
	argv := append([]string{"timeout", "--kill-after=1s", fmt.Sprintf("%gs", command.Timeout().Seconds())}, command.Argv...)
	result, err := e.Sandbox.Exec(ctx, ownershipMetadata(owned), []byte(command.Stdin), argv...)
	if err != nil {
		return provider.CommandResult{}, err
	}
	output := provider.CommandResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}
	if len(output.Stdout) > provider.MaxCommandOutputBytes {
		output.Stdout = output.Stdout[:provider.MaxCommandOutputBytes]
		output.Truncated = true
	}
	if len(output.Stderr) > provider.MaxCommandOutputBytes {
		output.Stderr = output.Stderr[:provider.MaxCommandOutputBytes]
		output.Truncated = true
	}
	return output, nil
}
