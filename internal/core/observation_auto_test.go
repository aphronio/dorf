package core

import (
	"context"
	"errors"
	"testing"
)

type observationCleanupStore struct {
	ExecutionStore
	accepted string
	failed   bool
}

func (s *observationCleanupStore) RequeueAutoMessageAsFollow(_ context.Context, _, _, accepted string) error {
	s.accepted = accepted
	return nil
}

func (s *observationCleanupStore) FailAgentRun(context.Context, string, string) error {
	s.failed = true
	return nil
}

func TestCleanupRetainsAutomaticObservationAcceptedAfterTarget(t *testing.T) {
	store := &observationCleanupStore{}
	service := NewExecutionService(store, nil, nil, func(context.Context) error { return nil })
	delivery := Delivery{Message: Message{ID: "event", Observation: true, RequestedIntent: MessageAuto, TargetTurnID: "target"}, AgentRun: AgentRun{ID: "run"}}
	turns := []HarnessTurn{{ID: "target", Status: "completed"}, {ID: "accepted", Status: "running", AcceptedMessageIDs: []string{"run"}}}
	err := service.cleanupTerminalSteerTarget(context.Background(), delivery, turns)
	var active cleanupStillActive
	if !errors.As(err, &active) || store.accepted != "accepted" || store.failed {
		t.Fatalf("cleanup discarded accepted work: store=%+v err=%v", store, err)
	}
	store = &observationCleanupStore{}
	service.store = store
	if err := service.cleanupTerminalSteerTarget(context.Background(), delivery, turns[:1]); err != nil || !store.failed || store.accepted != "" {
		t.Fatalf("cleanup should close unaccepted delivery, not resubmit: store=%+v err=%v", store, err)
	}
}

func TestAutomaticObservationRejectsConflictingAttribution(t *testing.T) {
	delivery := Delivery{Message: Message{Observation: true, TargetTurnID: "target"}, AgentRun: AgentRun{ID: "run"}}
	for _, turns := range [][]HarnessTurn{
		{{ID: "before", AcceptedMessageIDs: []string{"run"}}, {ID: "target"}},
		{{ID: "target"}, {ID: "first", AcceptedMessageIDs: []string{"run"}}, {ID: "second", AcceptedMessageIDs: []string{"run"}}},
	} {
		if _, err := automaticObservationTurn(delivery, turns); err == nil {
			t.Fatalf("accepted conflicting attribution: %+v", turns)
		}
	}
}
