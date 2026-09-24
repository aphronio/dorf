package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type workerCheckpoints struct {
	store    postgres.Store
	tasks    *absurd.Client
	cfg      config.Config
	captures persistence.Captures
}

func (w *workerCheckpoints) Close() { w.captures.Close() }

func (w *workerCheckpoints) CheckpointBoundary(ctx context.Context, id string) (persistence.BoundaryObservation, error) {
	var result persistence.BoundaryObservation
	err := w.store.WithSessionFence(ctx, id, func() error {
		session, err := w.store.Session(ctx, id)
		if err != nil {
			return err
		}
		boundary, err := w.store.Boundary(ctx, core.MainSandboxName(id), false)
		if err != nil {
			return err
		}
		native, err := w.store.NativeState(ctx, id)
		if err != nil {
			return err
		}
		result = persistence.BoundaryObservation{SessionID: id, ThreadID: session.ThreadID, AdmissionOpen: session.AdmissionOpen, NativeRevision: native.Revision, PendingInputID: native.PendingInputID, PendingTurnID: native.PendingTurnID, Boundary: boundary}
		return nil
	})
	return result, checkpointOperationError(err)
}

func (w *workerCheckpoints) StartCapture(ctx context.Context, id, key string) (persistence.CaptureAttempt, error) {
	if key != "" {
		previous, err := w.ObserveCapture(ctx, key, "status")
		if err == nil {
			if previous.SessionID != id {
				return persistence.CaptureAttempt{}, persistence.ErrCaptureConflict
			}
			return previous, nil
		}
		if !errors.Is(err, persistence.ErrCaptureNotFound) {
			return persistence.CaptureAttempt{}, err
		}
	}
	boundary, err := w.store.Boundary(ctx, core.MainSandboxName(id), false)
	if err != nil {
		return persistence.CaptureAttempt{}, checkpointOperationError(err)
	}
	resolver := profileRuntimeResolver{cfg: w.cfg, store: w.store, client: w.tasks}
	service, err := resolver.checkpointService(ctx, boundary)
	if err != nil {
		return persistence.CaptureAttempt{}, persistence.ErrCaptureConflict
	}
	service.Claim = func(context.Context) error { return nil }
	return w.captures.Start(id, boundary.SandboxID, service, key)
}

func (w *workerCheckpoints) ObserveCapture(ctx context.Context, id, action string) (persistence.CaptureAttempt, error) {
	saved, err := w.store.CheckpointByID(ctx, id)
	if err == nil {
		return persistence.CaptureAttempt{ID: id, SessionID: saved.SessionID, State: "ready", Boundary: &saved.CaptureBoundary, Checkpoint: &saved}, nil
	}
	if !errors.Is(err, persistence.ErrCheckpointNotFound) {
		return persistence.CaptureAttempt{}, err
	}
	return w.captures.Observe(id, action)
}

func (w *workerCheckpoints) BranchCheckpoint(ctx context.Context, request persistence.BranchRequest) (persistence.BranchReceipt, error) {
	if err := request.Validate(); err != nil {
		return persistence.BranchReceipt{}, controlapi.ErrInvalidInput
	}
	source, err := w.store.Session(ctx, request.SourceSessionID)
	if err != nil {
		return persistence.BranchReceipt{}, checkpointOperationError(err)
	}
	cfg, err := readCheckpointConfig(w.cfg.PersistenceFile)
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	if cfg == nil || !cfg.enabled(source.ProfileRef()) || cfg.ID != request.Repository {
		return persistence.BranchReceipt{}, persistence.ErrCaptureConflict
	}
	resolver := profileRuntimeResolver{cfg: w.cfg, store: w.store, client: w.tasks}
	if _, err := resolver.checkpointDriver(ctx, source.ProfileRef()); err != nil {
		return persistence.BranchReceipt{}, persistence.ErrCaptureConflict
	}
	receipt, err := w.store.RequestCheckpointBranch(ctx, w.tasks.QueueName(), request)
	if err != nil {
		return persistence.BranchReceipt{}, checkpointOperationError(err)
	}
	return w.wakeBranch(ctx, receipt)
}

func (w *workerCheckpoints) ObserveBranch(ctx context.Context, id string, release bool) (persistence.BranchReceipt, error) {
	if !release {
		receipt, err := w.store.CheckpointBranch(ctx, id)
		return receipt, checkpointOperationError(err)
	}
	receipt, err := w.store.ReleaseCheckpointBranch(ctx, w.tasks.QueueName(), id)
	if err != nil {
		return receipt, checkpointOperationError(err)
	}
	return w.wakeBranch(ctx, receipt)
}

func (w *workerCheckpoints) wakeBranch(ctx context.Context, receipt persistence.BranchReceipt) (persistence.BranchReceipt, error) {
	session, err := w.store.Session(ctx, receipt.DestinationSessionID)
	if err == nil {
		err = wakeFailedCheckpointSession(ctx, w.store, w.tasks, session, "branch:"+receipt.ID)
	}
	return receipt, err
}

func checkpointOperationError(err error) error {
	switch {
	case errors.Is(err, postgres.ErrNotFound), errors.Is(err, persistence.ErrCheckpointNotFound):
		return persistence.ErrCaptureNotFound
	case errors.Is(err, persistence.ErrCheckpointIneligible), errors.Is(err, persistence.ErrCheckpointSuperseded):
		return persistence.ErrCaptureConflict
	default:
		return err
	}
}

func (a controlAPISessions) checkpointOperations() (persistence.Operations, error) {
	operations, ok := a.reader.(persistence.Operations)
	if !ok {
		return nil, core.ErrNativeUnavailable
	}
	return operations, nil
}
func (a controlAPISessions) CheckpointBoundary(ctx context.Context, id string) (persistence.BoundaryObservation, error) {
	operations, err := a.checkpointOperations()
	if err != nil {
		return persistence.BoundaryObservation{}, err
	}
	return operations.CheckpointBoundary(ctx, id)
}
func (a controlAPISessions) StartCapture(ctx context.Context, id, key string) (persistence.CaptureAttempt, error) {
	operations, err := a.checkpointOperations()
	if err != nil {
		return persistence.CaptureAttempt{}, err
	}
	return operations.StartCapture(ctx, id, key)
}
func (a controlAPISessions) ObserveCapture(ctx context.Context, id, action string) (persistence.CaptureAttempt, error) {
	operations, err := a.checkpointOperations()
	if err != nil {
		return persistence.CaptureAttempt{}, err
	}
	return operations.ObserveCapture(ctx, id, action)
}
func (a controlAPISessions) BranchCheckpoint(ctx context.Context, request persistence.BranchRequest) (persistence.BranchReceipt, error) {
	operations, err := a.checkpointOperations()
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	return operations.BranchCheckpoint(ctx, request)
}
func (a controlAPISessions) ObserveBranch(ctx context.Context, id string, release bool) (persistence.BranchReceipt, error) {
	operations, err := a.checkpointOperations()
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	return operations.ObserveBranch(ctx, id, release)
}
func (a controlAPISessions) Close() {}

func (a controlAPISessions) createFromCheckpoint(ctx context.Context, clientID, key string, input controlapi.CreateSessionRequest) (controlapi.Session, bool, error) {
	if input.Start != "held" || input.Profile != "" || input.AgentsMD != "" || input.Model != "" || input.Reasoning != "" || input.AIConnection != "" || input.KeepRunning || input.ClientReference != "" {
		return controlapi.Session{}, false, controlapi.ErrInvalidInput
	}
	checkpoint, err := a.store.CheckpointByID(ctx, input.FromCheckpoint)
	if err != nil {
		return controlapi.Session{}, false, checkpointOperationError(err)
	}
	requestID := fmt.Sprintf("restore-%x", sha256.Sum256([]byte(clientID+"\x00"+key)))
	receipt, err := a.BranchCheckpoint(ctx, persistence.BranchRequest{ID: requestID, SourceSessionID: checkpoint.SessionID, Repository: checkpoint.Repository, SnapshotID: checkpoint.SnapshotID})
	if err != nil {
		return controlapi.Session{}, false, err
	}
	session, err := a.Get(ctx, receipt.DestinationSessionID)
	return session, false, err
}
func (a controlAPISessions) ActivateSession(ctx context.Context, id string) (controlapi.Session, error) {
	receipt, found, err := a.store.SessionCheckpointBranch(ctx, id)
	if err != nil {
		return controlapi.Session{}, err
	}
	if !found {
		return controlapi.Session{}, persistence.ErrCaptureConflict
	}
	if _, err = a.ObserveBranch(ctx, receipt.ID, true); err != nil {
		return controlapi.Session{}, err
	}
	return a.Get(ctx, id)
}
