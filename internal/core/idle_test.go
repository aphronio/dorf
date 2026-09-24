package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

type idleStore struct {
	ExecutionStore
	session     Session
	busy        bool
	fenced      bool
	beforeFence func()
}

func (s *idleStore) Session(context.Context, string) (Session, error) {
	if !s.fenced {
		panic("unfenced Session read")
	}
	return s.session, nil
}
func (s *idleStore) SandboxIdleFor(context.Context, string, time.Duration) (bool, error) {
	return !s.busy, nil
}
func (s *idleStore) Sandboxes(context.Context, string) ([]Sandbox, error) {
	return []Sandbox{{ID: "sandbox", SessionID: s.session.ID}}, nil
}
func (s *idleStore) WithSessionFence(_ context.Context, _ string, run func() error) error {
	if s.beforeFence != nil {
		s.beforeFence()
	}
	s.fenced = true
	defer func() { s.fenced = false }()
	return run()
}

func (s *idleStore) WithSandboxPauseFence(_ context.Context, _ string, run func() error) error {
	return run()
}

type idleExternals struct {
	Externals
	store *idleStore
	calls int
	err   error
}

func (e *idleExternals) SandboxPause(context.Context, Session, Sandbox) error {
	if !e.store.fenced {
		panic("unfenced pause")
	}
	e.calls++
	return e.err
}
func TestIdlePauseHonorsDurableWorkAndPolicy(t *testing.T) {
	for _, session := range []Session{{ID: "session", KeepRunning: true, AdmissionOpen: true, CleanupState: CleanupPending}, {ID: "session", AdmissionOpen: false, CleanupState: CleanupPending}, {ID: "session", AdmissionOpen: true, CleanupState: CleanupScheduled}} {
		store := &idleStore{session: session}
		external := &idleExternals{store: store}
		if err := NewExecutionService(store, external, nil, nil).ReconcileIdleSandboxes(context.Background(), "session"); err != nil || external.calls != 0 {
			t.Fatalf("unexpected pause: %d %v", external.calls, err)
		}
	}
}
func TestIdleRetryRechecksNewWork(t *testing.T) {
	store := &idleStore{session: Session{ID: "session", AdmissionOpen: true, CleanupState: CleanupPending}}
	external := &idleExternals{store: store, err: errors.New("snapshot busy")}
	service := NewExecutionService(store, external, nil, nil)
	if err := service.ReconcileIdleSandboxes(context.Background(), "session"); err == nil {
		t.Fatal("missing pause error")
	}
	store.beforeFence = func() { store.busy = true }
	if err := service.ReconcileIdleSandboxes(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if external.calls != 1 {
		t.Fatal("stale retry paused new work")
	}
}

func (s *idleStore) NativeState(context.Context, string) (NativeState, error) {
	return NativeState{}, nil
}
