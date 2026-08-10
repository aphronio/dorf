// Package absurdruntime contains the small amount of sequencing policy that
// Dorf deliberately binds to the pinned Absurd runtime.
package absurdruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	HeartbeatInterval = 30 * time.Second
	HeartbeatLease    = 2 * time.Minute
)

// RequireClaim closes the gap between an opaque external operation and the
// Dorf receipt that adopts its result. Call it immediately before recording a
// successful Action, AgentRun, Revision, or Check fact.
func RequireClaim(ctx context.Context) error {
	if err := absurd.Heartbeat(ctx, HeartbeatLease); err != nil {
		return fmt.Errorf("validate claim before recording external result: %w", err)
	}
	return nil
}

// WithHeartbeat keeps the current Absurd claim alive while an opaque external
// operation runs. Cancellation or a lost claim is observed through Heartbeat;
// the derived context then asks the external operation to stop. Stable Dorf
// Action reconciliation still decides whether an already-observed effect may
// be adopted or repeated.
func WithHeartbeat[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := absurd.Heartbeat(ctx, HeartbeatLease); err != nil {
		return zero, fmt.Errorf("heartbeat before opaque work: %w", err)
	}

	workCtx, cancel := context.WithCancel(ctx)
	heartbeatErr := make(chan error, 1)
	done := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(HeartbeatInterval)
		defer ticker.Stop()
		select {
		case <-done:
			return
		case <-workCtx.Done():
			return
		case <-ticker.C:
		}
		for {
			if err := absurd.Heartbeat(ctx, HeartbeatLease); err != nil {
				heartbeatErr <- fmt.Errorf("heartbeat during opaque work: %w", err)
				cancel()
				return
			}
			select {
			case <-done:
				return
			case <-workCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	value, err := fn(workCtx)
	var finalHeartbeatErr error
	if err == nil {
		// Close the window after the last periodic tick. The callback may have
		// reconciled and stored truthful stable Action success; this final
		// check prevents a cancelled or superseded run from committing its
		// Absurd checkpoint as successful orchestration.
		finalHeartbeatErr = absurd.Heartbeat(ctx, HeartbeatLease)
	}
	close(done)
	cancel()
	<-heartbeatDone
	select {
	case heartbeatFailure := <-heartbeatErr:
		return zero, heartbeatFailure
	default:
		if finalHeartbeatErr != nil {
			return zero, fmt.Errorf("heartbeat after opaque work: %w", finalHeartbeatErr)
		}
		return value, err
	}
}
