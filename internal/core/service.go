package core

import (
	"context"
	"errors"
	"fmt"
	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	"time"
)

type ExecutionStore interface {
	SandboxActivityStore
	SandboxIdleFor(context.Context, string, time.Duration) (bool, error)
	Session(context.Context, string) (Session, error)
	SessionTasks(context.Context, string) ([]SessionTask, error)
	Sandboxes(context.Context, string) ([]Sandbox, error)
	NativeState(context.Context, string) (NativeState, error)
	FinishNativeMutation(context.Context, string, int64) error
	WithSessionFence(context.Context, string, func() error) error
	AuthorizeSandboxAction(context.Context, string, string, string) (SandboxActionAuthorization, error)
	RecordSandboxActionSuccess(context.Context, string) error
	BindSandboxResource(context.Context, Sandbox, string) error
	SetExecutionAttention(context.Context, string, string, string) error
	ClearExecutionAttention(context.Context, string, string) error
	GetOrCreateSandboxAction(context.Context, string, ActionKind) (Action, error)
	CompleteCleanup(context.Context, string, string) error
	SetCleanupAttention(context.Context, string, string) error
}
type Externals interface {
	SandboxCreate(context.Context, Session, Sandbox) (string, error)
	RouteCreate(context.Context, Session, Sandbox, Route) error
	RouteRevoke(context.Context, Session, Sandbox, Route) error
	SandboxDelete(context.Context, Session, Sandbox) error
}
type FaultBarrier interface {
	ReachOperation(context.Context, string, string, string) error
}

const (
	BarrierSandboxCreated = "sandbox-created-before-record"
	BarrierRouteRevoked   = "route-revoked-before-record"
	BarrierSandboxDeleted = "sandbox-deleted-before-record"
)

type ExecutionService struct {
	store      ExecutionStore
	externals  Externals
	barrier    FaultBarrier
	claimCheck func(context.Context) error
}

func NewExecutionService(store ExecutionStore, externals Externals, barrier FaultBarrier, claimCheck func(context.Context) error) ExecutionService {
	return ExecutionService{
		store:      store,
		externals:  externals,
		barrier:    barrier,
		claimCheck: claimCheck,
	}
}

func (s ExecutionService) requireClaim(ctx context.Context) error {
	if s.claimCheck == nil {
		return errors.New("durable executor claim check is not configured")
	}
	return s.claimCheck(ctx)
}

func attentionNeeded(err error) bool {
	var attention interface{ AttentionNeeded() bool }
	return errors.As(err, &attention) && attention.AttentionNeeded()
}

func (s ExecutionService) reachOperation(ctx context.Context, point, sessionID, identity string) error {
	if s.barrier == nil {
		return nil
	}
	return s.barrier.ReachOperation(ctx, point, sessionID, identity)
}

func (s ExecutionService) PrepareCleanup(ctx context.Context, sessionID string) (Session, []Sandbox, error) {
	var session Session
	var sandboxes []Sandbox
	err := s.store.WithSessionFence(ctx, sessionID, func() error {
		if err := s.requireCleanupTask(ctx, sessionID); err != nil {
			return err
		}
		var err error
		session, err = s.store.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		sandboxes, err = s.store.Sandboxes(ctx, sessionID)
		return err
	})
	return session, sandboxes, err
}
func (s ExecutionService) requireCleanupTask(ctx context.Context, sessionID string) error {
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	session, err := exactCurrentAttachedTask(ctx, s.store, sessionID, CleanupTaskName)
	if err != nil {
		return err
	}
	if session.AdmissionOpen || (session.CleanupState != CleanupRequested && session.CleanupState != CleanupScheduled) {
		return fmt.Errorf("cleanup task cannot act before cleanup is requested for Session %s", sessionID)
	}
	return nil
}

// ExecuteSandboxAction reconciles one provider-owned mutation through the
// stable Action and Absurd step identities owned by Core custody.
func (s ExecutionService) ExecuteSandboxAction(ctx context.Context, sessionID, sandboxID string, kind ActionKind) error {
	return s.runSandboxAction(ctx, sessionID, sandboxID, kind, func(ctx context.Context, authorized SandboxActionAuthorization) error {
		switch authorized.Action.Kind {
		case ActionSandboxCreate:
			providerID, err := s.externals.SandboxCreate(ctx, authorized.Session, authorized.Sandbox)
			if err != nil {
				return err
			}
			if err := s.requireClaim(ctx); err != nil {
				return err
			}
			return s.store.BindSandboxResource(ctx, authorized.Sandbox, providerID)
		case ActionRouteCreate:
			return s.externals.RouteCreate(ctx, authorized.Session, authorized.Sandbox, RouteForSandbox(authorized.Sandbox))
		case ActionRouteRevoke:
			return s.externals.RouteRevoke(ctx, authorized.Session, authorized.Sandbox, RouteForSandbox(authorized.Sandbox))
		case ActionSandboxDelete:
			return s.externals.SandboxDelete(ctx, authorized.Session, authorized.Sandbox)
		default:
			return fmt.Errorf("unsupported Sandbox Action kind %q", authorized.Action.Kind)
		}
	})
}

func (s ExecutionService) runSandboxAction(ctx context.Context, sessionID, sandboxID string, kind ActionKind, effect func(context.Context, SandboxActionAuthorization) error) error {
	if sessionID == "" || sandboxID == "" || kind == "" {
		return fmt.Errorf("Sandbox Action requires durable Session, Sandbox, and kind identities")
	}
	actionID := ScopedActionID(sessionID, kind, sandboxID)
	action, err := s.store.GetOrCreateSandboxAction(ctx, sandboxID, kind)
	if err != nil {
		return err
	}
	if action.ID != actionID || action.SessionID != sessionID || action.Kind != kind || action.Scope != sandboxID {
		return fmt.Errorf("Sandbox Action %s changed authoritative identity", actionID)
	}
	if action.State == ActionSucceeded {
		return nil
	}
	return absurdruntime.RunActionStep(ctx, actionID, func(workCtx context.Context) error {
		return s.executeSandboxAction(workCtx, sessionID, actionID, kind, effect)
	})
}

func (s ExecutionService) executeSandboxAction(ctx context.Context, sessionID, actionID string, expectedKind ActionKind, effect func(context.Context, SandboxActionAuthorization) error) error {
	return s.store.WithSessionFence(ctx, sessionID, func() error {
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return absurd.ErrNoTaskContext
		}
		authorized, err := s.store.AuthorizeSandboxAction(ctx, actionID, task.TaskID(), task.TaskName())
		if err != nil {
			return err
		}
		authoritative := authorized.Action
		if authorized.Session.ID != sessionID || authoritative.ID != actionID || authoritative.SessionID != sessionID || authorized.Sandbox.SessionID != sessionID || authoritative.Scope != authorized.Sandbox.ID ||
			authorized.TaskID != task.TaskID() || authorized.TaskName != task.TaskName() {
			return fmt.Errorf("Sandbox Action does not match its authoritative Session, Sandbox, and task")
		}
		if expectedKind != "" && authoritative.Kind != expectedKind {
			return fmt.Errorf("Sandbox Action %s is %s, not expected %s", actionID, authoritative.Kind, expectedKind)
		}
		if authoritative.State == ActionSucceeded {
			return nil
		}
		err = WithSandboxActivity(ctx, s.store, sessionID, func() error { return effect(ctx, authorized) })
		if err != nil {
			if attentionNeeded(err) {
				if claimErr := s.requireClaim(ctx); claimErr != nil {
					return errors.Join(err, claimErr)
				}
				if attentionErr := s.store.SetExecutionAttention(ctx, sessionID, actionID, err.Error()); attentionErr != nil {
					return errors.Join(err, fmt.Errorf("record Sandbox Action attention: %w", attentionErr))
				}
			}
			return err
		}
		point := ""
		switch authoritative.Kind {
		case ActionSandboxCreate:
			point = BarrierSandboxCreated
		case ActionRouteRevoke:
			point = BarrierRouteRevoked
		case ActionSandboxDelete:
			point = BarrierSandboxDeleted
		}
		if point != "" {
			if err := s.reachOperation(ctx, point, authorized.Session.ID, authoritative.ID); err != nil {
				return err
			}
		}
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		if err := s.store.ClearExecutionAttention(ctx, sessionID, actionID); err != nil {
			return err
		}
		return s.store.RecordSandboxActionSuccess(ctx, authoritative.ID)
	})
}

// CompleteCleanup delegates the final locked terminal/mismatch/resource scan
// to the Store after revalidating the exact cleanup task under the Session fence.
func (s ExecutionService) CompleteCleanup(ctx context.Context, sessionID string) error {
	return s.store.WithSessionFence(ctx, sessionID, func() error {
		if err := s.requireCleanupTask(ctx, sessionID); err != nil {
			return err
		}
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return absurd.ErrNoTaskContext
		}
		return s.store.CompleteCleanup(ctx, sessionID, task.TaskID())
	})
}
