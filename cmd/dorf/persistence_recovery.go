package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/terminal"
)

type checkpointRecovery struct {
	capture   checkpointCapture
	externals terminal.Externals
}

func (r profileRuntimeResolver) checkpointRecovery(ctx context.Context, ref core.SandboxProfileRef) (persistence.RecoveryService, error) {
	driver, err := r.checkpointDriver(ctx, ref)
	if err != nil {
		return persistence.RecoveryService{}, err
	}
	return persistence.RecoveryService{Store: r.store, Driver: driver, Queue: r.client.QueueName(), Claim: absurdruntime.RequireClaim}, nil
}

func (r profileRuntimeResolver) checkpointDriver(ctx context.Context, ref core.SandboxProfileRef) (checkpointRecovery, error) {
	cfg, err := readCheckpointConfig(r.cfg.PersistenceFile)
	if err != nil {
		return checkpointRecovery{}, err
	}
	if cfg == nil || !cfg.enabled(ref) {
		return checkpointRecovery{}, fmt.Errorf("checkpoint recovery profile is not configured")
	}
	profile, err := r.store.SandboxProfileRevision(ctx, ref)
	if err != nil {
		return checkpointRecovery{}, err
	}
	if profile.Harness != codex.Harness || profile.Provider != core.SandboxProviderE2B {
		return checkpointRecovery{}, fmt.Errorf("checkpoint recovery requires the configured Codex E2B profile")
	}
	sandbox, err := sandboxForProfile(r.cfg, profile)
	if err != nil {
		return checkpointRecovery{}, err
	}
	agent := codex.Agent{Sandbox: sandbox, Port: r.cfg.AppServerPort, Timeout: r.cfg.TurnTimeout, Observations: r.observations}
	driver := checkpointRecovery{capture: checkpointCapture{config: *cfg, store: r.store, sandbox: sandbox, agent: agent, emit: r.emit},
		externals: terminal.Externals{Sandbox: sandbox, Agent: agent, Gateway: configuredProviderGateway(r.cfg)}}
	return driver, nil
}

func (d checkpointRecovery) Restore(ctx context.Context, session core.Session, destination core.Sandbox, checkpoint persistence.Checkpoint) (string, error) {
	if checkpoint.Repository != d.capture.config.ID || checkpoint.SessionID != session.ID || checkpoint.SandboxID != destination.ID {
		return "", fmt.Errorf("recovery checkpoint differs from configured repository or owner")
	}
	return d.restoreInto(ctx, destination, destination, checkpoint)
}

// restoreInto is the provider effect shared by source replacement and a new
// Session branch. Only the repository owner varies; the resource being created
// and written always belongs to destination.
func (d checkpointRecovery) restoreInto(ctx context.Context, source, destination core.Sandbox, checkpoint persistence.Checkpoint) (string, error) {
	if checkpoint.Repository != d.capture.config.ID || checkpoint.SessionID != source.SessionID || checkpoint.SandboxID != source.ID ||
		checkpoint.ProfileName == "" || destination.SessionID == "" || source.OwnershipNonce == "" {
		return "", fmt.Errorf("checkpoint restore custody differs")
	}
	owner := checkpointOwner(destination)
	if err := d.capture.sandbox.ReconcileOwnedCreate(ctx, owner); err != nil {
		return "", err
	}
	if _, err := d.capture.restic().RestoreFrom(ctx, owner, checkpointOwner(source), checkpoint.SnapshotID, "/"); err != nil {
		return "", err
	}
	observer, ok := d.capture.sandbox.(provider.StatusObserver)
	if !ok {
		return "", fmt.Errorf("recovery requires an attested provider resource locator")
	}
	status, err := observer.ObserveOwned(ctx, owner)
	if err != nil {
		return "", err
	}
	if status.ProviderID == "" || status.State != "running" {
		return "", fmt.Errorf("restored resource is not running")
	}
	return status.ProviderID, nil
}

func (d checkpointRecovery) VerifyAndRenew(ctx context.Context, session core.Session, destination core.Sandbox, checkpoint persistence.Checkpoint, pkg persistence.EffectivePackage) error {
	if checkpoint.Repository != d.capture.config.ID || checkpoint.SessionID != session.ID {
		return fmt.Errorf("checkpoint recovery custody differs")
	}
	owner := checkpointOwner(destination)
	// A lost verification acknowledgement may have left an authenticated server.
	if err := d.capture.agent.Quiesce(ctx, owner, session.ThreadID); err != nil {
		return err
	}
	if err := d.restorePackage(ctx, owner, pkg); err != nil {
		return err
	}
	route := core.RouteForSandbox(destination)
	if err := d.externals.Gateway.RevokeExact(ctx, "sandbox:"+destination.ID, route.ID); err != nil {
		return err
	}
	if err := d.externals.RouteCreate(ctx, session, destination, route); err != nil {
		return err
	}
	return d.capture.agent.VerifyRetainedThread(ctx, owner, session.ThreadID)
}

func (d checkpointRecovery) restorePackage(ctx context.Context, owner provider.Ownership, pkg persistence.EffectivePackage) error {
	if pkg.UpgradeID == "" {
		return nil
	}
	if !strings.HasPrefix(pkg.PackagePath, "/nix/store/") || pkg.Version == "" {
		return fmt.Errorf("checkpoint package generation is incomplete")
	}
	result, err := d.capture.sandbox.Exec(ctx, owner, nil, "bash", "-c", `set -eu
 dorf-packages stage "$2" >/dev/null
 recipe=$(dirname "$(readlink -f "$(command -v dorf-packages)")")
 test "$(readlink -f "$recipe/generations/$2")" = "$1"
 test "$("$1/bin/codex" --version)" = "codex-cli $2"
 dorf-packages activate "$2" >/dev/null
 test "$(readlink -f /nix/var/nix/profiles/dorf-runner)" = "$1"`, "dorf-checkpoint-package", pkg.PackagePath, pkg.Version)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("exact checkpoint package generation could not be rebuilt")
	}
	return nil
}

func (d checkpointRecovery) DeleteResource(ctx context.Context, owned core.Sandbox) error {
	return d.capture.sandbox.DeleteOwned(ctx, checkpointOwner(owned))
}

func (e checkpointExecution) ReconcileSession(ctx context.Context, sessionID string) (core.SessionReconciliationProgress, error) {
	session, err := e.resolver.store.Session(ctx, sessionID)
	if err != nil {
		return core.SessionReconciliationIdle, err
	}
	recovery, err := e.resolver.checkpointRecovery(ctx, session.ProfileRef())
	if err != nil {
		return core.SessionReconciliationIdle, err
	}
	progressed, err := absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (bool, error) { return recovery.Reconcile(workCtx, sessionID) })
	if err != nil {
		return core.SessionReconciliationIdle, err
	}
	if progressed {
		return core.SessionReconciliationReady, nil
	}
	return e.Execution.ReconcileSession(ctx, sessionID)
}
