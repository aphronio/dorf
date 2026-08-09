package spine

import (
	"context"
	"fmt"
)

// nativeAgentBinding is the complete harness identity needed to durably adopt a
// native turn. OwnerID is optional for harnesses whose Session is the ownership
// boundary and required by adapters (such as review) with a distinct logical
// controller.
type nativeAgentBinding struct {
	OwnerID   string
	SessionID string
	Turn      NativeTurn
}

type nativeAgentHistory struct {
	OwnerID   string
	SessionID string
	Turns     []NativeTurn
}

// nativeAgentRunContract contains only the concrete harness operations and the
// durable boundary variations needed by one AgentRun. Prompt construction,
// workspace preparation, capability checks, and result interpretation remain
// outside this runner.
type nativeAgentRunContract struct {
	service  Service
	delivery Delivery
	run      AgentRun

	submitNew   func(context.Context, AgentRun) (nativeAgentBinding, error)
	submitBound func(context.Context, AgentRun) (nativeAgentBinding, error)
	recover     func(context.Context, AgentRun) (nativeAgentBinding, error)
	history     func(context.Context, AgentRun) (nativeAgentHistory, error)
	wait        func(context.Context, AgentRun, string) (nativeAgentBinding, error)

	validateOwner  func(AgentRun, string, string) error
	bindSession    func(context.Context, nativeAgentBinding) error
	adoptBinding   func(*AgentRun, nativeAgentBinding)
	beforeBind     func(context.Context) error
	onReadError    func(context.Context, string, error)
	onSubmitError  func(context.Context, AgentRun, nativeAgentBinding, error) (NativeTurn, error)
	onRecoverError func(context.Context, AgentRun, error) error

	reconcileSubmitError bool
	bindUnsupportedTurn  bool
	label                string
}

func (c nativeAgentRunContract) execute(ctx context.Context) (NativeTurn, error) {
	if c.label == "" {
		c.label = "native"
	}
	run := c.run
	if run.State == AgentRunFailed || run.State == AgentRunInterrupted {
		return NativeTurn{ID: run.NativeTurnID, Status: string(run.State)}, nil
	}
	if run.State == AgentRunCompleted && c.history == nil {
		return NativeTurn{ID: run.NativeTurnID, Status: string(run.State)}, nil
	}
	if run.State == AgentRunCompleted {
		return c.completedTurn(ctx, run)
	}

	if run.SessionID == "" {
		preparedBeforeCall := run.BaselineRecorded
		if !run.BaselineRecorded {
			if err := c.service.Store.PrepareAgentRun(ctx, run.ID, ""); err != nil {
				return NativeTurn{}, err
			}
			run.BaselineRecorded, run.State = true, AgentRunSubmitting
			c.delivery.AgentRun = run
		} else if run.BaselineTurnID != "" {
			reason := c.label + " AgentRun without a Session has a nonempty native baseline"
			return NativeTurn{}, c.markUncertain(ctx, run.ID, reason)
		}
		if preparedBeforeCall && c.recover != nil {
			return c.recoverAndSettle(ctx, run)
		}
		return c.submitAndSettle(ctx, run, c.submitNew)
	}

	return c.inspectAndSettle(ctx, run)
}

func (c nativeAgentRunContract) completedTurn(ctx context.Context, run AgentRun) (NativeTurn, error) {
	history, err := c.history(ctx, run)
	if err != nil {
		c.recordReadError(ctx, run.ID, err)
		return NativeTurn{}, err
	}
	if err := c.owner(run, history.OwnerID, history.SessionID); err != nil {
		_ = c.service.Store.UncertainAgentRun(ctx, run.ID, err.Error())
		return NativeTurn{}, err
	}
	for _, turn := range history.Turns {
		if turn.ID == run.NativeTurnID {
			return turn, nil
		}
	}
	return NativeTurn{}, fmt.Errorf("terminal %s AgentRun %s native output is unavailable", c.label, run.ID)
}

func (c nativeAgentRunContract) recoverAndSettle(ctx context.Context, run AgentRun) (NativeTurn, error) {
	binding, err := c.recover(ctx, run)
	if err != nil {
		if c.onRecoverError != nil {
			return NativeTurn{}, c.onRecoverError(ctx, run, err)
		}
		c.recordReadError(ctx, run.ID, err)
		return NativeTurn{}, err
	}
	if binding.OwnerID == "" && binding.SessionID == "" && binding.Turn.ID == "" {
		return c.submitAndSettle(ctx, run, c.submitNew)
	}
	if err := c.validateBinding(run, binding, false); err != nil {
		_ = c.service.Store.UncertainAgentRun(ctx, run.ID, err.Error())
		return NativeTurn{}, err
	}
	return c.bindAndSettle(ctx, run, binding, true)
}

func (c nativeAgentRunContract) inspectAndSettle(ctx context.Context, run AgentRun) (NativeTurn, error) {
	history, err := c.history(ctx, run)
	if err != nil {
		c.recordReadError(ctx, run.ID, err)
		return NativeTurn{}, err
	}
	if err := c.owner(run, history.OwnerID, history.SessionID); err != nil {
		_ = c.service.Store.UncertainAgentRun(ctx, run.ID, err.Error())
		return NativeTurn{}, err
	}
	if !run.BaselineRecorded {
		baseline := ""
		if len(history.Turns) > 0 {
			baseline = history.Turns[len(history.Turns)-1].ID
		}
		if err := c.service.Store.PrepareAgentRun(ctx, run.ID, baseline); err != nil {
			return NativeTurn{}, err
		}
		run.BaselineRecorded, run.BaselineTurnID, run.State = true, baseline, AgentRunSubmitting
		c.delivery.AgentRun = run
	}
	reconciliation := ReconcileTurns(run.BaselineRecorded, run.BaselineTurnID, run.NativeTurnID, history.Turns)
	switch reconciliation.Classification {
	case "no-submit":
		if c.submitBound == nil {
			return NativeTurn{}, fmt.Errorf("%s Session exists but its durably prepared turn was not submitted", c.label)
		}
		return c.submitAndSettle(ctx, run, c.submitBound)
	case "uncertain":
		if reconciliation.Turn.ID != "" && c.bindUnsupportedTurn {
			if err := c.durableBind(ctx, run, nativeAgentBinding{OwnerID: history.OwnerID, SessionID: history.SessionID, Turn: reconciliation.Turn}, false); err != nil {
				return NativeTurn{}, err
			}
			return reconciliation.Turn, nil
		}
		return NativeTurn{}, c.markUncertain(ctx, run.ID, reconciliation.Reason)
	default:
		binding := nativeAgentBinding{OwnerID: history.OwnerID, SessionID: history.SessionID, Turn: reconciliation.Turn}
		return c.bindAndSettle(ctx, run, binding, false)
	}
}

func (c nativeAgentRunContract) submitAndSettle(ctx context.Context, run AgentRun, submit func(context.Context, AgentRun) (nativeAgentBinding, error)) (NativeTurn, error) {
	if submit == nil {
		return NativeTurn{}, fmt.Errorf("%s AgentRun has no native submission operation", c.label)
	}
	if err := c.service.reach(ctx, BarrierBeforeSubmit, c.delivery); err != nil {
		return NativeTurn{}, err
	}
	if err := c.service.Store.BeginTurnSubmission(ctx, run.ID); err != nil {
		return NativeTurn{}, err
	}
	binding, err := submit(ctx, run)
	if err != nil {
		if c.reconcileSubmitError && run.SessionID != "" {
			if reconciled, ok, reconcileErr := c.reconcileLostSubmission(ctx, run, err); ok || reconcileErr != nil {
				return reconciled, reconcileErr
			}
		}
		if c.onSubmitError != nil {
			return c.onSubmitError(ctx, run, binding, err)
		}
		return NativeTurn{}, err
	}
	if err := c.validateBinding(run, binding, run.SessionID != ""); err != nil {
		_ = c.service.Store.UncertainAgentRun(ctx, run.ID, err.Error())
		return NativeTurn{}, err
	}
	if err := c.service.reach(ctx, BarrierAfterSubmitBeforeBind, c.delivery); err != nil {
		return NativeTurn{}, err
	}
	return c.bindAndSettle(ctx, run, binding, run.SessionID == "")
}

func (c nativeAgentRunContract) reconcileLostSubmission(ctx context.Context, run AgentRun, submitErr error) (NativeTurn, bool, error) {
	history, err := c.history(ctx, run)
	if err != nil {
		reason := c.label + " submission is genuinely uncertain: " + submitErr.Error() + "; history inspection failed: " + err.Error()
		return NativeTurn{}, true, c.markUncertain(ctx, run.ID, reason)
	}
	if err := c.owner(run, history.OwnerID, history.SessionID); err != nil {
		_ = c.service.Store.UncertainAgentRun(ctx, run.ID, err.Error())
		return NativeTurn{}, true, err
	}
	reconciliation := ReconcileTurns(true, run.BaselineTurnID, "", history.Turns)
	switch reconciliation.Classification {
	case "no-submit":
		return NativeTurn{}, false, nil
	case "uncertain":
		if reconciliation.Turn.ID != "" && c.bindUnsupportedTurn {
			if err := c.service.reach(ctx, BarrierAfterSubmitBeforeBind, c.delivery); err != nil {
				return NativeTurn{}, true, err
			}
			err := c.durableBind(ctx, run, nativeAgentBinding{OwnerID: history.OwnerID, SessionID: history.SessionID, Turn: reconciliation.Turn}, false)
			return reconciliation.Turn, true, err
		}
		return NativeTurn{}, true, c.markUncertain(ctx, run.ID, reconciliation.Reason)
	default:
		if err := c.service.reach(ctx, BarrierAfterSubmitBeforeBind, c.delivery); err != nil {
			return NativeTurn{}, true, err
		}
		turn, err := c.bindAndSettle(ctx, run, nativeAgentBinding{OwnerID: history.OwnerID, SessionID: history.SessionID, Turn: reconciliation.Turn}, false)
		return turn, true, err
	}
}

func (c nativeAgentRunContract) bindAndSettle(ctx context.Context, run AgentRun, binding nativeAgentBinding, bindSession bool) (NativeTurn, error) {
	if err := c.durableBind(ctx, run, binding, bindSession); err != nil {
		return NativeTurn{}, err
	}
	run.SessionID, run.NativeTurnID = binding.SessionID, binding.Turn.ID
	if c.adoptBinding != nil {
		c.adoptBinding(&run, binding)
	}
	if terminalNative(binding.Turn.Status) || c.wait == nil || !activeNative(binding.Turn.Status) {
		return binding.Turn, nil
	}
	if err := c.service.reach(ctx, BarrierNativeActive, c.delivery); err != nil {
		return NativeTurn{}, err
	}
	waited, err := c.wait(ctx, run, binding.Turn.ID)
	if err != nil {
		c.recordReadError(ctx, run.ID, err)
		return NativeTurn{}, err
	}
	if err := c.validateBinding(run, waited, true); err != nil {
		_ = c.service.Store.UncertainAgentRun(ctx, run.ID, err.Error())
		return NativeTurn{}, err
	}
	if waited.Turn.ID != binding.Turn.ID {
		reason := fmt.Sprintf("%s wait returned native turn %s for bound turn %s", c.label, waited.Turn.ID, binding.Turn.ID)
		return NativeTurn{}, c.markUncertain(ctx, run.ID, reason)
	}
	if err := c.beforeDurableBind(ctx); err != nil {
		return NativeTurn{}, err
	}
	if err := c.service.Store.BindNativeTurn(ctx, run.ID, waited.Turn.ID, waited.Turn.Status); err != nil {
		return NativeTurn{}, err
	}
	return waited.Turn, nil
}

func (c nativeAgentRunContract) durableBind(ctx context.Context, run AgentRun, binding nativeAgentBinding, bindSession bool) error {
	if err := c.beforeDurableBind(ctx); err != nil {
		return err
	}
	if bindSession && c.bindSession != nil {
		if err := c.bindSession(ctx, binding); err != nil {
			return err
		}
	}
	return c.service.Store.BindNativeTurn(ctx, run.ID, binding.Turn.ID, binding.Turn.Status)
}

func (c nativeAgentRunContract) validateBinding(run AgentRun, binding nativeAgentBinding, requireBoundSession bool) error {
	if binding.SessionID == "" || binding.Turn.ID == "" {
		return fmt.Errorf("%s native submission or recovery returned an incomplete Session or turn binding", c.label)
	}
	if requireBoundSession && binding.SessionID != run.SessionID {
		return fmt.Errorf("%s native recovery returned Session %s for bound Session %s", c.label, binding.SessionID, run.SessionID)
	}
	return c.owner(run, binding.OwnerID, binding.SessionID)
}

func (c nativeAgentRunContract) owner(run AgentRun, ownerID, sessionID string) error {
	if c.validateOwner == nil {
		return nil
	}
	return c.validateOwner(run, ownerID, sessionID)
}

func (c nativeAgentRunContract) beforeDurableBind(ctx context.Context) error {
	if c.beforeBind == nil {
		return nil
	}
	return c.beforeBind(ctx)
}

func (c nativeAgentRunContract) recordReadError(ctx context.Context, runID string, err error) {
	if c.onReadError != nil {
		c.onReadError(ctx, runID, err)
	}
}

func (c nativeAgentRunContract) markUncertain(ctx context.Context, runID, reason string) error {
	if err := c.service.Store.UncertainAgentRun(ctx, runID, reason); err != nil {
		return err
	}
	return fmt.Errorf("%s native reconciliation is uncertain: %s", c.label, reason)
}
