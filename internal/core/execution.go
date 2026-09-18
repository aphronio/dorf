package core

import "context"

type SessionReconciliationProgress uint8

const (
	// SessionReconciliationIdle means no native work is active.
	SessionReconciliationIdle SessionReconciliationProgress = iota
	// SessionReconciliationPending means native work is active, so the runtime
	// should keep its active poll cadence until a later reconciliation is idle.
	SessionReconciliationPending
	// SessionReconciliationReady means maintenance progressed and the next
	// authoritative selection is immediately eligible for reconciliation.
	SessionReconciliationReady
)

// SessionReconciliation is the runtime-only Core contract for advancing at most
// native execution and lifecycle maintenance for a Session.
type SessionReconciliation interface {
	ReconcileSession(context.Context, string) (SessionReconciliationProgress, error)
}

// SandboxExecution reconciles one stable Sandbox Action through Core custody.
type SandboxExecution interface {
	ExecuteSandboxAction(context.Context, string, string, ActionKind) error
}

// Execution is the Core lifecycle and observation contract used by the runtime.
type Execution interface {
	SandboxExecution
}

// CleanupExecution is the Core capability needed after a client requests cleanup.
type CleanupExecution interface {
	SandboxExecution
	PrepareCleanup(context.Context, string) (Session, []Sandbox, error)
	CompleteCleanup(context.Context, string) error
}
