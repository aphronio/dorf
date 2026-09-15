package main

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/upgrade"
)

func (r profileRuntimeResolver) upgradeExecution(ctx context.Context, resolved resolvedBaseRuntime) (upgrade.Execution, error) {
	profile, err := r.store.SandboxProfileRevision(ctx, resolved.SandboxProfile)
	if err != nil {
		return upgrade.Execution{}, err
	}
	checkpoints, ok := resolved.Sandbox.(provider.Checkpointer)
	if !ok || profile.Harness != codex.Harness {
		return upgrade.Execution{}, fmt.Errorf("profile does not support verified Codex package upgrades")
	}
	queue := ""
	if r.client != nil {
		queue = r.client.QueueName()
	}
	driver := upgrade.NativeDriver{Sandbox: resolved.Sandbox, Checkpointer: checkpoints,
		Agent:   codex.Agent{Sandbox: resolved.Sandbox, Port: r.cfg.AppServerPort, Timeout: r.cfg.TurnTimeout, Observations: r.observations},
		Replace: profile.Provider == core.SandboxProviderE2B}
	return upgrade.Execution{ExecutionService: resolved.Execution, Upgrades: upgrade.Service{Store: r.store, Driver: driver, Queue: queue, Provider: string(profile.Provider), Claim: absurdruntime.RequireClaim, Emit: r.emit}}, nil
}
