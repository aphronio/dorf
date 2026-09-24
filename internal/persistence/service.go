package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/telemetry"
)

const DefaultIdleDelay = 5 * time.Second
const boundaryPollInterval = 100 * time.Millisecond

// CaptureStore retains successful recovery facts. Failed attempts belong in
// diagnostics, never in the authoritative checkpoint history.
type CaptureStore interface {
	Boundary(context.Context, string, bool) (CaptureBoundary, error)
	PublishCheckpoint(context.Context, CaptureBoundary, Reference) (Checkpoint, error)
	LastCheckpoint(context.Context, string) (Checkpoint, error)
}

// Capturer owns native readiness, coherent file capture and exact storage
// identity. It must stop remote work with a bounded independent context when
// cancelled; the foreground executor never waits for this method.
type Capturer interface {
	Capture(context.Context, CaptureBoundary) (Reference, error)
}

// GuardedCapturer keeps its native source guard active while pin runs. A
// successful pin is still provisional until the guard and publication succeed.
type GuardedCapturer interface {
	CaptureWithPin(context.Context, CaptureBoundary, func(context.Context, Reference) error) (Reference, error)
}

type Service struct {
	Store     CaptureStore
	Driver    Capturer
	IdleDelay time.Duration
	Claim     func(context.Context) error
	Emit      func(telemetry.Event)
}

// Capture runs on backup capacity, or after cleanup has closed admission. It
// holds no Session fence while reading native files, hashing, uploading or stopping
// a cancelled process. Publication independently rechecks the durable boundary.
func (s Service) Capture(ctx context.Context, sandboxID string, cleanup bool) (Checkpoint, error) {
	return s.capture(ctx, sandboxID, cleanup, nil)
}

// CaptureWithPin calls pin after the immutable upload is available and before
// the native guard ends. The caller must keep any pinned application view
// provisional until this method returns a published checkpoint.
func (s Service) CaptureWithPin(ctx context.Context, sandboxID string, pin func(context.Context, CaptureBoundary, Reference) error) (Checkpoint, error) {
	if pin == nil {
		return Checkpoint{}, fmt.Errorf("checkpoint pin is not configured")
	}
	if _, ok := s.Driver.(GuardedCapturer); !ok {
		return Checkpoint{}, fmt.Errorf("checkpoint driver does not support guarded pinning")
	}
	return s.capture(ctx, sandboxID, false, pin)
}

func (s Service) capture(ctx context.Context, sandboxID string, cleanup bool, pin func(context.Context, CaptureBoundary, Reference) error) (Checkpoint, error) {
	boundary, err := s.Store.Boundary(ctx, sandboxID, cleanup)
	if err != nil {
		return Checkpoint{}, err
	}
	if !boundary.Eligible || (!cleanup && !s.idle(boundary)) {
		s.event(boundary, "skipped", 0)
		return Checkpoint{}, ErrCheckpointIneligible
	}
	if s.Claim == nil || s.Driver == nil {
		return Checkpoint{}, fmt.Errorf("checkpoint execution is not configured")
	}
	if err := s.Claim(ctx); err != nil {
		return Checkpoint{}, err
	}
	started := time.Now()
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	monitorDone := make(chan error, 1)
	go func() { monitorDone <- s.monitor(workCtx, cancel, boundary) }()
	var reference Reference
	var captureErr error
	if pin == nil {
		reference, captureErr = s.Driver.Capture(workCtx, boundary)
	} else {
		reference, captureErr = s.captureWithPin(workCtx, boundary, pin)
	}
	invalidated := workCtx.Err() != nil
	cancel()
	monitorErr := <-monitorDone
	if ctx.Err() != nil {
		s.event(boundary, "cancelled", time.Since(started))
		return Checkpoint{}, errors.Join(ctx.Err(), captureErr)
	}
	if invalidated || monitorErr != nil {
		s.event(boundary, "cancelled", time.Since(started))
		return Checkpoint{}, errors.Join(ErrCheckpointSuperseded, captureErr, monitorErr)
	}
	if captureErr != nil {
		s.event(boundary, "failed", time.Since(started))
		return Checkpoint{}, captureErr
	}
	return s.publish(ctx, boundary, reference, started)
}

func (s Service) captureWithPin(ctx context.Context, boundary CaptureBoundary, pin func(context.Context, CaptureBoundary, Reference) error) (Reference, error) {
	return s.Driver.(GuardedCapturer).CaptureWithPin(ctx, boundary, func(pinCtx context.Context, reference Reference) error {
		if err := s.Claim(pinCtx); err != nil {
			return err
		}
		current, err := s.Store.Boundary(pinCtx, boundary.SandboxID, false)
		if err != nil {
			return err
		}
		if current != boundary || !current.Eligible {
			return ErrCheckpointSuperseded
		}
		return pin(pinCtx, boundary, reference)
	})
}

func (s Service) publish(ctx context.Context, boundary CaptureBoundary, reference Reference, started time.Time) (Checkpoint, error) {
	if err := s.Claim(ctx); err != nil {
		return Checkpoint{}, err
	}
	checkpoint, err := s.Store.PublishCheckpoint(ctx, boundary, reference)
	outcome := "completed"
	if err != nil {
		outcome = "failed"
		if errors.Is(err, ErrCheckpointSuperseded) || errors.Is(err, ErrCheckpointIneligible) {
			outcome = "cancelled"
		}
	}
	s.event(boundary, outcome, time.Since(started))
	return checkpoint, err
}

func (s Service) idle(boundary CaptureBoundary) bool {
	delay := s.IdleDelay
	if delay <= 0 {
		delay = DefaultIdleDelay
	}
	return boundary.NativeRevision > 0 && !boundary.LastActivityAt.IsZero() && time.Since(boundary.LastActivityAt) >= delay
}

func (s Service) monitor(ctx context.Context, cancel context.CancelFunc, expected CaptureBoundary) error {
	ticker := time.NewTicker(boundaryPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			current, err := s.Store.Boundary(ctx, expected.SandboxID, expected.Cleanup)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil || current != expected || !current.Eligible {
				cancel()
				return errors.Join(ErrCheckpointSuperseded, err)
			}
		}
	}
}

func (s Service) event(boundary CaptureBoundary, outcome string, elapsed time.Duration) {
	if s.Emit == nil {
		return
	}
	s.Emit(telemetry.Event{Name: "dorf.checkpoint." + outcome, At: time.Now(), Failed: outcome == "failed", Attributes: map[string]any{
		"dorf.session_id": boundary.SessionID, "dorf.sandbox_id": boundary.SandboxID,
		"dorf.resource_id": boundary.ResourceID, "dorf.checkpoint_outcome": outcome,
		"duration_ms": elapsed.Milliseconds(),
	}})
}
