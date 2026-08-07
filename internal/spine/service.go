package spine

import (
	"context"
	"errors"
	"fmt"
)

type Store interface {
	Job(context.Context, string) (Job, error)
	WithJobFence(context.Context, string, func() error) error
	StartRun(context.Context, string) error
	BeginAction(context.Context, string, ActionKind) (Action, error)
	CompleteAction(context.Context, string, Receipt) error
	UncertainAction(context.Context, string) error
	NextDelivery(context.Context, string, string) (*Delivery, error)
	PrepareAgentRun(context.Context, string, string) error
	BeginTurnSubmission(context.Context, string) error
	BindNativeTurn(context.Context, string, string, string) error
	FailAgentRun(context.Context, string, string) error
	UncertainAgentRun(context.Context, string, string) error
	AgentRunAttention(context.Context, string, string) error
	CompleteCleanup(context.Context, string) error
}

type Externals interface {
	SandboxCreate(context.Context, Job, Action) (Receipt, error)
	RepositoryClone(context.Context, Job, Action) (Receipt, error)
	RouteCreate(context.Context, Job, Action) (Receipt, error)
	AgentSession(context.Context, Job, Action) (Receipt, error)
	AgentTurns(context.Context, Job, string) ([]NativeTurn, error)
	AgentSubmit(context.Context, Job, Delivery) (NativeTurn, error)
	AgentWait(context.Context, Job, string, string) (NativeTurn, error)
	RouteRevoke(context.Context, Job, Action) (Receipt, error)
	SandboxDelete(context.Context, Job, Action) (Receipt, error)
}

type FaultBarrier interface {
	Reach(context.Context, string, Delivery) error
}

const (
	BarrierBeforeSubmit          = "before-submit"
	BarrierAfterSubmitBeforeBind = "after-submit-before-bind"
	BarrierNativeActive          = "native-active"
)

type RunDisposition string

const (
	RunIdle    RunDisposition = "idle"
	RunBlocked RunDisposition = "blocked"
	RunClosed  RunDisposition = "closed"
)

type Service struct {
	Store     Store
	Externals Externals
	Barrier   FaultBarrier
}

func (s Service) RunUntilIdle(ctx context.Context, jobID string) (RunDisposition, error) {
	disposition := RunIdle
	err := s.Store.WithJobFence(ctx, jobID, func() error {
		var err error
		disposition, err = s.runFenced(ctx, jobID)
		return err
	})
	return disposition, err
}

func (s Service) Run(ctx context.Context, jobID string) error {
	_, err := s.RunUntilIdle(ctx, jobID)
	return err
}

func (s Service) runFenced(ctx context.Context, jobID string) (RunDisposition, error) {
	job, err := s.Store.Job(ctx, jobID)
	if err != nil {
		return RunIdle, err
	}
	if !job.AdmissionOpen {
		return RunClosed, nil
	}
	if err := s.Store.StartRun(ctx, jobID); err != nil {
		return RunIdle, err
	}
	for _, kind := range []ActionKind{ActionSandboxCreate, ActionRepositoryClone, ActionRouteCreate} {
		job, err = s.Store.Job(ctx, jobID)
		if err != nil {
			return RunIdle, err
		}
		if _, err := s.reconcile(ctx, job, kind); err != nil {
			return RunIdle, fmt.Errorf("reconcile %s: %w", kind, err)
		}
	}
	job, err = s.Store.Job(ctx, jobID)
	if err != nil {
		return RunIdle, err
	}
	sessionID, err := s.reconcileSession(ctx, job)
	if err != nil {
		return RunIdle, fmt.Errorf("reconcile native Session: %w", err)
	}
	for {
		job, err = s.Store.Job(ctx, jobID)
		if err != nil {
			return RunIdle, err
		}
		if !job.AdmissionOpen {
			return RunClosed, nil
		}
		delivery, err := s.Store.NextDelivery(ctx, jobID, sessionID)
		if err != nil {
			return RunIdle, err
		}
		if delivery == nil {
			return RunIdle, nil
		}
		switch delivery.AgentRun.State {
		case AgentRunFailed, AgentRunInterrupted, AgentRunUncertain:
			return RunBlocked, nil
		}
		if err := s.deliver(ctx, job, *delivery); err != nil {
			return RunIdle, err
		}
	}
}

func (s Service) reconcileSession(ctx context.Context, job Job) (string, error) {
	action, err := s.Store.BeginAction(ctx, job.ID, ActionSessionStart)
	if err != nil {
		return "", err
	}
	if action.State == ActionSucceeded {
		return action.ExternalID, nil
	}
	receipt, err := s.Externals.AgentSession(ctx, job, action)
	if err != nil {
		_ = s.Store.UncertainAction(ctx, action.ID)
		return "", err
	}
	if err := s.Store.CompleteAction(ctx, action.ID, receipt); err != nil {
		return "", err
	}
	return receipt.ExternalID, nil
}

func (s Service) deliver(ctx context.Context, job Job, delivery Delivery) error {
	run := delivery.AgentRun
	turns, err := s.Externals.AgentTurns(ctx, job, run.SessionID)
	if err != nil {
		_ = s.Store.AgentRunAttention(ctx, run.ID, "native Session history is currently unavailable: "+err.Error())
		return err
	}
	if !run.BaselineRecorded {
		baseline := ""
		if len(turns) > 0 {
			baseline = turns[len(turns)-1].ID
		}
		if err := s.Store.PrepareAgentRun(ctx, run.ID, baseline); err != nil {
			return err
		}
		run.BaselineRecorded, run.BaselineTurnID, run.State = true, baseline, AgentRunSubmitting
		delivery.AgentRun = run
	}
	reconciliation := ReconcileTurns(run.BaselineRecorded, run.BaselineTurnID, run.NativeTurnID, turns)
	if reconciliation.Classification == "uncertain" {
		return s.Store.UncertainAgentRun(ctx, run.ID, reconciliation.Reason)
	}
	if reconciliation.Classification == "no-submit" {
		return s.submit(ctx, job, delivery)
	}
	if err := s.Store.BindNativeTurn(ctx, run.ID, reconciliation.Turn.ID, reconciliation.Turn.Status); err != nil {
		return err
	}
	if reconciliation.Classification == "active" {
		if err := s.reach(ctx, BarrierNativeActive, delivery); err != nil {
			return err
		}
		outcome, err := s.Externals.AgentWait(ctx, job, run.SessionID, reconciliation.Turn.ID)
		if err != nil {
			_ = s.Store.AgentRunAttention(ctx, run.ID, "submitted native turn outcome is currently unavailable: "+err.Error())
			return err
		}
		return s.Store.BindNativeTurn(ctx, run.ID, outcome.ID, outcome.Status)
	}
	return nil
}

func (s Service) submit(ctx context.Context, job Job, delivery Delivery) error {
	if err := s.reach(ctx, BarrierBeforeSubmit, delivery); err != nil {
		return err
	}
	if err := s.Store.BeginTurnSubmission(ctx, delivery.AgentRun.ID); err != nil {
		return err
	}
	turn, err := s.Externals.AgentSubmit(ctx, job, delivery)
	if err != nil {
		var definite interface{ DefiniteNoSubmit() bool }
		if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
			return s.Store.FailAgentRun(ctx, delivery.AgentRun.ID, err.Error())
		}
		turns, inspectErr := s.Externals.AgentTurns(ctx, job, delivery.AgentRun.SessionID)
		if inspectErr != nil {
			reason := "native submission is genuinely uncertain: " + err.Error() + "; history inspection failed: " + inspectErr.Error()
			return s.Store.UncertainAgentRun(ctx, delivery.AgentRun.ID, reason)
		}
		reconciliation := ReconcileTurns(true, delivery.AgentRun.BaselineTurnID, "", turns)
		if reconciliation.Classification == "no-submit" {
			return err
		}
		if reconciliation.Classification == "uncertain" {
			return s.Store.UncertainAgentRun(ctx, delivery.AgentRun.ID, reconciliation.Reason)
		}
		turn = reconciliation.Turn
	}
	if err := s.reach(ctx, BarrierAfterSubmitBeforeBind, delivery); err != nil {
		return err
	}
	if err := s.Store.BindNativeTurn(ctx, delivery.AgentRun.ID, turn.ID, turn.Status); err != nil {
		return err
	}
	if terminalNative(turn.Status) {
		return nil
	}
	if err := s.reach(ctx, BarrierNativeActive, delivery); err != nil {
		return err
	}
	outcome, err := s.Externals.AgentWait(ctx, job, delivery.AgentRun.SessionID, turn.ID)
	if err != nil {
		_ = s.Store.AgentRunAttention(ctx, delivery.AgentRun.ID, "submitted native turn outcome is currently unavailable: "+err.Error())
		return err
	}
	return s.Store.BindNativeTurn(ctx, delivery.AgentRun.ID, outcome.ID, outcome.Status)
}

func (s Service) reach(ctx context.Context, point string, delivery Delivery) error {
	if s.Barrier == nil {
		return nil
	}
	return s.Barrier.Reach(ctx, point, delivery)
}

func terminalNative(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}

func (s Service) Cleanup(ctx context.Context, jobID string) error {
	return s.Store.WithJobFence(ctx, jobID, func() error {
		job, err := s.Store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		for _, kind := range []ActionKind{ActionRouteRevoke, ActionSandboxDelete} {
			if _, err := s.reconcile(ctx, job, kind); err != nil {
				return fmt.Errorf("reconcile %s: %w", kind, err)
			}
		}
		return s.Store.CompleteCleanup(ctx, job.ID)
	})
}

func (s Service) reconcile(ctx context.Context, job Job, kind ActionKind) (Receipt, error) {
	action, err := s.Store.BeginAction(ctx, job.ID, kind)
	if err != nil {
		return Receipt{}, err
	}
	if action.State == ActionSucceeded {
		return Receipt{ExternalID: action.ExternalID, Outcome: action.Outcome}, nil
	}
	var receipt Receipt
	switch kind {
	case ActionSandboxCreate:
		receipt, err = s.Externals.SandboxCreate(ctx, job, action)
	case ActionRepositoryClone:
		receipt, err = s.Externals.RepositoryClone(ctx, job, action)
	case ActionRouteCreate:
		receipt, err = s.Externals.RouteCreate(ctx, job, action)
	case ActionRouteRevoke:
		receipt, err = s.Externals.RouteRevoke(ctx, job, action)
	case ActionSandboxDelete:
		receipt, err = s.Externals.SandboxDelete(ctx, job, action)
	default:
		err = fmt.Errorf("unsupported Action kind %q", kind)
	}
	if err != nil {
		_ = s.Store.UncertainAction(ctx, action.ID)
		return Receipt{}, err
	}
	if err := s.Store.CompleteAction(ctx, action.ID, receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}
