package core

import (
	"context"
	"errors"
	"testing"
)

type scopedContractStore struct {
	ExecutionStore
	state  *agentRunTestStore
	active *bool
	t      *testing.T
}

func (s scopedContractStore) PrepareAgentRun(ctx context.Context, id, harness, baseline string) error {
	if !*s.active {
		s.t.Fatal("baseline escaped scope")
	}
	return s.state.PrepareAgentRun(ctx, id, harness, baseline)
}
func (s scopedContractStore) BindAgentRun(ctx context.Context, id, harness, thread, turn, outcome string) error {
	if !*s.active {
		s.t.Fatal("binding escaped scope")
	}
	return s.state.BindAgentRun(ctx, id, harness, thread, turn, outcome)
}

type scopedContractOperation struct {
	AgentRunOperation
	active *bool
}

type scopedSteerStore struct {
	ExecutionStore
	run    AgentRun
	active *bool
	t      *testing.T
}

func (s *scopedSteerStore) PrepareAgentRun(_ context.Context, runID, harness, baseline string) error {
	if !*s.active || s.run.ID != runID {
		s.t.Fatal("steer baseline escaped its exact scope")
	}
	s.run.Harness, s.run.BaselineRecorded, s.run.BaselineTurnID, s.run.State = harness, true, baseline, AgentRunSubmitting
	return nil
}

func (s *scopedSteerStore) BindSteer(_ context.Context, runID, turnID, outcome string) error {
	if !*s.active || s.run.ID != runID {
		s.t.Fatal("steer binding escaped its exact scope")
	}
	s.run.TurnID, s.run.TurnOutcome, s.run.State = turnID, outcome, AgentRunActive
	return nil
}

type scopedSteerExternal struct {
	Externals
	active               *bool
	store                *scopedSteerStore
	scopes, historyCalls int
	mutations            int
	t                    *testing.T
}

func (e *scopedSteerExternal) WithSteerScope(ctx context.Context, _ Session, _ Delivery, fn func(context.Context, SteerExternals) error) error {
	e.scopes++
	*e.active = true
	defer func() { *e.active = false }()
	return fn(ctx, e)
}

type scopedSteerBarrier struct {
	FaultBarrier
	active *bool
	store  *scopedSteerStore
	calls  int
	t      *testing.T
}

func (b *scopedSteerBarrier) Reach(_ context.Context, point string, _ Delivery) error {
	if point != BarrierBeforeSubmit || !*b.active || !b.store.run.BaselineRecorded {
		b.t.Fatal("steer crossed its effect fence outside the scoped durable baseline")
	}
	b.calls++
	return nil
}

func (e *scopedSteerExternal) SteerHistory(context.Context, Session, string, string) (HarnessHistory, error) {
	if !*e.active {
		e.t.Fatal("steer history escaped scope")
	}
	e.historyCalls++
	accepted := []string(nil)
	if e.mutations > 0 {
		accepted = []string{e.store.run.ID}
	}
	return HarnessHistory{Harness: "codex", ThreadID: "thread", Turns: []HarnessTurn{{ID: "target", Status: "inProgress", AcceptedMessageIDs: accepted}}}, nil
}

func (e *scopedSteerExternal) AgentSteer(context.Context, Session, Delivery) (string, error) {
	if !*e.active || !e.store.run.BaselineRecorded || e.store.run.BaselineTurnID != "target" {
		e.t.Fatal("steer mutation preceded its durable baseline or escaped scope")
	}
	e.mutations++
	return "", errors.New("lost acknowledgement")
}

func (o scopedContractOperation) WithScope(ctx context.Context, _ AgentRun, fn func(context.Context, AgentRunOperation) error) error {
	*o.active = true
	defer func() { *o.active = false }()
	return fn(ctx, o.AgentRunOperation)
}
func TestOperationScopeContainsBaselineLostAcknowledgementAndBinding(t *testing.T) {
	active := false
	state := &agentRunTestStore{run: AgentRun{ID: "run", State: AgentRunPending, Harness: "codex", ThreadID: "thread"}}
	history := HarnessHistory{Harness: "codex", ThreadID: "thread", Turns: []HarnessTurn{{ID: "baseline", Status: "completed"}}}
	submits := 0
	operation := scopedContractOperation{active: &active, AgentRunOperation: agentRunTestOperation{
		harness: "codex",
		history: func(context.Context, AgentRun) (HarnessHistory, error) {
			if !active {
				t.Fatal("history escaped scope")
			}
			return history, nil
		},
		submit: func(context.Context, AgentRun, string) (HarnessBinding, error) {
			submits++
			if !active || !state.run.BaselineRecorded || state.run.BaselineTurnID != "baseline" {
				t.Fatal("submit preceded durable baseline")
			}
			history.Turns = append(history.Turns, HarnessTurn{ID: "accepted", Status: "completed"})
			return HarnessBinding{}, errors.New("lost acknowledgement")
		},
	}}
	service := NewExecutionService(scopedContractStore{state: state, active: &active, t: t}, nil, nil, allowAgentRunRecord)
	turn, err := service.executeAgentRun(context.Background(), Delivery{AgentRun: state.run}, operation, "follow")
	if err != nil || turn.ID != "accepted" || state.run.TurnID != "accepted" || submits != 1 || active {
		t.Fatalf("scoped contract turn=%#v err=%v submits=%d active=%v", turn, err, submits, active)
	}
}

func TestSteerScopeContainsHistoryBaselineMutationFreshRecoveryAndBinding(t *testing.T) {
	active := false
	store := &scopedSteerStore{active: &active, t: t, run: AgentRun{
		ID: "run", State: AgentRunPending, Harness: "codex", ThreadID: "thread",
		MessageID: "message", SessionID: "session", SandboxID: "sandbox",
	}}
	external := &scopedSteerExternal{active: &active, store: store, t: t}
	barrier := &scopedSteerBarrier{active: &active, store: store, t: t}
	service := NewExecutionService(store, external, barrier, allowAgentRunRecord)
	delivery := Delivery{AgentRun: store.run, Message: Message{
		ID: "message", SessionID: "session", Intent: MessageSteer, TargetTurnID: "target",
	}}
	if err := service.deliver(context.Background(), Session{ID: "session"}, delivery, nil, "steer"); err != nil {
		t.Fatal(err)
	}
	if active || external.scopes != 1 || external.historyCalls != 2 || external.mutations != 1 || barrier.calls != 1 || store.run.TurnID != "target" || store.run.State != AgentRunActive {
		t.Fatalf("active=%v scopes=%d history=%d mutations=%d fences=%d run=%#v", active, external.scopes, external.historyCalls, external.mutations, barrier.calls, store.run)
	}
}
