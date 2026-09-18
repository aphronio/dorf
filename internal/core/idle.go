package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// SandboxIdleGracePeriod is the existing provider pause grace after activity.
const SandboxIdleGracePeriod = time.Minute

// SandboxIdleReconciliation applies the admitted idle policy. It never changes
// Turn outcomes or requests cleanup. The provider remains the power-state authority.
type SandboxIdleReconciliation interface {
	ReconcileIdleSandboxes(context.Context, string) error
}

// ReconcileIdle retries on the next durable wake instead of failing successful
// work when an optional power-saving operation is temporarily unavailable.
func ReconcileIdle(ctx context.Context, runtime any, sessionID string) {
	idle, ok := runtime.(SandboxIdleReconciliation)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := idle.ReconcileIdleSandboxes(ctx, sessionID); err != nil {
		slog.WarnContext(ctx, "Sandbox idle reconciliation will retry", "session_id", sessionID, "error", err)
	}
}

func (s ExecutionService) ReconcileIdleSandboxes(ctx context.Context, sessionID string) error {
	pauser, ok := s.externals.(interface {
		SandboxPause(context.Context, Session, Sandbox) error
	})
	if !ok {
		return nil
	}
	if sessionID == "" {
		return fmt.Errorf("idle reconciliation requires a Session identity")
	}
	return s.store.WithSessionFence(ctx, sessionID, func() error {
		session, err := s.store.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		if session.ID != sessionID {
			return fmt.Errorf("idle reconciliation changed Session identity")
		}
		if !session.canPause() {
			return nil
		}
		idle, err := s.store.SandboxIdleFor(ctx, sessionID, SandboxIdleGracePeriod)
		if err != nil || !idle {
			return err
		}
		sandboxes, err := s.store.Sandboxes(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, sandbox := range sandboxes {
			if sandbox.SessionID != sessionID {
				return fmt.Errorf("idle Sandbox has a different Session owner")
			}
			idle, err := s.nativeIdle(ctx, session, sandbox)
			if err != nil || !idle {
				return err
			}
			if err := pauser.SandboxPause(ctx, session, sandbox); err != nil {
				return err
			}
		}
		return nil
	})
}

// SandboxActivityStore records access under the caller's Session effect fence.
// A missing finish timestamp starts a fresh grace period after process recovery.
type SandboxActivityStore interface {
	BeginSandboxActivity(context.Context, string) error
	FinishSandboxActivity(context.Context, string) error
}

func WithSandboxActivity(ctx context.Context, store SandboxActivityStore, sessionID string, operation func() error) (err error) {
	if err := store.BeginSandboxActivity(ctx, sessionID); err != nil {
		return err
	}
	defer func() {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, store.FinishSandboxActivity(finishCtx, sessionID))
	}()
	return operation()
}

func (s Session) canPause() bool {
	return !s.KeepRunning && s.AdmissionOpen && s.CleanupState == CleanupPending
}
