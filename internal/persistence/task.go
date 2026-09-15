package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const TaskName = "dorf-checkpoint-v1"
const candidatePollInterval = time.Second

type CandidateStore interface {
	CaptureStore
	ListCheckpointCandidates(context.Context, time.Duration) ([]CaptureBoundary, error)
}

type TaskParams struct {
	SandboxID string `json:"sandbox_id"`
}

// Worker runs on its own Absurd queue inside the existing worker process.
// A one-slot foreground worker therefore keeps its full execution capacity.
// Candidate discovery is derived from retained execution and checkpoint facts;
// restart needs neither an in-memory timer nor a second task-status table.
type Worker struct {
	Store     CandidateStore
	Tasks     *absurd.Client
	IdleDelay time.Duration
	Enabled   func(CaptureBoundary) bool
	Resolve   func(context.Context, CaptureBoundary) (Service, error)
}

func (w Worker) Register(lifetime context.Context) {
	w.Tasks.MustRegister(absurd.Task(TaskName, func(ctx context.Context, p TaskParams) (string, error) {
		ctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(lifetime, cancel)
		defer stop()
		defer cancel()
		boundary, err := w.Store.Boundary(ctx, p.SandboxID, false)
		if err != nil {
			return "", err
		}
		if !boundary.Eligible || !w.Enabled(boundary) {
			return "skipped", nil
		}
		previous, err := w.Store.LastCheckpoint(ctx, p.SandboxID)
		if err != nil && !errors.Is(err, ErrCheckpointNotFound) {
			return "", err
		}
		if err == nil && previous.CaptureBoundary == boundary {
			return "already-protected", nil
		}
		service, err := w.Resolve(ctx, boundary)
		if err != nil {
			return "", err
		}
		_, err = absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (Checkpoint, error) { return service.Capture(workCtx, p.SandboxID, false) })
		if errors.Is(err, ErrCheckpointSuperseded) || errors.Is(err, ErrCheckpointIneligible) {
			return "cancelled", nil
		}
		if err != nil {
			return "", fmt.Errorf("checkpoint attempt failed; previous recovery point remains authoritative")
		}
		return "completed", nil
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
}

func (w Worker) Run(ctx context.Context) error {
	if w.IdleDelay <= 0 {
		w.IdleDelay = DefaultIdleDelay
	}
	ticker := time.NewTicker(candidatePollInterval)
	defer ticker.Stop()
	for {
		if err := w.discover(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "Checkpoint discovery will retry")
		}
		// WorkBatch claims one backup at a time. It also services bounded retries
		// already retained by Absurd when no fresh candidate exists.
		if err := w.Tasks.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "checkpoint-worker", ClaimTimeout: absurdruntime.HeartbeatLease, BatchSize: 1}); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "Checkpoint task execution will retry")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w Worker) discover(ctx context.Context) error {
	candidates, err := w.Store.ListCheckpointCandidates(ctx, w.IdleDelay)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if !w.Enabled(candidate) {
			continue
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("checkpoint:%x", sha256.Sum256(encoded))
		if _, err := w.Tasks.Spawn(ctx, TaskName, TaskParams{SandboxID: candidate.SandboxID}, absurdruntime.TaskSpawnOptions(w.Tasks.QueueName(), key)); err != nil {
			return err
		}
	}
	return nil
}
