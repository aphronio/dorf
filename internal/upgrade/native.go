package upgrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

// NativeDriver consumes an already staged immutable Nix closure. Staging is an
// operator operation; this path never broadens a guest's admitted network policy.
type NativeDriver struct {
	Sandbox      provider.Sandbox
	Checkpointer provider.Checkpointer
	Agent        codex.Agent
	Replace      bool
}

func upgradeOwner(s core.Sandbox) provider.Ownership {
	return provider.Ownership{JobID: s.JobID, SandboxID: s.ID, OwnershipNonce: s.OwnershipNonce}
}
func (d NativeDriver) InspectPackage(ctx context.Context, s core.Sandbox, r Request) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	// The read-only executable version probe also rejects a staged closure whose
	// actual binary does not match the admitted package version.
	result, err := d.Sandbox.Exec(ctx, upgradeOwner(s), nil, "bash", "-c", `set -eu; test -d "$1"; test "$(readlink -f "$1")" = "$1"; test "$("$1/bin/codex" --version)" = "codex-cli $2"; codex --version`, "dorf-upgrade", r.PackagePath, r.Version)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("staged package verification failed")
	}
	version := strings.TrimPrefix(strings.TrimSpace(result.Stdout), "codex-cli ")
	if !packageVersion.MatchString(version) {
		return "", fmt.Errorf("current runner omitted its exact version")
	}
	return version, nil
}
func (d NativeDriver) Quiesce(ctx context.Context, s core.Sandbox, runs []core.AgentRun) error {
	return d.Agent.QuiesceUpgrade(ctx, upgradeOwner(s), runs)
}
func (d NativeDriver) Capture(ctx context.Context, s core.Sandbox, key string) (provider.Checkpoint, error) {
	return d.Checkpointer.CaptureCheckpoint(ctx, upgradeOwner(s), key)
}
func (d NativeDriver) Activate(ctx context.Context, s core.Sandbox, r Request) error {
	if err := d.ready(ctx, s); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	result, err := d.Sandbox.Exec(ctx, upgradeOwner(s), nil, "bash", "-c", `set -eu
 test "$("$1/bin/codex" --version)" = "codex-cli $2"
 /root/.nix-profile/bin/nix-env --profile /nix/var/nix/profiles/dorf-runner --set "$1"
 ln -sfn /nix/var/nix/profiles/dorf-runner/bin/codex /usr/local/bin/.codex-upgrade
 mv -Tf /usr/local/bin/.codex-upgrade /usr/local/bin/codex
 test "$(codex --version)" = "codex-cli $2"`, "dorf-upgrade", r.PackagePath, r.Version)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("package activation did not verify")
	}
	return nil
}
func (d NativeDriver) ReplacesResource() bool { return d.Replace }
func (d NativeDriver) Restore(ctx context.Context, source, destination core.Sandbox, checkpoint provider.Checkpoint) (string, error) {
	// Stop a possibly started new runner before restoring its checkpoint. Its
	// data may no longer be readable, so quiescence here is provider-owned.
	id, err := d.Checkpointer.RestoreCheckpoint(ctx, upgradeOwner(source), upgradeOwner(destination), checkpoint)
	if err != nil {
		return "", err
	}
	return id, d.ready(ctx, destination)
}
func (d NativeDriver) Verify(ctx context.Context, s core.Sandbox, version string, runs []core.AgentRun) error {
	result, err := d.Sandbox.Exec(ctx, upgradeOwner(s), nil, "codex", "--version")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "codex-cli "+version {
		return fmt.Errorf("restored runner version does not match")
	}
	return d.Agent.VerifyUpgrade(ctx, upgradeOwner(s), runs)
}
func (d NativeDriver) DeleteResource(ctx context.Context, s core.Sandbox) error {
	return d.Sandbox.DeleteOwned(ctx, upgradeOwner(s))
}
func (d NativeDriver) DeleteCheckpoint(ctx context.Context, s core.Sandbox, checkpoint provider.Checkpoint) error {
	return d.Checkpointer.DeleteCheckpoint(ctx, upgradeOwner(s), checkpoint)
}

// Provider start acknowledgement precedes guest-agent readiness. Boot latency
// is not an installation failure and must not trigger an unnecessary rollback.
func (d NativeDriver) ready(ctx context.Context, s core.Sandbox) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		result, err := d.Sandbox.Exec(ctx, upgradeOwner(s), nil, "true")
		if err == nil && result.ExitCode == 0 {
			return nil
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("guest did not become ready before upgrade operation")
		case <-timer.C:
		}
	}
}
