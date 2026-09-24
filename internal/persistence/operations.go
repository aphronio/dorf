package persistence

import "context"

// BoundaryObservation is a passive cut, not a reservation or saved checkpoint.
type BoundaryObservation struct {
	SessionID      string          `json:"session_id"`
	ThreadID       string          `json:"thread_id"`
	AdmissionOpen  bool            `json:"admission_open"`
	NativeRevision int64           `json:"native_revision"`
	PendingInputID string          `json:"pending_input_id"`
	PendingTurnID  string          `json:"pending_turn_id"`
	Boundary       CaptureBoundary `json:"boundary"`
}

// Operations is the fixed checkpoint capability exposed by the worker.
// It accepts no provider selection, credentials, callback or executable.
type Operations interface {
	CheckpointBoundary(context.Context, string) (BoundaryObservation, error)
	StartCapture(context.Context, string) (CaptureAttempt, error)
	ObserveCapture(context.Context, string, string) (CaptureAttempt, error)
	BranchCheckpoint(context.Context, BranchRequest) (BranchReceipt, error)
	ObserveBranch(context.Context, string, bool) (BranchReceipt, error)
	Close()
}
