package controlplane

import (
	"context"

	"github.com/aphronio/dorf/internal/spine"
)

// AgentExecution is the provider-neutral Core contract for delivering and
// observing one durably admitted AgentRun.
type AgentExecution interface {
	Deliver(context.Context, spine.Job, spine.Delivery, string) error
	ObserveAgentRun(context.Context, spine.Job, spine.AgentRun) (bool, error)
	ObserveAgentRunTurn(context.Context, spine.Job, spine.AgentRun, string) (spine.HarnessTurn, error)
}

// SandboxExecution reconciles one already-admitted stable Sandbox Action.
type SandboxExecution interface {
	ExecuteSandboxAction(context.Context, spine.Job, spine.Sandbox, spine.Action) error
}

// Execution is the shared in-process Core application contract consumed by
// native workflows. A future transport may expose an earned client resource
// contract over these same lifecycle capabilities without exposing adapters.
type Execution interface {
	AgentExecution
	SandboxExecution
}

// CleanupExecution is the smallest Core capability needed after a client or
// workflow requests Job cleanup.
type CleanupExecution interface {
	SandboxExecution
	PrepareCleanup(context.Context, string) (spine.Job, []spine.Sandbox, error)
}
