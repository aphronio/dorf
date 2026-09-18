package core

import (
	"context"
	"slices"
)

// ReconcileSession observes native work; it never selects or resubmits input.
func (s ExecutionService) ReconcileSession(ctx context.Context, sessionID string) (SessionReconciliationProgress, error) {
	progress := SessionReconciliationIdle
	err := s.store.WithSessionFence(ctx, sessionID, func() error {
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		session, err := exactCurrentAttachedTask(ctx, s.store, sessionID, "")
		if err != nil {
			return err
		}
		if !session.AdmissionOpen || session.CleanupState != CleanupPending {
			return ErrNativeUnavailable
		}
		sandboxes, err := s.store.Sandboxes(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, owned := range sandboxes {
			if owned.Name != DefaultSandbox {
				continue
			}
			idle, err := s.nativeIdle(ctx, session, owned)
			if err != nil {
				return err
			}
			if !idle {
				progress = SessionReconciliationPending
			}
		}
		return nil
	})
	return progress, err
}

func NativeMutationProven(state NativeState, history HarnessHistory) bool {
	for _, turn := range history.Turns {
		if state.PendingInputID != "" && slices.Contains(turn.ClientIDs, state.PendingInputID) {
			return true
		}
		if state.PendingTurnID != "" && turn.ID == state.PendingTurnID && turn.Terminal() {
			return true
		}
	}
	return false
}

func (s ExecutionService) nativeIdle(ctx context.Context, session Session, owned Sandbox) (bool, error) {
	state, err := s.store.NativeState(ctx, session.ID)
	if err != nil {
		return false, err
	}
	if session.ThreadID == "" {
		return state.PendingInputID == "" && state.PendingTurnID == "", nil
	}
	native, ok := s.externals.(NativeSession)
	if !ok {
		return false, ErrNativeUnavailable
	}
	if state.PendingInputID == "" && state.PendingTurnID == "" {
		return native.NativeIdle(ctx, session, owned)
	}
	history, err := native.ReadNativeTurns(ctx, session, owned)
	if err != nil {
		return false, err
	}
	if state.PendingInputID != "" || state.PendingTurnID != "" {
		if !NativeMutationProven(state, history) {
			return false, nil
		}
		if err := s.store.FinishNativeMutation(ctx, session.ID, state.Revision); err != nil {
			return false, err
		}
	}
	for _, turn := range history.Turns {
		if !turn.Terminal() {
			return false, nil
		}
	}
	return true, nil
}
