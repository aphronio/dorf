package postgres_test

import (
	"context"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestObservationAdmissionRetainsKindAndSupportsAuto(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	session, _ := prepareTransportIntegrationSession(t, store, "observation")
	input := core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: core.MessageFromWorkflow, FromID: "event-1", Input: "Task updated", Intent: core.MessageFollow, Observation: true}
	accepted, err := store.AdmitDirectMessage(ctx, input)
	if err != nil || !accepted.Created {
		t.Fatalf("admit: %+v %v", accepted, err)
	}
	restarted := postgres.Store{DB: store.DB}
	execution, err := restarted.AgentMessageExecution(ctx, accepted.Message.ID)
	if err != nil || !execution.Message.Observation {
		t.Fatalf("restart: %+v %v", execution, err)
	}
	replay, err := restarted.AdmitDirectMessage(ctx, input)
	if err != nil || replay.Created || !replay.Message.Observation {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	input.Observation = false
	if _, err := restarted.AdmitDirectMessage(ctx, input); err == nil {
		t.Fatal("changed input kind replay accepted")
	}
	input.Observation = true
	input.Intent, input.FromID = core.MessageAuto, "auto"
	if receipt, err := restarted.AdmitDirectMessage(ctx, input); err != nil || !receipt.Message.Observation {
		t.Fatalf("automatic observation: %+v %v", receipt, err)
	}
	for _, intent := range []core.MessageDeliveryIntent{core.MessageSteer} {
		input.Intent = intent
		input.FromID = string(intent)
		if _, err := restarted.AdmitDirectMessage(ctx, input); err == nil {
			t.Fatal("observation accepted without explicit follow")
		}
	}
}
