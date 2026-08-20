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
		store:        store,
		run:          store.run,
		harness:      "codex",
		beforeRecord: allowAgentRunRecord,
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
		store:        store,
		run:          store.run,
		harness:      "codex",
		beforeRecord: allowAgentRunRecord,
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

func TestAgentRunReturnsAfterDurablyBindingActiveTurn(t *testing.T) {
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", State: AgentRunPending}}
	activeBarriers := 0
	contract := agentRunContract{
		store:        store,
		run:          store.run,
		harness:      "pi",
		beforeRecord: allowAgentRunRecord,
		reachBarrier: func(_ context.Context, point string, _ Delivery) error {
			if point == BarrierHarnessActive {
				activeBarriers++
			}
			return nil
		},
		submitNew: func(context.Context, AgentRun) (HarnessBinding, error) {
			return HarnessBinding{Harness: "pi", ThreadID: "thread-1", Turn: HarnessTurn{ID: "turn-1", Status: "running"}}, nil
		},
	}

	turn, err := contract.execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != "running" || store.run.State != AgentRunActive {
		t.Fatalf("active submission returned turn=%#v run=%#v", turn, store.run)
	}
	if activeBarriers != 1 {
		t.Fatalf("active submission barriers=%d, want 1", activeBarriers)
	}
}

func TestAgentRunObservationSettlesExactBoundTurn(t *testing.T) {
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", Harness: "pi", ThreadID: "thread-1", TurnID: "turn-1", State: AgentRunActive, BaselineRecorded: true}}
	contract := agentRunContract{
		store:        store,
		run:          store.run,
		harness:      "pi",
		beforeRecord: allowAgentRunRecord,
		history: func(context.Context, AgentRun) (HarnessHistory, error) {
			return HarnessHistory{Harness: "pi", ThreadID: "thread-1", Turns: []HarnessTurn{{ID: "turn-1", Status: "completed"}}}, nil
		},
	}

	turn, err := contract.execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if turn.ID != "turn-1" || turn.Status != "completed" || store.run.State != AgentRunCompleted {
		t.Fatalf("observation returned turn=%#v run=%#v", turn, store.run)
	}
}

func TestAgentRunReconcilesLostBoundSubmissionAcknowledgement(t *testing.T) {
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", Harness: "codex", ThreadID: "thread-1", State: AgentRunSubmitting, BaselineRecorded: true, BaselineTurnID: "before"}}
	history := HarnessHistory{Harness: "codex", ThreadID: "thread-1", Turns: []HarnessTurn{{ID: "before", Status: "completed"}}}
	contract := agentRunContract{
		store:        store,
		run:          store.run,
		beforeRecord: allowAgentRunRecord,
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
	claimChecks := 0
	contract := agentRunContract{
		store: store,
		run:   store.run,
		submitBound: func(context.Context, AgentRun) (HarnessBinding, error) {
			return HarnessBinding{Harness: "codex", ThreadID: "thread-1", Turn: HarnessTurn{ID: "turn-1", Status: "running"}}, nil
		},
		history: func(context.Context, AgentRun) (HarnessHistory, error) {
			return HarnessHistory{Harness: "codex", ThreadID: "thread-1"}, nil
		},
		beforeRecord: func(context.Context) error {
			claimChecks++
			if claimChecks > 1 {
				return claimLost
			}
			return nil
		},
	}

	_, err := contract.execute(context.Background())
	if !errors.Is(err, claimLost) {
		t.Fatalf("execute error = %v", err)
	}
	if store.run.TurnID != "" || store.run.State != AgentRunSubmitting {
		t.Fatalf("claim loss durably bound submitted turn: %#v", store.run)
	}
}

func TestAgentRunClaimLossBeforeBaselineDoesNotPrepareOrSubmit(t *testing.T) {
	claimLost := errors.New("claim lost")
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", State: AgentRunPending}}
	submits := 0
	contract := agentRunContract{
		store:        store,
		run:          store.run,
		harness:      "codex",
		beforeRecord: func(context.Context) error { return claimLost },
		submitNew: func(context.Context, AgentRun) (HarnessBinding, error) {
			submits++
			return HarnessBinding{}, nil
		},
	}

	if _, err := contract.execute(context.Background()); !errors.Is(err, claimLost) {
		t.Fatalf("execute error = %v", err)
	}
	if submits != 0 || store.run.State != AgentRunPending || store.run.BaselineRecorded || store.run.Harness != "" {
		t.Fatalf("stale attempt prepared=%#v submits=%d", store.run, submits)
	}
}

func TestLostClaimCannotOverwriteReplacementAgentRun(t *testing.T) {
	claimLost := errors.New("claim lost")
	replacement := AgentRun{ID: "run-1", State: AgentRunActive, Harness: "codex", ThreadID: "replacement-thread", TurnID: "replacement-turn"}
	for _, test := range []struct {
		name     string
		contract func(*agentRunTestStore) agentRunContract
	}{
		{"failed", func(store *agentRunTestStore) agentRunContract {
			return agentRunContract{
				store: store, run: AgentRun{ID: replacement.ID, Harness: "codex", BaselineRecorded: true}, beforeRecord: func(context.Context) error { return claimLost },
				submitNew: func(context.Context, AgentRun) (HarnessBinding, error) {
					return HarnessBinding{}, errors.New("definite stale failure")
				},
				onSubmitError: func(ctx context.Context, run AgentRun, _ HarnessBinding, _ error) (HarnessTurn, error) {
					err := claimBeforeAgentRunRecord(ctx, func(context.Context) error { return claimLost }, func() error { return store.FailAgentRun(ctx, run.ID, "stale failure") })
					return HarnessTurn{}, err
				},
			}
		}},
		{"uncertain", func(store *agentRunTestStore) agentRunContract {
			return agentRunContract{
				store: store, run: AgentRun{ID: replacement.ID, Harness: "codex", ThreadID: "stale-thread", State: AgentRunActive}, beforeRecord: func(context.Context) error { return claimLost },
				history: func(context.Context, AgentRun) (HarnessHistory, error) {
					return HarnessHistory{Harness: "codex", ThreadID: "foreign-thread"}, nil
				},
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &agentRunTestStore{run: replacement}
			if _, err := test.contract(store).execute(context.Background()); !errors.Is(err, claimLost) {
				t.Fatalf("execute error = %v", err)
			}
			if store.run != replacement {
				t.Fatalf("stale %s changed replacement to %#v", test.name, store.run)
			}
		})
	}
}

func TestFailedAgentRunWithoutTurnRemainsTerminal(t *testing.T) {
	store := &agentRunTestStore{run: AgentRun{ID: "run-1", Harness: "codex", State: AgentRunFailed, Attention: "proved no submission"}}
	turn, err := (agentRunContract{store: store, run: store.run, beforeRecord: allowAgentRunRecord}).execute(context.Background())
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

func allowAgentRunRecord(context.Context) error { return nil }

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
func (s *agentRunTestStore) UncertainAgentRun(_ context.Context, runID, reason string) error {
	if s.run.ID != runID {
		return errors.New("wrong run")
	}
	s.run.State, s.run.Attention = AgentRunUncertain, reason
	return nil
}
func (s *agentRunTestStore) FailAgentRun(_ context.Context, runID, reason string) error {
	if s.run.ID != runID {
		return errors.New("wrong run")
	}
	s.run.State, s.run.Attention = AgentRunFailed, reason
	return nil
}

func requireContains(t *testing.T, value, substring string) {
	t.Helper()
	if !strings.Contains(value, substring) {
		t.Fatalf("%q does not contain %q", value, substring)
	}
}

func TestServiceRefusesDurableRecordWithoutClaimCheck(t *testing.T) {
	if err := (ExecutionService{}).requireClaim(context.Background()); err == nil {
		t.Fatal("missing durable executor claim check was accepted")
	}
}
