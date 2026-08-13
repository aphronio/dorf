package spine

import (
	"context"
	"fmt"
)

// agentRunContract contains only the harness operations and recovery policy
// needed to execute one durable AgentRun. Prompt construction, workspace
// preparation, and capability enforcement remain at the adapter boundary.
type agentRunStore interface {
	PrepareAgentRun(context.Context, string, string, string) error
	BindAgentRun(context.Context, string, string, string, string, string) error
	UncertainAgentRun(context.Context, string, string) error
}

type agentRunContract struct {
	store        agentRunStore
	reachBarrier func(context.Context, string, Delivery) error
	delivery     Delivery
	run          AgentRun
	harness      string

	submitNew   func(context.Context, AgentRun) (HarnessBinding, error)
	submitBound func(context.Context, AgentRun) (HarnessBinding, error)
	recover     func(context.Context, AgentRun) (HarnessBinding, error)
	history     func(context.Context, AgentRun) (HarnessHistory, error)
	wait        func(context.Context, AgentRun, string) (HarnessBinding, error)

	validateOwner  func(HarnessBinding) error
	beforeRecord   func(context.Context) error
	onReadError    func(context.Context, string, error)
	onSubmitError  func(context.Context, AgentRun, HarnessBinding, error) (HarnessTurn, error)
	onRecoverError func(context.Context, AgentRun, error) error

	bindUnsupportedTurn bool
	label               string
}

func (c agentRunContract) execute(ctx context.Context) (HarnessTurn, error) {
	if c.label == "" {
		c.label = "harness"
	}
	run := c.run
	if run.Harness != "" && c.harness != "" && run.Harness != c.harness {
		reason := fmt.Sprintf("%s AgentRun is bound to harness %s, not %s", c.label, run.Harness, c.harness)
		return HarnessTurn{}, c.markUncertain(ctx, run.ID, reason)
	}
	if run.Harness == "" {
		run.Harness = c.harness
	}
	if run.Harness == "" {
		return HarnessTurn{}, fmt.Errorf("%s AgentRun has no harness", c.label)
	}
	if run.State == AgentRunFailed || run.State == AgentRunInterrupted {
		status := run.TurnOutcome
		if status == "" {
			status = string(run.State)
		}
		return HarnessTurn{ID: run.TurnID, Status: status}, nil
	}
	if run.State == AgentRunCompleted && c.history == nil {
		return HarnessTurn{ID: run.TurnID, Status: run.TurnOutcome}, nil
	}
	if run.State == AgentRunCompleted {
		return c.completedTurn(ctx, run)
	}

	if run.ThreadID == "" {
		preparedBeforeCall := run.BaselineRecorded
		if !run.BaselineRecorded {
			if err := c.beforeDurableWrite(ctx); err != nil {
				return HarnessTurn{}, err
			}
			if err := c.store.PrepareAgentRun(ctx, run.ID, run.Harness, ""); err != nil {
				return HarnessTurn{}, err
			}
			run.BaselineRecorded, run.State = true, AgentRunSubmitting
			c.delivery.AgentRun = run
		} else if run.BaselineTurnID != "" {
			reason := c.label + " AgentRun without a thread has a nonempty turn baseline"
			return HarnessTurn{}, c.markUncertain(ctx, run.ID, reason)
		}
		if preparedBeforeCall && c.recover != nil {
			return c.recoverAndSettle(ctx, run)
		}
		return c.submitAndSettle(ctx, run, c.submitNew)
	}

	return c.inspectAndSettle(ctx, run)
}

func (c agentRunContract) completedTurn(ctx context.Context, run AgentRun) (HarnessTurn, error) {
	history, err := c.history(ctx, run)
	if err != nil {
		c.recordReadError(ctx, run.ID, err)
		return HarnessTurn{}, err
	}
	if err := c.validateHistory(run, history); err != nil {
		return HarnessTurn{}, c.recordUncertainError(ctx, run.ID, err)
	}
	for _, turn := range history.Turns {
		if turn.ID == run.TurnID {
			return turn, nil
		}
	}
	return HarnessTurn{}, fmt.Errorf("terminal %s AgentRun %s output is unavailable", c.label, run.ID)
}

func (c agentRunContract) recoverAndSettle(ctx context.Context, run AgentRun) (HarnessTurn, error) {
	binding, err := c.recover(ctx, run)
	if err != nil {
		if c.onRecoverError != nil {
			return HarnessTurn{}, c.onRecoverError(ctx, run, err)
		}
		c.recordReadError(ctx, run.ID, err)
		return HarnessTurn{}, err
	}
	if binding.Harness == "" && binding.ThreadID == "" && binding.Turn.ID == "" {
		return c.submitAndSettle(ctx, run, c.submitNew)
	}
	if err := c.validateBinding(run, binding, false); err != nil {
		return HarnessTurn{}, c.recordUncertainError(ctx, run.ID, err)
	}
	return c.bindAndSettle(ctx, run, binding)
}

func (c agentRunContract) inspectAndSettle(ctx context.Context, run AgentRun) (HarnessTurn, error) {
	history, err := c.history(ctx, run)
	if err != nil {
		c.recordReadError(ctx, run.ID, err)
		return HarnessTurn{}, err
	}
	if err := c.validateHistory(run, history); err != nil {
		return HarnessTurn{}, c.recordUncertainError(ctx, run.ID, err)
	}
	if !run.BaselineRecorded {
		baseline := ""
		if len(history.Turns) > 0 {
			baseline = history.Turns[len(history.Turns)-1].ID
		}
		if err := c.beforeDurableWrite(ctx); err != nil {
			return HarnessTurn{}, err
		}
		if err := c.store.PrepareAgentRun(ctx, run.ID, run.Harness, baseline); err != nil {
			return HarnessTurn{}, err
		}
		run.BaselineRecorded, run.BaselineTurnID, run.State = true, baseline, AgentRunSubmitting
		c.delivery.AgentRun = run
	}
	reconciliation := ReconcileTurns(run.BaselineRecorded, run.BaselineTurnID, run.TurnID, history.Turns)
	switch reconciliation.Classification {
	case "no-submit":
		if c.submitBound == nil {
			return HarnessTurn{}, fmt.Errorf("%s thread exists but its prepared turn was not submitted", c.label)
		}
		return c.submitAndSettle(ctx, run, c.submitBound)
	case "uncertain":
		if reconciliation.Turn.ID != "" && c.bindUnsupportedTurn {
			binding := HarnessBinding{Harness: history.Harness, ThreadID: history.ThreadID, Turn: reconciliation.Turn, ControllerID: history.ControllerID}
			if err := c.durableBind(ctx, run, binding); err != nil {
				return HarnessTurn{}, err
			}
			return reconciliation.Turn, nil
		}
		return HarnessTurn{}, c.markUncertain(ctx, run.ID, reconciliation.Reason)
	default:
		return c.bindAndSettle(ctx, run, HarnessBinding{Harness: history.Harness, ThreadID: history.ThreadID, Turn: reconciliation.Turn, ControllerID: history.ControllerID})
	}
}

func (c agentRunContract) submitAndSettle(ctx context.Context, run AgentRun, submit func(context.Context, AgentRun) (HarnessBinding, error)) (HarnessTurn, error) {
	if submit == nil {
		return HarnessTurn{}, fmt.Errorf("%s AgentRun has no harness submission operation", c.label)
	}
	if err := c.reach(ctx, BarrierBeforeSubmit); err != nil {
		return HarnessTurn{}, err
	}
	binding, err := submit(ctx, run)
	if err != nil {
		if run.ThreadID != "" && c.history != nil {
			if reconciled, ok, reconcileErr := c.reconcileLostSubmission(ctx, run, err); ok || reconcileErr != nil {
				return reconciled, reconcileErr
			}
		}
		if c.onSubmitError != nil {
			return c.onSubmitError(ctx, run, binding, err)
		}
		return HarnessTurn{}, err
	}
	if err := c.validateBinding(run, binding, run.ThreadID != ""); err != nil {
		return HarnessTurn{}, c.recordUncertainError(ctx, run.ID, err)
	}
	if err := c.reach(ctx, BarrierAfterSubmitBeforeBind); err != nil {
		return HarnessTurn{}, err
	}
	return c.bindAndSettle(ctx, run, binding)
}

func (c agentRunContract) reconcileLostSubmission(ctx context.Context, run AgentRun, submitErr error) (HarnessTurn, bool, error) {
	history, err := c.history(ctx, run)
	if err != nil {
		reason := c.label + " submission is genuinely uncertain: " + submitErr.Error() + "; history inspection failed: " + err.Error()
		return HarnessTurn{}, true, c.markUncertain(ctx, run.ID, reason)
	}
	if err := c.validateHistory(run, history); err != nil {
		return HarnessTurn{}, true, c.recordUncertainError(ctx, run.ID, err)
	}
	reconciliation := ReconcileTurns(true, run.BaselineTurnID, "", history.Turns)
	switch reconciliation.Classification {
	case "no-submit":
		return HarnessTurn{}, false, nil
	case "uncertain":
		if reconciliation.Turn.ID != "" && c.bindUnsupportedTurn {
			if err := c.reach(ctx, BarrierAfterSubmitBeforeBind); err != nil {
				return HarnessTurn{}, true, err
			}
			binding := HarnessBinding{Harness: history.Harness, ThreadID: history.ThreadID, Turn: reconciliation.Turn, ControllerID: history.ControllerID}
			return reconciliation.Turn, true, c.durableBind(ctx, run, binding)
		}
		return HarnessTurn{}, true, c.markUncertain(ctx, run.ID, reconciliation.Reason)
	default:
		if err := c.reach(ctx, BarrierAfterSubmitBeforeBind); err != nil {
			return HarnessTurn{}, true, err
		}
		binding := HarnessBinding{Harness: history.Harness, ThreadID: history.ThreadID, Turn: reconciliation.Turn, ControllerID: history.ControllerID}
		turn, err := c.bindAndSettle(ctx, run, binding)
		return turn, true, err
	}
}

func (c agentRunContract) bindAndSettle(ctx context.Context, run AgentRun, binding HarnessBinding) (HarnessTurn, error) {
	if err := c.durableBind(ctx, run, binding); err != nil {
		return HarnessTurn{}, err
	}
	run.Harness, run.ThreadID, run.TurnID = binding.Harness, binding.ThreadID, binding.Turn.ID
	if terminalHarness(binding.Turn.Status) || !activeHarness(binding.Turn.Status) {
		return binding.Turn, nil
	}
	if err := c.reach(ctx, BarrierHarnessActive); err != nil {
		return HarnessTurn{}, err
	}
	if c.wait == nil {
		return binding.Turn, nil
	}
	waited, err := c.wait(ctx, run, binding.Turn.ID)
	if err != nil {
		c.recordReadError(ctx, run.ID, err)
		return HarnessTurn{}, err
	}
	if err := c.validateBinding(run, waited, true); err != nil {
		return HarnessTurn{}, c.recordUncertainError(ctx, run.ID, err)
	}
	if waited.Turn.ID != binding.Turn.ID {
		reason := fmt.Sprintf("%s wait returned turn %s for bound turn %s", c.label, waited.Turn.ID, binding.Turn.ID)
		return HarnessTurn{}, c.markUncertain(ctx, run.ID, reason)
	}
	if err := c.beforeDurableWrite(ctx); err != nil {
		return HarnessTurn{}, err
	}
	if err := c.store.BindAgentRun(ctx, run.ID, waited.Harness, waited.ThreadID, waited.Turn.ID, waited.Turn.Status); err != nil {
		return HarnessTurn{}, err
	}
	return waited.Turn, nil
}

func (c agentRunContract) durableBind(ctx context.Context, run AgentRun, binding HarnessBinding) error {
	if err := c.beforeDurableWrite(ctx); err != nil {
		return err
	}
	return c.store.BindAgentRun(ctx, run.ID, binding.Harness, binding.ThreadID, binding.Turn.ID, binding.Turn.Status)
}

func (c agentRunContract) validateBinding(run AgentRun, binding HarnessBinding, requireBoundThread bool) error {
	if binding.Turn.ID == "" {
		return fmt.Errorf("%s submission or recovery returned an incomplete harness, thread, or turn binding", c.label)
	}
	return c.validateHarnessIdentity(run, binding, requireBoundThread)
}

func (c agentRunContract) validateHarnessIdentity(run AgentRun, binding HarnessBinding, requireBoundThread bool) error {
	if binding.Harness == "" || binding.ThreadID == "" {
		return fmt.Errorf("%s recovery returned an incomplete harness or thread identity", c.label)
	}
	if requireBoundThread && (binding.Harness != run.Harness || binding.ThreadID != run.ThreadID) {
		return fmt.Errorf("%s recovery returned %s thread %s for bound %s thread %s", c.label, binding.Harness, binding.ThreadID, run.Harness, run.ThreadID)
	}
	if c.validateOwner != nil {
		return c.validateOwner(binding)
	}
	return nil
}

func (c agentRunContract) validateHistory(run AgentRun, history HarnessHistory) error {
	return c.validateHarnessIdentity(run, HarnessBinding{Harness: history.Harness, ThreadID: history.ThreadID, ControllerID: history.ControllerID}, run.ThreadID != "")
}

func (c agentRunContract) beforeDurableWrite(ctx context.Context) error {
	if c.beforeRecord == nil {
		return fmt.Errorf("%s AgentRun has no durable claim check", c.label)
	}
	return c.beforeRecord(ctx)
}

func (c agentRunContract) recordReadError(ctx context.Context, runID string, err error) {
	if c.onReadError != nil {
		c.onReadError(ctx, runID, err)
	}
}

func (c agentRunContract) markUncertain(ctx context.Context, runID, reason string) error {
	if err := c.recordUncertain(ctx, runID, reason); err != nil {
		return err
	}
	return fmt.Errorf("%s reconciliation is uncertain: %s", c.label, reason)
}

func (c agentRunContract) recordUncertain(ctx context.Context, runID, reason string) error {
	if err := c.beforeDurableWrite(ctx); err != nil {
		return err
	}
	return c.store.UncertainAgentRun(ctx, runID, reason)
}

func (c agentRunContract) recordUncertainError(ctx context.Context, runID string, cause error) error {
	if err := c.recordUncertain(ctx, runID, cause.Error()); err != nil {
		return err
	}
	return cause
}

func (c agentRunContract) reach(ctx context.Context, point string) error {
	if c.reachBarrier == nil {
		return nil
	}
	return c.reachBarrier(ctx, point, c.delivery)
}
