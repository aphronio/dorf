package core

import (
	"context"
	"fmt"
	"time"
)

// AgentInterruptOperation returns the observed outcome of the exact bound Turn.
// Accepting a native interrupt is not evidence that the Turn has stopped.
type AgentInterruptOperation interface {
	Interrupt(context.Context, AgentRun) (HarnessBinding, error)
}

func (run AgentRun) hasPendingInterrupt() bool {
	return run.InterruptRequested && (run.State == AgentRunActive || run.State == AgentRunUncertain)
}

func (s ExecutionService) interruptAgentMessage(ctx context.Context, run AgentRun, operation AgentRunOperation) error {
	native, ok := operation.(AgentInterruptOperation)
	if !ok {
		return s.agentRunAttention(ctx, run.ID, "Harness does not support interrupting this Message")
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	binding, err := native.Interrupt(bounded, run)
	if err != nil {
		if recordErr := s.agentRunAttention(ctx, run.ID, "interrupt observation failed: "+err.Error()); recordErr != nil {
			return recordErr
		}
		return err
	}
	if binding.Harness != run.Harness || binding.ThreadID != run.ThreadID || binding.Turn.ID != run.TurnID {
		return fmt.Errorf("interrupt returned a different Harness, Thread, or Turn for Message %s", run.MessageID)
	}
	return s.bindAgentRun(ctx, run.ID, binding.Harness, binding.ThreadID, binding.Turn.ID, binding.Turn.Status)
}
