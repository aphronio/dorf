package postgres_test

import (
	"context"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestDeveloperInstructionsRetainExactNullableAdmissionAcrossRestart(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, _ := prepareTransportIntegrationJob(t, store, "developer-instructions")
	value, empty := "Application rules\nexact bytes", ""
	for _, item := range []struct {
		key   string
		value *string
	}{{"absent", nil}, {"set", &value}, {"clear", &empty}} {
		input := core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: item.key, Input: "continue", Intent: core.MessageFollow, DeveloperInstructions: item.value}
		accepted, err := store.AdmitCodingMessage(ctx, input)
		if err != nil || !accepted.Created {
			t.Fatalf("admit: %+v %v", accepted, err)
		}
		restarted := postgres.Store{DB: store.DB}
		execution, err := restarted.AgentMessageExecution(ctx, accepted.Message.ID)
		if err != nil || !core.SameDeveloperInstructions(execution.Message.DeveloperInstructions, item.value) {
			t.Fatalf("restored snapshot: %+v %v", execution.Message, err)
		}
		replay, err := restarted.AdmitCodingMessage(ctx, input)
		if err != nil || replay.Created || !core.SameDeveloperInstructions(replay.Message.DeveloperInstructions, item.value) {
			t.Fatalf("replay: %+v %v", replay, err)
		}
		changed := "different generation"
		input.DeveloperInstructions = &changed
		if _, err := restarted.AdmitCodingMessage(ctx, input); err == nil {
			t.Fatal("changed snapshot replay accepted")
		}
	}
}
