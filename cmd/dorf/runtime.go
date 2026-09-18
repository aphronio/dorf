package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/incus"
	piagent "github.com/aphronio/dorf/internal/pi"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/aphronio/dorf/internal/terminal"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type profileRuntimeResolver struct {
	store        postgres.Store
	cfg          config.Config
	client       *absurd.Client
	barrier      core.FaultBarrier
	observations *codex.Observations
	emit         func(telemetry.Event)
}

func configuredObservations(ctx context.Context, stderr io.Writer, terminalWake ...func(context.Context, core.NativeTerminalWakeTarget) error) (*codex.Observations, func(telemetry.Event), func()) {
	publisher, err := telemetry.FromEnv(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "Execution diagnostics could not initialize; work remains enabled.")
	}
	var emit func(telemetry.Event)
	if publisher != nil {
		emit = publisher.Emit
	}
	observations := codex.NewObservations(ctx, emit, terminalWake...)
	return observations, emit, func() {
		observations.Close()
		if publisher == nil {
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if publisher.Shutdown(shutdownCtx) != nil {
			fmt.Fprintln(stderr, "Execution diagnostics did not finish exporting before shutdown.")
		}
	}
}

func (r profileRuntimeResolver) ResolveCleanup(ctx context.Context, ref core.SandboxProfileRef) (core.CleanupRuntime, error) {
	resolved, err := r.resolveBase(ctx, ref)
	if err != nil {
		return core.CleanupRuntime{}, err
	}
	profile, err := r.store.SandboxProfileRevision(ctx, ref)
	if err != nil {
		return core.CleanupRuntime{}, err
	}
	if profile.Harness != codex.Harness {
		return core.CleanupRuntime{Execution: resolved.Execution, SandboxProfile: resolved.SandboxProfile}, nil
	}
	execution, err := r.upgradeExecution(ctx, resolved)
	if err != nil {
		return core.CleanupRuntime{}, err
	}
	checkpoint, err := readCheckpointConfig(r.cfg.PersistenceFile)
	if err != nil {
		return core.CleanupRuntime{}, err
	}
	if checkpoint != nil && checkpoint.enabled(ref) {
		return core.CleanupRuntime{Execution: checkpointExecution{Execution: execution, resolver: r}, SandboxProfile: resolved.SandboxProfile}, nil
	}
	return core.CleanupRuntime{Execution: execution, SandboxProfile: resolved.SandboxProfile}, nil
}

func (r profileRuntimeResolver) ResolveSandbox(ctx context.Context, ref core.SandboxProfileRef) (core.SandboxRuntime, error) {
	resolved, err := r.resolveBase(ctx, ref)
	if err != nil {
		return core.SandboxRuntime{}, err
	}
	return core.SandboxRuntime{
		Native:         resolved.Externals,
		Execution:      resolved.Execution,
		Files:          resolved.Externals,
		Commands:       resolved.Externals,
		Status:         resolved.Externals,
		Timeline:       resolved.Externals,
		SandboxProfile: resolved.SandboxProfile,
	}, nil
}

func (r profileRuntimeResolver) ResolveDirect(ctx context.Context, ref core.SandboxProfileRef) (direct.Runtime, error) {
	resolved, err := r.resolveBase(ctx, ref)
	if err != nil {
		return direct.Runtime{}, err
	}
	profile, err := r.store.SandboxProfileRevision(ctx, ref)
	if err != nil {
		return direct.Runtime{}, err
	}
	if profile.Harness != codex.Harness {
		return direct.Runtime{SandboxProfile: resolved.SandboxProfile, Execution: resolved.Execution}, nil
	}
	execution, err := r.upgradeExecution(ctx, resolved)
	if err != nil {
		return direct.Runtime{}, err
	}
	checkpoint, err := readCheckpointConfig(r.cfg.PersistenceFile)
	if err != nil {
		return direct.Runtime{}, err
	}
	if checkpoint != nil && checkpoint.enabled(ref) {
		return direct.Runtime{SandboxProfile: resolved.SandboxProfile, Execution: checkpointExecution{Execution: execution, resolver: r}}, nil
	}
	return direct.Runtime{SandboxProfile: resolved.SandboxProfile, Execution: execution}, nil
}

type resolvedBaseRuntime struct {
	SandboxProfile core.SandboxProfileRef
	Execution      core.ExecutionService
	Externals      terminal.Externals
	Sandbox        provider.Sandbox
	Ownership      func(context.Context, string) (provider.Ownership, error)
}

// Runtime resolution is downstream of Session admission. The Session's immutable
// reference to this definition remains usable while a later verification
// receipt is unsettled or failed; only new admission and default selection
// consult that live eligibility receipt.
func (r profileRuntimeResolver) resolveBase(ctx context.Context, ref core.SandboxProfileRef) (resolvedBaseRuntime, error) {
	profile, err := r.store.SandboxProfileRevision(ctx, ref)
	if err != nil {
		return resolvedBaseRuntime{}, err
	}
	sandbox, err := sandboxForProfile(r.cfg, profile)
	if err != nil {
		return resolvedBaseRuntime{}, err
	}
	var agent terminal.Harness
	switch profile.Harness {
	case codex.Harness:
		agent = codex.Agent{Sandbox: sandbox, Port: r.cfg.AppServerPort, Timeout: r.cfg.TurnTimeout, Observations: r.observations}
	case piagent.Harness:
		agent = piagent.Agent{Sandbox: sandbox}
	default:
		return resolvedBaseRuntime{}, fmt.Errorf("unsupported Harness %q in Sandbox profile %q", profile.Harness, profile.Name)
	}
	ownership := func(ctx context.Context, sandboxID string) (provider.Ownership, error) {
		owned, err := r.store.Sandbox(ctx, sandboxID)
		if err != nil {
			return provider.Ownership{}, err
		}
		return provider.Ownership{SessionID: owned.SessionID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce}, nil
	}
	externals := terminal.Externals{
		Sandbox: sandbox, Gateway: configuredProviderGateway(r.cfg),
		Agent: agent, Ownership: ownership,
	}
	execution := core.NewExecutionService(r.store, externals, r.barrier, absurdruntime.RequireClaim)
	return resolvedBaseRuntime{
		SandboxProfile: profile.Ref(),
		Execution:      execution,
		Externals:      externals, Sandbox: sandbox, Ownership: ownership,
	}, nil
}

func sandboxForProfile(cfg config.Config, profile core.SandboxProfile) (provider.Sandbox, error) {
	switch profile.Provider {
	case core.SandboxProviderIncus:
		if cfg.Incus == nil {
			return nil, fmt.Errorf("invalid Incus Sandbox profile %q: Deployment Incus authority is not configured", profile.Name)
		}
		authorityHash, err := cfg.Incus.AuthorityHash()
		if err != nil {
			return nil, fmt.Errorf("invalid Incus Deployment authority: %w", err)
		}
		if authorityHash != profile.IncusEndpointAuthorityHash {
			return nil, fmt.Errorf("invalid Incus Sandbox profile %q: endpoint authority does not match its verified definition", profile.Name)
		}
		return incus.Adapter{Sandbox: incus.Sandbox{Config: incus.Config{
			Image: profile.Artifact, Network: profile.IncusNetwork, DiskSize: profile.IncusDiskSize,
			Workspace: cfg.Workspace, ProviderGatewayURL: profile.IncusGatewayURL,
			Connection: incus.ConnectionConfig{
				Endpoint: cfg.Incus.Endpoint, Project: profile.IncusProject, StoragePool: profile.IncusStoragePool,
				TLSServerCertificate: cfg.Incus.ServerCertificate,
				TLSClientCertificate: cfg.Incus.ClientCertificate,
				TLSClientKey:         cfg.Incus.ClientPrivateKey,
			},
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
