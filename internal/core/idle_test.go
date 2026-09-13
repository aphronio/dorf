package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

type idleStore struct {
	ExecutionStore
	job         Job
	deliveries  []Delivery
	fenced      bool
	beforeFence func()
}

func (s *idleStore) Job(context.Context, string) (Job, error) {
	if !s.fenced {
		panic("unfenced Job read")
	}
	return s.job, nil
}
func (s *idleStore) SandboxIdleFor(context.Context, string, time.Duration) (bool, error) {
	for _, delivery := range s.deliveries {
		switch delivery.AgentRun.State {
		case AgentRunCompleted, AgentRunFailed, AgentRunInterrupted:
		default:
			return false, nil
		}
	}
	return true, nil
}
func (s *idleStore) Sandboxes(context.Context, string) ([]Sandbox, error) {
	return []Sandbox{{ID: "sandbox", JobID: s.job.ID}}, nil
}
func (s *idleStore) WithJobFence(_ context.Context, _ string, run func() error) error {
	if s.beforeFence != nil {
		s.beforeFence()
	}
	s.fenced = true
	defer func() { s.fenced = false }()
	return run()
}

type idleExternals struct {
	Externals
	store *idleStore
	calls int
	err   error
}

func (e *idleExternals) SandboxPause(context.Context, Job, Sandbox) error {
	if !e.store.fenced {
		panic("unfenced pause")
	}
	e.calls++
	return e.err
}
func TestIdlePauseHonorsDurableWorkAndPolicy(t *testing.T) {
	for _, state := range []AgentRunState{AgentRunCompleted, AgentRunFailed, AgentRunInterrupted, AgentRunActive, AgentRunSubmitting, AgentRunUncertain, "pending"} {
		t.Run(string(state), func(t *testing.T) {
			store := &idleStore{job: Job{ID: "job", AdmissionOpen: true, CleanupState: CleanupPending}, deliveries: []Delivery{{AgentRun: AgentRun{State: state}}}}
			external := &idleExternals{store: store}
			service := NewExecutionService(store, external, nil, nil)
			if err := service.ReconcileIdleSandboxes(context.Background(), "job"); err != nil {
				t.Fatal(err)
			}
			terminal := state == AgentRunCompleted || state == AgentRunFailed || state == AgentRunInterrupted
			if (external.calls == 1) != terminal {
				t.Fatalf("state %s paused %d times", state, external.calls)
			}
		})
	}
	for _, job := range []Job{{ID: "job", KeepRunning: true, AdmissionOpen: true, CleanupState: CleanupPending}, {ID: "job", AdmissionOpen: false, CleanupState: CleanupPending}, {ID: "job", AdmissionOpen: true, CleanupState: CleanupScheduled}} {
		store := &idleStore{job: job}
		external := &idleExternals{store: store}
		if err := NewExecutionService(store, external, nil, nil).ReconcileIdleSandboxes(context.Background(), "job"); err != nil || external.calls != 0 {
			t.Fatalf("unexpected pause: %d %v", external.calls, err)
		}
	}
}
func TestIdleRetryRechecksNewWork(t *testing.T) {
	store := &idleStore{job: Job{ID: "job", AdmissionOpen: true, CleanupState: CleanupPending}}
	external := &idleExternals{store: store, err: errors.New("snapshot busy")}
	service := NewExecutionService(store, external, nil, nil)
	if err := service.ReconcileIdleSandboxes(context.Background(), "job"); err == nil {
		t.Fatal("missing pause error")
	}
	store.beforeFence = func() { store.deliveries = []Delivery{{AgentRun: AgentRun{State: AgentRunActive}}} }
	if err := service.ReconcileIdleSandboxes(context.Background(), "job"); err != nil {
		t.Fatal(err)
	}
	if external.calls != 1 {
		t.Fatal("stale retry paused new work")
	}
}
