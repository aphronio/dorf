package core

import "context"

// AgentExecution is the provider-neutral Core contract for delivering and
// observing one durably admitted AgentRun.
type AgentExecution interface {
	Deliver(context.Context, Job, Delivery, string) error
	ObserveAgentRunTurn(context.Context, Job, AgentRun, string) (HarnessTurn, error)
}

// SandboxExecution reconciles one already-admitted stable Sandbox Action.
type SandboxExecution interface {
	ExecuteSandboxAction(context.Context, Job, Sandbox, Action) error
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
	PrepareCleanup(context.Context, string) (Job, []Sandbox, error)
}
