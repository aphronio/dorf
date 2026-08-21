package core

import "context"

type SandboxActionEffect func(context.Context, Job, Sandbox) error

// AgentReconciliation is the provider-neutral Core contract for reconciling
// one durably admitted Message without exposing its AgentRun lifecycle.
type AgentReconciliation interface {
	ReconcileAgentMessage(context.Context, string, string, string) (MessageResult, error)
}

// SandboxExecution reconciles one already-admitted stable Sandbox Action.
type SandboxExecution interface {
	ExecuteSandboxAction(context.Context, string, string) error
	ExecuteSandboxActionEffect(context.Context, string, string, ActionKind, SandboxActionEffect) error
}

// Execution is the shared in-process Core application contract consumed by
// native workflows. A future transport may expose an earned client resource
// contract over these same lifecycle capabilities without exposing adapters.
type Execution interface {
	AgentReconciliation
	SandboxExecution
}

// CleanupExecution is the smallest Core capability needed after a client or
// workflow requests Job cleanup.
type CleanupExecution interface {
	SandboxExecution
	PrepareCleanup(context.Context, string) (Job, []Sandbox, error)
	CompleteCleanup(context.Context, string) error
}
