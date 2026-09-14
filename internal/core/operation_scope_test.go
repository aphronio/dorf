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
