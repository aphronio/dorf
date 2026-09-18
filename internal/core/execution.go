package core

import "context"

type AgentReconciliationProgress uint8

const (
	// AgentReconciliationIdle means no Message was selected.
	AgentReconciliationIdle AgentReconciliationProgress = iota
	// AgentReconciliationPending means one Message was selected, so the runtime
	// should keep its active poll cadence until a later reconciliation is idle.
	AgentReconciliationPending
	// AgentReconciliationReady means one Message was selected and the next
	// authoritative selection is immediately eligible for reconciliation.
	AgentReconciliationReady
)

// AgentReconciliation is the runtime-only Core contract for advancing at most
// one selected Message for a Session.
type AgentReconciliation interface {
	ReconcileSessionAgent(context.Context, string) (AgentReconciliationProgress, error)
}

// AgentObservation exposes a settled Message result through its exact native binding.
type AgentObservation interface {
	ObserveSettledAgentMessage(context.Context, string, string) (MessageResult, error)
}

// SandboxExecution reconciles one stable Sandbox Action through Core custody.
type SandboxExecution interface {
	ExecuteSandboxAction(context.Context, string, string, ActionKind) error
}

// Execution is the Core lifecycle and observation contract used by the runtime.
type Execution interface {
	AgentObservation
	SandboxExecution
}

// CleanupExecution is the Core capability needed after a client requests cleanup.
type CleanupExecution interface {
	SandboxExecution
	PrepareCleanup(context.Context, string) (Session, []Sandbox, error)
	CompleteCleanup(context.Context, string) error
}
