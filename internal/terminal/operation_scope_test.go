package terminal

import (
	"context"
	"errors"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type mismatchScopeHarness struct {
	Harness
	entered bool
}

type steerScopeHarness struct {
	Harness
	scopes, histories, mutations int
	owner                        provider.Ownership
	thread                       string
}

func (*steerScopeHarness) Name() string { return "selected" }
func (h *steerScopeHarness) WithOperation(ctx context.Context, owner provider.Ownership, thread string, fn func(context.Context, Harness) error) error {
	h.scopes++
	h.owner, h.thread = owner, thread
	return fn(ctx, h)
}
func (h *steerScopeHarness) ReadTurns(_ context.Context, owner provider.Ownership, thread string) (core.HarnessHistory, error) {
	h.histories++
	if owner != h.owner || thread != h.thread {
		return core.HarnessHistory{}, errors.New("history escaped exact steer binding")
	}
	return core.HarnessHistory{Harness: h.Name(), ThreadID: thread, Turns: []core.HarnessTurn{{ID: "turn", Status: "inProgress"}}}, nil
}
func (h *steerScopeHarness) SteerTurn(_ context.Context, owner provider.Ownership, thread, turn, _ string, _ core.HarnessInput) (string, error) {
	h.mutations++
	if owner != h.owner || thread != h.thread || turn != "turn" {
		return "", errors.New("mutation escaped exact steer binding")
	}
	return turn, nil
}

func (*mismatchScopeHarness) Name() string { return "selected" }
func (h *mismatchScopeHarness) WithOperation(context.Context, provider.Ownership, string, func(context.Context, Harness) error) error {
	h.entered = true
	return errors.New("unexpected native scope")
}

func TestMismatchedHarnessReturnsToCoreWithoutAcquiringScope(t *testing.T) {
	harness := &mismatchScopeHarness{}
	operation := AgentRunOperation{messageIntent: core.MessageFollow, externals: Externals{
		Agent: harness,
		Ownership: func(context.Context, string) (provider.Ownership, error) {
			t.Fatal("mismatched Harness acquired ownership")
			return provider.Ownership{}, nil
		},
	}}
	run := core.AgentRun{Harness: "retained-other", State: core.AgentRunPending, ThreadID: "thread"}
	rejection := errors.New("Core rejected conflicting Harness")
	called := false
	err := operation.WithScope(context.Background(), run, func(_ context.Context, bound core.AgentRunOperation) error {
		called = true
		if bound.Harness() != "selected" {
			t.Fatal("scope hid conflicting Harness from Core")
		}
		return rejection
	})
	if !called || !errors.Is(err, rejection) || harness.entered {
		t.Fatalf("callback=%v native scope=%v err=%v", called, harness.entered, err)
	}
}

func TestSteerScopeBindsCopiedExternalsToOneOwnerAndThread(t *testing.T) {
	owner := provider.Ownership{SessionID: "session", SandboxID: "sandbox", OwnershipNonce: "nonce"}
	harness := &steerScopeHarness{}
	ownerReads := 0
	externals := Externals{Agent: harness, Ownership: func(context.Context, string) (provider.Ownership, error) {
		ownerReads++
		return owner, nil
	}}
	session := core.Session{ID: owner.SessionID}
	delivery := core.Delivery{
		AgentRun: core.AgentRun{ID: "run", Harness: harness.Name(), ThreadID: "thread", MessageID: "message", SessionID: session.ID, SandboxID: owner.SandboxID},
		Message:  core.Message{ID: "message", Intent: core.MessageSteer, TargetTurnID: "turn"},
	}
	if err := externals.WithSteerScope(context.Background(), session, delivery, func(ctx context.Context, bound core.SteerExternals) error {
		if _, err := bound.SteerHistory(ctx, session, owner.SandboxID, "thread"); err != nil {
			return err
		}
		_, err := bound.AgentSteer(ctx, session, delivery)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if harness.scopes != 1 || harness.histories != 1 || harness.mutations != 1 || ownerReads != 1 {
		t.Fatalf("scopes=%d histories=%d mutations=%d owner reads=%d", harness.scopes, harness.histories, harness.mutations, ownerReads)
	}
}
