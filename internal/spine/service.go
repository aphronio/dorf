package spine

import (
	"context"
	"fmt"
)

type Store interface {
	Job(context.Context, string) (Job, error)
	WithJobFence(context.Context, string, func() error) error
	StartRun(context.Context, string) error
	BeginAction(context.Context, string, ActionKind) (Action, error)
	CompleteAction(context.Context, string, Receipt) error
	UncertainAction(context.Context, string) error
	ObserveRun(context.Context, string, Observation) error
	CompleteCleanup(context.Context, string) error
}

// Externals is the fixed effect set for this one concrete terminal. Each method
// must inspect its native authority before creating anything.
type Externals interface {
	SandboxCreate(context.Context, Job, Action) (Receipt, error)
	RepositoryClone(context.Context, Job, Action) (Receipt, error)
	RouteCreate(context.Context, Job, Action) (Receipt, error)
	AgentRun(context.Context, Job, Action, Action) (Receipt, Receipt, error)
	RouteRevoke(context.Context, Job, Action) (Receipt, error)
	SandboxDelete(context.Context, Job, Action) (Receipt, error)
}

type Service struct {
	Store     Store
	Externals Externals
}

func (s Service) Run(ctx context.Context, jobID string) error {
	return s.Store.WithJobFence(ctx, jobID, func() error {
		return s.runFenced(ctx, jobID)
	})
}

func (s Service) runFenced(ctx context.Context, jobID string) error {
	job, err := s.Store.Job(ctx, jobID)
	if err != nil {
		return err
	}
	if err := s.Store.StartRun(ctx, jobID); err != nil {
		return err
	}
	receipts := make(map[ActionKind]Receipt, 5)
	for _, kind := range []ActionKind{
		ActionSandboxCreate,
		ActionRepositoryClone,
		ActionRouteCreate,
	} {
		job, err = s.Store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		receipt, err := s.reconcile(ctx, job, kind)
		if err != nil {
			return fmt.Errorf("reconcile %s: %w", kind, err)
		}
		receipts[kind] = receipt
	}
	job, err = s.Store.Job(ctx, jobID)
	if err != nil {
		return err
	}
	sessionReceipt, turnReceipt, err := s.reconcileAgentRun(ctx, job)
	if err != nil {
		return fmt.Errorf("reconcile AgentRun: %w", err)
	}
	receipts[ActionSessionStart] = sessionReceipt
	receipts[ActionTurnStart] = turnReceipt
	return s.Store.ObserveRun(ctx, job.ID, Observation{
		SessionID:  receipts[ActionSessionStart].ExternalID,
		AgentRunID: ActionID(job.ID, ActionTurnStart),
		TurnID:     receipts[ActionTurnStart].ExternalID,
		Outcome:    receipts[ActionTurnStart].Outcome,
	})
}

func (s Service) reconcileAgentRun(ctx context.Context, job Job) (Receipt, Receipt, error) {
	session, err := s.Store.BeginAction(ctx, job.ID, ActionSessionStart)
	if err != nil {
		return Receipt{}, Receipt{}, err
	}
	turn, err := s.Store.BeginAction(ctx, job.ID, ActionTurnStart)
	if err != nil {
		return Receipt{}, Receipt{}, err
	}
	if session.State == ActionSucceeded && turn.State == ActionSucceeded {
		return Receipt{ExternalID: session.ExternalID, Outcome: session.Outcome}, Receipt{ExternalID: turn.ExternalID, Outcome: turn.Outcome}, nil
	}
	sessionReceipt, turnReceipt, err := s.Externals.AgentRun(ctx, job, session, turn)
	if err != nil {
		if session.State != ActionSucceeded {
			_ = s.Store.UncertainAction(ctx, session.ID)
		}
		if turn.State != ActionSucceeded {
			_ = s.Store.UncertainAction(ctx, turn.ID)
		}
		return Receipt{}, Receipt{}, err
	}
	if session.State != ActionSucceeded {
		if err := s.Store.CompleteAction(ctx, session.ID, sessionReceipt); err != nil {
			return Receipt{}, Receipt{}, err
		}
	}
	if turn.State != ActionSucceeded {
		if err := s.Store.CompleteAction(ctx, turn.ID, turnReceipt); err != nil {
			return Receipt{}, Receipt{}, err
		}
	}
	return sessionReceipt, turnReceipt, nil
}

func (s Service) Cleanup(ctx context.Context, jobID string) error {
	return s.Store.WithJobFence(ctx, jobID, func() error {
		return s.cleanupFenced(ctx, jobID)
	})
}

func (s Service) cleanupFenced(ctx context.Context, jobID string) error {
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
