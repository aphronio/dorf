package codex

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
)

// This provider view models callback-owned access cancellation independently
// from the authenticated native subscription transferred to Observations.
type observationAccessSandbox struct {
	provider.Sandbox
	scopes []context.Context
}

func (s *observationAccessSandbox) WithAccess(ctx context.Context, _ provider.Ownership, fn func(provider.Sandbox) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.scopes = append(s.scopes, ctx)
	return fn(observationAccessView{Sandbox: s.Sandbox, ctx: ctx})
}

type observationAccessView struct {
	provider.Sandbox
	ctx context.Context
}

func (s observationAccessView) Exec(ctx context.Context, owner provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	if err := s.ctx.Err(); err != nil {
		return provider.Result{}, err
	}
	return s.Sandbox.Exec(ctx, owner, input, args...)
}

func (s observationAccessView) ReadFile(ctx context.Context, owner provider.Ownership, name string) ([]byte, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	return s.Sandbox.ReadFile(ctx, owner, name)
}

func (s observationAccessView) Endpoint(ctx context.Context, owner provider.Ownership, port int) (provider.Endpoint, error) {
	if err := s.ctx.Err(); err != nil {
		return provider.Endpoint{}, err
	}
	return s.Sandbox.Endpoint(ctx, owner, port)
}

func TestScopedAccessCompletionPreservesNativeObservationAndInstructions(t *testing.T) {
	f := newInstructionFixture(t)
	f.agent.Observations.Close()
	events := make(chan telemetry.Event, 8)
	f.agent.Observations = NewObservations(context.Background(), func(event telemetry.Event) { events <- event })
	sandbox := &observationAccessSandbox{Sandbox: f.sandbox}
	f.agent.Sandbox = sandbox
	owner := testOwner("scoped-observation")
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	session := &instructionSession{runID: "scoped-run", initial: true, closed: make(chan struct{}), release: release}
	f.sessions <- session
	ctx, cancel := context.WithCancel(telemetry.WithExecution(context.Background(), core.AgentRun{
		ID: session.runID, JobID: owner.JobID, SandboxID: owner.SandboxID, MessageID: "scoped-message",
	}))
	defer cancel()
	binding, err := f.agent.StartInitialTurn(ctx, owner, "/workspace/job", session.runID, core.HarnessInput{Text: "hello"}, "model", "high", false)
	if err != nil || binding.Turn.ID != "native-"+session.runID {
		t.Fatalf("accepted binding=%#v err=%v", binding, err)
	}
	if len(sandbox.scopes) != 1 || !errors.Is(sandbox.scopes[0].Err(), context.Canceled) {
		t.Fatal("native submission did not finish its single provider scope")
	}
	cancel()
	key := observationKey{scope: instructionScope{jobID: owner.JobID, sandboxID: owner.SandboxID, threadID: "retained-thread"}, turnID: binding.Turn.ID}
	observations := f.agent.Observations
	observations.mu.Lock()
	active := observations.active[key]
	before, known := observations.instructions[key.scope]
	observations.mu.Unlock()
	if !active || !known {
		t.Fatal("accepted observation or instructions were lost when the provider scope returned")
	}
	// Only now does the native peer complete the accepted turn: both the scope
	// and submission contexts have already been cancelled.
	releaseOnce.Do(func() { close(release) })
	select {
	case event := <-events:
		if event.Name != "codex.turn/completed" || event.Attributes["native.turn_id"] != binding.Turn.ID || event.Attributes["dorf.agent_run_id"] != session.runID {
			t.Fatalf("lost or misattributed completion: %#v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native observation did not outlive its provider access scope")
	}
	observations.wg.Wait()
	observations.mu.Lock()
	after, retained := observations.instructions[key.scope]
	stillActive := observations.active[key]
	observations.mu.Unlock()
	if stillActive || !retained || before != after {
		t.Fatal("settled observation lost accepted instructions or retained an active subscription")
	}
	select {
	case <-session.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("settled native subscription was not released")
	}
	// A subsequent scope should reuse the accepted unchanged instruction hashes.
	next := &instructionSession{}
	f.submit(t, owner, next, "exact")
	f.requireInjection(t, next, false, false)
	if len(sandbox.scopes) != 2 {
		t.Fatal("follow submission did not use one fresh provider scope")
	}
}
