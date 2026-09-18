package codex

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
)

// SubmitNative binds and submits on one connection, then hands its subscription
// to the worker. Native turn/start decides whether input starts or steers.
func (a Agent) SubmitNative(ctx context.Context, owner provider.Ownership, session core.Session, event core.NativeEvent, input core.HarnessInput, mutation core.NativeMutation) (core.NativeAcknowledgement, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	ack := core.NativeAcknowledgement{Harness: Harness, ClientID: event.ClientID, ThreadID: session.ThreadID}
	err := a.withSandboxAccess(ctx, owner, func(a Agent) error {
		var instructions *workspaceInstructions
		var err error
		if event.Type != core.InputCancel {
			instructions, err = a.readWorkspaceInstructions(ctx, owner, a.Sandbox.Workspace())
			if err != nil {
				return err
			}
		}
		return a.withServer(ctx, owner, func(p *protocol) error {
			p.instructions = instructions
			p.refreshSkills = event.RefreshSkills
			if ack.ThreadID != "" {
				if err = p.resumeThread(ctx, ack.ThreadID); err != nil {
					if !mutation.Unused {
						return err
					}
					// No input was ever authorized on this binding. Empty native Threads
					// need not survive a restart; replacing this one cannot replay input.
					ack.ThreadID = ""
				}
			}
			if ack.ThreadID == "" {
				if event.Type == core.InputCancel {
					ack.Type = "input.cancelled"
					return nil
				}
				ack.ThreadID, err = p.startThread(ctx, a.Sandbox.Workspace(), session.Model, "danger-full-access")
				if err != nil {
					return err
				}
				if err = mutation.Bind(ctx, ack.ThreadID); err != nil {
					return err
				}
				p.freshThread = true
			}
			p.execution = telemetry.NativeExecution{ID: event.ClientID, SessionID: session.ID, SandboxID: owner.SandboxID, Harness: Harness, ThreadID: ack.ThreadID}
			if event.Type == core.InputCancel {
				return p.cancelNative(ctx, &ack, mutation)
			}
			correlation, err := mutation.Begin(ctx, "")
			if err != nil {
				return err
			}
			p.execution.ID = correlation
			turn, err := p.startTurn(ctx, ack.ThreadID, a.Sandbox.Workspace(), correlation, input, session.Model, session.ReasoningEffort, "danger-full-access")
			if err != nil {
				return fmt.Errorf("%w: %w", core.ErrNativeUnknown, err)
			}
			ack.Type = "input.accepted"
			ack.TurnID = turn.ID
			return nil
		})
	})
	return ack, err
}

func (p *protocol) cancelNative(ctx context.Context, ack *core.NativeAcknowledgement, mutation core.NativeMutation) error {
	turns, err := p.readTurns(ctx, ack.ThreadID)
	if err != nil {
		return err
	}
	for _, turn := range turns {
		if turn.Terminal() {
			continue
		}
		if ack.TurnID != "" {
			return core.ErrNativeUnavailable
		}
		ack.TurnID = turn.ID
	}
	ack.Type = "input.cancelled"
	if ack.TurnID == "" {
		return nil
	}
	if _, err := mutation.Begin(ctx, ack.TurnID); err != nil {
		return err
	}
	// Capture once; no transport retry or successor lookup after dispatch.
	_, err = p.call(ctx, "turn/interrupt", map[string]any{"threadId": ack.ThreadID, "turnId": ack.TurnID})
	if err != nil {
		return fmt.Errorf("%w: %w", core.ErrNativeUnknown, err)
	}
	return nil
}

// Reuse continuous native observations so idle polling does not write native
// history during a backup. A disconnect requires fresh native evidence.
func (a Agent) NativeIdle(ctx context.Context, owner provider.Ownership, threadID string) (bool, error) {
	if a.Observations != nil {
		if snapshot, ok := a.Observations.Replies.Latest(owner, threadID); ok && !snapshot.Gap {
			return snapshot.Complete && terminal(snapshot.Status), nil
		}
	}
	history, err := a.ReadTurns(ctx, owner, threadID)
	if err != nil {
		return false, err
	}
	for _, turn := range history.Turns {
		if !turn.Terminal() {
			return false, nil
		}
	}
	return true, nil
}
