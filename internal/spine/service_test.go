package spine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAgentRunRecoversInitialHarnessTurnWithoutResubmission(t *testing.T) {
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", Harness: "codex", State: AgentRunSubmitting, BaselineRecorded: true}}
	submits := 0
	contract := agentRunContract{
		service: Service{Store: store},
		run:     store.run,
		harness: "codex",
		submitNew: func(context.Context, AgentRun) (HarnessBinding, error) {
			submits++
			return HarnessBinding{}, nil
		},
		recover: func(context.Context, AgentRun) (HarnessBinding, error) {
			return HarnessBinding{Harness: "codex", ThreadID: "thread-1", Turn: HarnessTurn{ID: "turn-1", Status: "completed"}}, nil
		},
	}

	turn, err := contract.execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if submits != 0 || turn.ID != "turn-1" {
		t.Fatalf("recovery submitted %d turns and returned %#v", submits, turn)
	}
	if got := store.run; got.Harness != "codex" || got.ThreadID != "thread-1" || got.TurnID != "turn-1" || got.TurnOutcome != "completed" || got.State != AgentRunCompleted {
		t.Fatalf("durable AgentRun binding = %#v", got)
	}
}

func TestAgentRunPersistsHarnessBeforeFirstSubmission(t *testing.T) {
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", State: AgentRunPending}}
	contract := agentRunContract{
		service: Service{Store: store},
		run:     store.run,
		harness: "codex",
		submitNew: func(context.Context, AgentRun) (HarnessBinding, error) {
			if store.run.Harness != "codex" || !store.run.BaselineRecorded || store.run.State != AgentRunSubmitting {
				t.Fatalf("submission began before harness preparation: %#v", store.run)
			}
			return HarnessBinding{Harness: "codex", ThreadID: "thread-1", Turn: HarnessTurn{ID: "turn-1", Status: "completed"}}, nil
		},
	}
	if _, err := contract.execute(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRunReconcilesLostBoundSubmissionAcknowledgement(t *testing.T) {
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", Harness: "codex", ThreadID: "thread-1", State: AgentRunSubmitting, BaselineRecorded: true, BaselineTurnID: "before"}}
	history := HarnessHistory{Harness: "codex", ThreadID: "thread-1", Turns: []HarnessTurn{{ID: "before", Status: "completed"}}}
	contract := agentRunContract{
		service: Service{Store: store},
		run:     store.run,
		submitBound: func(context.Context, AgentRun) (HarnessBinding, error) {
			history.Turns = append(history.Turns, HarnessTurn{ID: "turn-1", Status: "completed"})
			return HarnessBinding{}, errors.New("response lost")
		},
		history: func(context.Context, AgentRun) (HarnessHistory, error) { return history, nil },
	}

	turn, err := contract.execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if turn.ID != "turn-1" || store.run.TurnID != "turn-1" || store.run.State != AgentRunCompleted {
		t.Fatalf("lost acknowledgement did not reconcile: turn=%#v run=%#v", turn, store.run)
	}
}

func TestAgentRunClaimLossAfterSubmissionDoesNotDurablyBind(t *testing.T) {
	claimLost := errors.New("claim lost")
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", Harness: "codex", ThreadID: "thread-1", State: AgentRunPending}}
	contract := agentRunContract{
		service: Service{Store: store},
		run:     store.run,
		submitBound: func(context.Context, AgentRun) (HarnessBinding, error) {
			return HarnessBinding{Harness: "codex", ThreadID: "thread-1", Turn: HarnessTurn{ID: "turn-1", Status: "running"}}, nil
		},
		history: func(context.Context, AgentRun) (HarnessHistory, error) {
			return HarnessHistory{Harness: "codex", ThreadID: "thread-1"}, nil
		},
		beforeBind: func(context.Context) error { return claimLost },
	}

	_, err := contract.execute(context.Background())
	if !errors.Is(err, claimLost) {
		t.Fatalf("execute error = %v", err)
	}
	if store.run.TurnID != "" || store.run.State != AgentRunSubmitting {
		t.Fatalf("claim loss durably bound submitted turn: %#v", store.run)
	}
}

func TestFailedAgentRunWithoutTurnRemainsTerminal(t *testing.T) {
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", Harness: "codex", State: AgentRunFailed, Attention: "proved no submission"}}
	turn, err := (agentRunContract{service: Service{Store: store}, run: store.run}).execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if turn.ID != "" || turn.Status != "failed" || !terminalHarness(turn.Status) {
		t.Fatalf("failed no-submit run was not terminal: %#v", turn)
	}
}

func TestHarnessTurnReconciliationClassifications(t *testing.T) {
	tests := []struct {
		name             string
		baselineRecorded bool
		baseline         string
		known            string
		turns            []HarnessTurn
		want             string
	}{
		{"prepared but not submitted", true, "before", "", []HarnessTurn{{ID: "before", Status: "completed"}}, "no-submit"},
		{"submitted active", true, "before", "", []HarnessTurn{{ID: "before", Status: "completed"}, {ID: "turn", Status: "inProgress"}}, "active"},
		{"submitted complete", true, "before", "", []HarnessTurn{{ID: "before", Status: "completed"}, {ID: "turn", Status: "completed"}}, "completed"},
		{"known failed", true, "", "turn", []HarnessTurn{{ID: "turn", Status: "failed"}}, "failed"},
		{"ambiguous suffix", true, "", "", []HarnessTurn{{ID: "a", Status: "completed"}, {ID: "b", Status: "completed"}}, "uncertain"},
		{"missing baseline", true, "gone", "", nil, "uncertain"},
		{"not prepared", false, "", "", nil, "uncertain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ReconcileTurns(test.baselineRecorded, test.baseline, test.known, test.turns)
			if got.Classification != test.want {
				t.Fatalf("classification = %q (%s), want %q", got.Classification, got.Reason, test.want)
			}
		})
	}
}

type agentRunTestStore struct {
	run AgentRun
}

func (s *agentRunTestStore) Job(context.Context, string) (Job, error) {
	return Job{}, errors.New("unused")
}
func (s *agentRunTestStore) WithJobFence(context.Context, string, func() error) error {
	return errors.New("unused")
}
func (s *agentRunTestStore) Sandbox(context.Context, string) (Sandbox, error) {
	return Sandbox{}, errors.New("unused")
}
func (s *agentRunTestStore) Sandboxes(context.Context, string) ([]Sandbox, error) {
	return nil, errors.New("unused")
}
func (s *agentRunTestStore) AgentRuns(context.Context, string) ([]AgentRun, error) {
	return nil, errors.New("unused")
}
func (s *agentRunTestStore) GetOrCreateSandboxAction(context.Context, string, ActionKind) (Action, error) {
	return Action{}, errors.New("unused")
}
func (s *agentRunTestStore) InterruptAgentRun(context.Context, string, string) error {
	return errors.New("unused")
}
func (s *agentRunTestStore) BeginSetup(context.Context, string) (Action, error) {
	return Action{}, errors.New("unused")
}
func (s *agentRunTestStore) RecordSandboxActionSuccess(context.Context, string) error {
	return errors.New("unused")
}
func (s *agentRunTestStore) NextDelivery(context.Context, string) (*Delivery, error) {
	return nil, errors.New("unused")
}
func (s *agentRunTestStore) PrepareAgentRun(_ context.Context, runID, harness, baseline string) error {
	if s.run.ID != runID {
		return errors.New("wrong run")
	}
	s.run.Harness, s.run.BaselineRecorded, s.run.BaselineTurnID, s.run.State = harness, true, baseline, AgentRunSubmitting
	return nil
}
func (s *agentRunTestStore) BindAgentRun(_ context.Context, runID, harness, threadID, turnID, outcome string) error {
	if s.run.ID != runID {
		return errors.New("wrong run")
	}
	s.run.Harness, s.run.ThreadID, s.run.TurnID, s.run.TurnOutcome = harness, threadID, turnID, outcome
	switch outcome {
	case "completed":
		s.run.State = AgentRunCompleted
	case "failed":
		s.run.State = AgentRunFailed
	case "interrupted":
		s.run.State = AgentRunInterrupted
	default:
		s.run.State = AgentRunActive
	}
	return nil
}
func (s *agentRunTestStore) BindSteer(context.Context, string, string, string) error {
	return errors.New("unused")
}
func (s *agentRunTestStore) FailAgentRun(_ context.Context, runID, reason string) error {
	if s.run.ID != runID {
		return errors.New("wrong run")
	}
	s.run.State, s.run.Attention = AgentRunFailed, reason
	return nil
}
func (s *agentRunTestStore) UncertainAgentRun(_ context.Context, runID, reason string) error {
	if s.run.ID != runID {
		return errors.New("wrong run")
	}
	s.run.State, s.run.Attention = AgentRunUncertain, reason
	return nil
}
func (s *agentRunTestStore) AgentRunAttention(_ context.Context, runID, reason string) error {
	if s.run.ID != runID {
		return errors.New("wrong run")
	}
	s.run.Attention = reason
	return nil
}
func (s *agentRunTestStore) HarnessMutationDelivery(context.Context, string) (*Delivery, error) {
	return nil, errors.New("unused")
}
func (s *agentRunTestStore) SetCleanupAttention(context.Context, string, string) error {
	return errors.New("unused")
}
func (s *agentRunTestStore) CompleteCleanup(context.Context, string) error {
	return errors.New("unused")
}

func requireContains(t *testing.T, value, substring string) {
	t.Helper()
	if !strings.Contains(value, substring) {
		t.Fatalf("%q does not contain %q", value, substring)
	}
}
