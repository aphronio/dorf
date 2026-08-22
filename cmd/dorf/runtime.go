package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/gateway"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/investigation"
	outcomeapp "github.com/aphronio/dorf/internal/outcome"
	piagent "github.com/aphronio/dorf/internal/pi"
	"github.com/aphronio/dorf/internal/postgres"
	profileapp "github.com/aphronio/dorf/internal/profile"
	"github.com/aphronio/dorf/internal/publication"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/terminal"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type profileRuntimeResolver struct {
	cfg     config.Config
	store   postgres.Store
	client  *absurd.Client
	barrier core.FaultBarrier
}

func (r profileRuntimeResolver) ResolveCleanup(ctx context.Context, name string) (core.CleanupRuntime, error) {
	resolved, err := r.resolveBase(ctx, name, false)
	if err != nil {
		return core.CleanupRuntime{}, err
	}
	return core.CleanupRuntime{
		Execution:      resolved.Execution,
		SandboxProfile: resolved.Profile.SandboxProfile,
	}, nil
}

func (r profileRuntimeResolver) ResolveSandbox(ctx context.Context, name string) (core.SandboxRuntime, error) {
	resolved, err := r.resolveBase(ctx, name, true)
	if err != nil {
		return core.SandboxRuntime{}, err
	}
	return core.SandboxRuntime{
		Execution:      resolved.Execution,
		Files:          resolved.Externals,
		SandboxProfile: resolved.Profile.SandboxProfile,
	}, nil
}

func (r profileRuntimeResolver) ResolveCoding(ctx context.Context, name string) (coding.Runtime, error) {
	resolved, err := r.resolveBase(ctx, name, true)
	if err != nil {
		return coding.Runtime{}, err
	}
	workspaceExecutor := gitworkspace.NewExecutor(resolved.Execution, resolved.Externals)
	codingService := coding.NewService(workspaceExecutor, r.store, resolved.Externals, blob.Store{Root: r.cfg.BlobRoot}, absurdruntime.RequireClaim)
	githubClient := githubapi.Client{APIURL: r.cfg.GitHubAPIURL, Metadata: r.cfg.GitHubMetadata, PrivateKey: r.cfg.GitHubPrivateKey}
	publicationService := publication.Service{
		Store: r.store, GitHub: githubClient,
		Repository: publication.GitRepository{Sandbox: resolved.Sandbox, Workspace: r.cfg.Workspace, Ownership: resolved.Ownership},
		Evidence:   blob.Store{Root: r.cfg.BlobRoot}, Barrier: r.barrier,
	}.WithClaimCheck(absurdruntime.RequireClaim)
	outcomeService := (outcomeapp.Service{Store: r.store, GitHub: githubClient}).WithClaimCheck(absurdruntime.RequireClaim)
	return coding.Runtime{
		Profile: resolved.Profile,
		Agent:   resolved.Execution,
		Coding:  codingService,
		Proposal: coding.ProposalRuntime{
			Publication: publicationService, GitHub: githubClient,
			Outcome: outcomeService, Store: r.store,
			AdmitMessage: func(ctx context.Context, jobID, fromID, input string) (core.MessageReceipt, error) {
				job, err := coreApplication(r.store, r.client).OpenJob(ctx, jobID)
				if err != nil {
					return core.MessageReceipt{}, err
				}
				sandbox, err := job.DefaultSandbox(ctx)
				if err != nil {
					return core.MessageReceipt{}, err
				}
				return sandbox.Agent().Message(ctx, fromID, input)
			},
		},
	}, nil
}

func (r profileRuntimeResolver) ResolveInvestigation(ctx context.Context, name string) (investigation.Runtime, error) {
	resolved, err := r.resolveBase(ctx, name, true)
	if err != nil {
		return investigation.Runtime{}, err
	}
	workspaceExecutor := gitworkspace.NewExecutor(resolved.Execution, resolved.Externals)
	service := investigation.NewService(workspaceExecutor, resolved.Externals, blob.Store{Root: r.cfg.BlobRoot})
	return investigation.Runtime{Profile: resolved.Profile, Agent: resolved.Execution, Investigation: service}, nil
}

type resolvedBaseRuntime struct {
	Profile   profileapp.Runtime
	Execution core.ExecutionService
	Externals terminal.Externals
	Sandbox   provider.Sandbox
	Ownership func(context.Context, string) (provider.Ownership, error)
}

func (r profileRuntimeResolver) resolveBase(ctx context.Context, name string, requireVerified bool) (resolvedBaseRuntime, error) {
	profile, err := r.store.SandboxProfile(ctx, name)
	if err != nil {
		return resolvedBaseRuntime{}, err
	}
	if requireVerified && !profile.BaseVerified() {
		return resolvedBaseRuntime{}, fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup", profile.Name, core.BaseProfileContract)
	}
	sandbox, err := sandboxForProfile(r.cfg, profile)
	if err != nil {
		return resolvedBaseRuntime{}, err
	}
	var agent terminal.Harness
	switch profile.Harness {
	case codex.Harness:
		agent = codex.Agent{Sandbox: sandbox, Port: r.cfg.AppServerPort, Timeout: r.cfg.TurnTimeout}
	case piagent.Harness:
		agent = piagent.Agent{Sandbox: sandbox, Timeout: r.cfg.TurnTimeout}
	default:
		return resolvedBaseRuntime{}, fmt.Errorf("unsupported Harness %q in Sandbox profile %q", profile.Harness, profile.Name)
	}
	ownership := func(ctx context.Context, sandboxID string) (provider.Ownership, error) {
		owned, err := r.store.Sandbox(ctx, sandboxID)
		if err != nil {
			return provider.Ownership{}, err
		}
		return provider.Ownership{JobID: owned.JobID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce}, nil
	}
	externals := terminal.Externals{
		Sandbox: sandbox, Gateway: gateway.Gateway{StatePath: r.cfg.GatewayStatePath},
		Agent: agent, Ownership: ownership,
	}
	execution := core.NewExecutionService(r.store, externals, r.barrier, absurdruntime.RequireClaim).
		WithAgentStrategies(workflowAgentStrategies{store: r.store, externals: externals})
	return resolvedBaseRuntime{
		Profile:   profileapp.Runtime{SandboxProfile: profile.Name},
		Execution: execution,
		Externals: externals, Sandbox: sandbox, Ownership: ownership,
	}, nil
}

// workflowAgentStrategies is static deployment composition. It selects prompt
// policy and the strict-review Harness adapter owned by the pinned native
// workflow; Core never switches on a workflow or receives caller-built AgentRun
// lifecycle callbacks.
type workflowAgentStrategies struct {
	store     postgres.Store
	externals terminal.Externals
}

func (s workflowAgentStrategies) SelectAgentMessage(ctx context.Context, jobID string) (*core.AgentMessageWork, error) {
	job, err := s.store.Job(ctx, jobID)
	if err != nil {
		return nil, err
	}
	switch {
	case job.Workflow == coding.Workflow && job.WorkflowRevision == coding.WorkflowRevision:
		return coding.SelectAgentMessage(ctx, s.store, jobID)
	case job.Workflow == investigation.Workflow && job.WorkflowRevision == investigation.WorkflowRevision:
		return investigation.SelectAgentMessage(ctx, s.store, jobID)
	default:
		return nil, fmt.Errorf("Job %s has no statically composed Agent Message selector", jobID)
	}
}

func (s workflowAgentStrategies) ResolveAgentPrompt(ctx context.Context, execution core.AgentMessageExecution) (string, error) {
	switch {
	case execution.Job.Workflow == coding.Workflow && execution.Job.WorkflowRevision == coding.WorkflowRevision && execution.AgentRun.Role == "implement":
		if err := s.store.ValidateCodingAgentMessage(ctx, execution); err != nil {
			return "", err
		}
		job, err := s.store.CodingJob(ctx, execution.Job.ID)
		if err != nil {
			return "", err
		}
		return coding.AgentPrompt(job, execution.Message.Input), nil
	case execution.Job.Workflow == investigation.Workflow && execution.Job.WorkflowRevision == investigation.WorkflowRevision && execution.AgentRun.Role == "investigate":
		if err := s.store.ValidateInvestigationAgentMessage(ctx, execution); err != nil {
			return "", err
		}
		source, err := s.store.CodebaseInvestigationSource(ctx, execution.Job.ID)
		if err != nil {
			return "", err
		}
		return investigation.AgentPrompt(source, execution.Message.Input), nil
	default:
		return "", fmt.Errorf("Message %s has no statically composed ordinary Agent prompt", execution.Message.ID)
	}
}

// ResolveAgentHarnessStrategy is shared by ordinary reconciliation and cleanup
// recovery. Cleanup never asks for a prompt, so open-admission eligibility
// remains confined to ResolveAgentPrompt.
func (s workflowAgentStrategies) ResolveAgentHarnessStrategy(ctx context.Context, execution core.AgentMessageExecution) (core.AgentHarnessStrategy, error) {
	switch {
	case execution.Job.Workflow == coding.Workflow && execution.Job.WorkflowRevision == coding.WorkflowRevision && execution.AgentRun.Capability == coding.ReviewReadOnlyCapability:
		strategy, err := coding.NewReviewAgentStrategy(ctx, s.store, s.externals, execution)
		if err != nil {
			return nil, err
		}
		return strategy, nil
	case execution.Job.Workflow == coding.Workflow && execution.Job.WorkflowRevision == coding.WorkflowRevision && execution.AgentRun.Role == "implement":
		return nil, nil
	case execution.Job.Workflow == investigation.Workflow && execution.Job.WorkflowRevision == investigation.WorkflowRevision && execution.AgentRun.Role == "investigate":
		return nil, nil
	default:
		return nil, fmt.Errorf("Message %s has no statically composed Agent Harness strategy", execution.Message.ID)
	}
}

func sandboxForProfile(cfg config.Config, profile core.SandboxProfile) (provider.Sandbox, error) {
	switch profile.Provider {
	case core.SandboxProviderIncus:
		return incus.Adapter{Sandbox: incus.Sandbox{Config: incus.Config{
			Image: profile.Artifact, Network: profile.IncusNetwork, DiskSize: profile.IncusDiskSize,
			Workspace: cfg.Workspace,
		}}}, nil
	case core.SandboxProviderE2B:
		if strings.TrimSpace(cfg.E2BAPIKey) == "" {
			return nil, fmt.Errorf("invalid E2B Sandbox profile %q: E2B_API_KEY is empty", profile.Name)
		}
		adapter := e2b.Adapter{
			Client: e2b.Client{APIKey: cfg.E2BAPIKey},
			Config: e2b.AdapterConfig{
				Template: profile.Artifact, Workspace: cfg.Workspace,
				SandboxTimeout: profile.E2BSandboxTimeout, ProcessTimeout: cfg.TurnTimeout,
				ProviderGatewayURL: profile.E2BGatewayURL, AllowInternet: profile.E2BAllowInternet,
			},
		}
		if err := adapter.Validate(); err != nil {
			return nil, fmt.Errorf("invalid E2B Sandbox profile %q: %w", profile.Name, err)
		}
		return adapter, nil
	default:
		return nil, fmt.Errorf("unsupported Sandbox provider %q in profile %q", profile.Provider, profile.Name)
	}
}
