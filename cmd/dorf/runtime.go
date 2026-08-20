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
	"github.com/aphronio/dorf/internal/controlplane"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/gateway"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/investigation"
	outcomeapp "github.com/aphronio/dorf/internal/outcome"
	piagent "github.com/aphronio/dorf/internal/pi"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/publication"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/terminal"
	"github.com/aphronio/dorf/internal/workflow"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type profileRuntimeResolver struct {
	cfg     config.Config
	store   postgres.Store
	client  *absurd.Client
	barrier spine.FaultBarrier
}

func (r profileRuntimeResolver) ResolveCleanup(ctx context.Context, name string) (controlplane.CleanupRuntime, error) {
	resolved, err := r.resolveBase(ctx, name)
	if err != nil {
		return controlplane.CleanupRuntime{}, err
	}
	return controlplane.CleanupRuntime{
		Execution:      resolved.Execution,
		SandboxProfile: resolved.Profile.SandboxProfile,
	}, nil
}

func (r profileRuntimeResolver) ResolveCoding(ctx context.Context, name string) (workflow.CodingRuntime, error) {
	resolved, err := r.resolveBase(ctx, name)
	if err != nil {
		return workflow.CodingRuntime{}, err
	}
	workspaceExecutor := gitworkspace.NewExecutor(resolved.Execution, r.store, resolved.Externals, absurdruntime.RequireClaim)
	codingService := coding.NewService(workspaceExecutor, r.store, resolved.Externals, blob.Store{Root: r.cfg.BlobRoot}, r.barrier, absurdruntime.RequireClaim)
	githubClient := githubapi.Client{APIURL: r.cfg.GitHubAPIURL, Metadata: r.cfg.GitHubMetadata, PrivateKey: r.cfg.GitHubPrivateKey}
	publicationService := publication.Service{
		Store: r.store, GitHub: githubClient,
		Repository: publication.GitRepository{Sandbox: resolved.Sandbox, Workspace: r.cfg.Workspace, Ownership: resolved.Ownership},
		Evidence:   blob.Store{Root: r.cfg.BlobRoot}, Barrier: r.barrier,
	}
	return workflow.CodingRuntime{
		Profile: resolved.Profile,
		Coding:  codingService,
		Proposal: workflow.ProposalRuntime{
			Publication: publicationService, GitHub: githubClient,
			Outcome: outcomeapp.Service{Store: r.store, GitHub: githubClient},
			Store:   r.store, Client: r.client,
		},
	}, nil
}

func (r profileRuntimeResolver) ResolveInvestigation(ctx context.Context, name string) (workflow.InvestigationRuntime, error) {
	resolved, err := r.resolveBase(ctx, name)
	if err != nil {
		return workflow.InvestigationRuntime{}, err
	}
	workspaceExecutor := gitworkspace.NewExecutor(resolved.Execution, r.store, resolved.Externals, absurdruntime.RequireClaim)
	service := investigation.NewService(workspaceExecutor, r.store, resolved.Externals, blob.Store{Root: r.cfg.BlobRoot}, absurdruntime.RequireClaim)
	return workflow.InvestigationRuntime{Profile: resolved.Profile, Investigation: service}, nil
}

type resolvedBaseRuntime struct {
	Profile   workflow.RuntimeProfile
	Execution spine.ExecutionService
	Externals terminal.Externals
	Sandbox   provider.Sandbox
	Ownership func(context.Context, string) (provider.Ownership, error)
}

func (r profileRuntimeResolver) resolveBase(ctx context.Context, name string) (resolvedBaseRuntime, error) {
	profile, err := r.store.SandboxProfile(ctx, name)
	if err != nil {
		return resolvedBaseRuntime{}, err
	}
	if !profile.BaseVerified() {
		return resolvedBaseRuntime{}, fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup", profile.Name, spine.BaseProfileContract)
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
	execution := spine.NewExecutionService(r.store, externals, blob.Store{Root: r.cfg.BlobRoot}, r.barrier, absurdruntime.RequireClaim)
	return resolvedBaseRuntime{
		Profile:   workflow.RuntimeProfile{SandboxProfile: profile.Name},
		Execution: execution,
		Externals: externals, Sandbox: sandbox, Ownership: ownership,
	}, nil
}

func sandboxForProfile(cfg config.Config, profile spine.SandboxProfile) (provider.Sandbox, error) {
	switch profile.Provider {
	case spine.SandboxProviderIncus:
		return incus.Adapter{Sandbox: incus.Sandbox{Config: incus.Config{
			Image: profile.Artifact, Network: profile.IncusNetwork, DiskSize: profile.IncusDiskSize,
			Workspace: cfg.Workspace,
		}}}, nil
	case spine.SandboxProviderE2B:
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
