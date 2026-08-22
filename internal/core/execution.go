package core

import "context"

type SandboxActionEffect func(context.Context, Job, Sandbox) error

// AgentReconciliation is the runtime-only Core contract for advancing at most
// one workflow-selected Message for a Job. It is deliberately not embedded in
// the workflow execution surface.
type AgentReconciliation interface {
	ReconcileJobAgent(context.Context, string) error
}

// AgentObservation exposes only the settled Message result needed by typed
// workflow evaluation and crash replay.
type AgentObservation interface {
	ObserveSettledAgentMessage(context.Context, string, string) (MessageResult, error)
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
	AgentObservation
	SandboxExecution
}

// CleanupExecution is the smallest Core capability needed after a client or
// workflow requests Job cleanup.
type CleanupExecution interface {
	SandboxExecution
	PrepareCleanup(context.Context, string) (Job, []Sandbox, error)
	CompleteCleanup(context.Context, string) error
}
