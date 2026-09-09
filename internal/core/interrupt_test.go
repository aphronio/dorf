package core

import (
	"context"
	"errors"
	"testing"
)

type interruptTestStore struct {
	ExecutionStore
	bindings []HarnessBinding
}

func (s *interruptTestStore) BindAgentRun(_ context.Context, _, harness, thread, turn, outcome string) error {
	s.bindings = append(s.bindings, HarnessBinding{Harness: harness, ThreadID: thread, Turn: HarnessTurn{ID: turn, Status: outcome}})
	return nil
}

type interruptTestOperation struct {
	AgentRunOperation
	binding HarnessBinding
	calls   int
}

func (o *interruptTestOperation) Interrupt(context.Context, AgentRun) (HarnessBinding, error) {
	o.calls++
	return o.binding, nil
}

func TestInterruptRecordsObservationAndChecksExactBindingAndClaim(t *testing.T) {
	ctx := context.Background()
	run := AgentRun{ID: "run", MessageID: "message", Harness: "codex", ThreadID: "thread", TurnID: "turn", State: AgentRunActive, InterruptRequested: true}
	store := &interruptTestStore{}
	native := &interruptTestOperation{binding: HarnessBinding{Harness: "codex", ThreadID: "thread", Turn: HarnessTurn{ID: "turn", Status: "inProgress"}}}
	service := NewExecutionService(store, nil, nil, allowAgentRunRecord)
	if err := service.interruptAgentMessage(ctx, run, native); err != nil {
		t.Fatal(err)
	}
	if len(store.bindings) != 1 || store.bindings[0].Turn.Status != "inProgress" {
		t.Fatalf("interrupt acknowledgement fabricated completion: %+v", store.bindings)
	}
	native.binding.Turn.Status = "interrupted"
	restarted := NewExecutionService(store, nil, nil, allowAgentRunRecord)
	if err := restarted.interruptAgentMessage(ctx, run, native); err != nil {
		t.Fatal(err)
	}
	if len(store.bindings) != 2 || store.bindings[1].Turn.Status != "interrupted" {
		t.Fatalf("observed interruption was not recorded: %+v", store.bindings)
	}
	native.binding.Turn.ID = "successor"
	if err := restarted.interruptAgentMessage(ctx, run, native); err == nil || len(store.bindings) != 2 {
		t.Fatal("foreign Turn observation changed the bound run")
	}
	calls := native.calls
	lost := errors.New("claim lost")
	stale := NewExecutionService(store, nil, nil, func(context.Context) error { return lost })
	if err := stale.interruptAgentMessage(ctx, run, native); !errors.Is(err, lost) || native.calls != calls {
		t.Fatalf("stale executor touched native execution: calls=%d err=%v", native.calls, err)
	}
}
