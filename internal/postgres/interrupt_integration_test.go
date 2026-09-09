package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestDirectAutomaticMessagesAndExactInterruptSurviveReopen(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("assistant-%d", time.Now().UnixNano()), Goal: "retain this conversation",
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := codingDelivery(ctx, store, job.ID)
	if err != nil || initial == nil {
		t.Fatalf("initial=%v err=%v", initial, err)
	}
	idleInput := core.MessageAdmission{JobID: job.ID, SandboxID: initial.AgentRun.SandboxID,
		FromKind: core.MessageFromHuman, FromID: "idle-auto", Input: "next question", Intent: core.MessageAuto}
	idle, err := store.AdmitDirectMessage(ctx, idleInput)
	if err != nil || idle.Message.Intent != core.MessageFollow {
		t.Fatalf("idle message=%+v err=%v", idle, err)
	}
	if err := store.PrepareAgentRun(ctx, initial.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, initial.AgentRun.ID, "codex", "assistant-thread", "first-turn", "inProgress"); err != nil {
		t.Fatal(err)
	}
	activeInput := idleInput
	activeInput.FromID, activeInput.Input = "active-auto", "use the corrected value"
	steer, err := store.AdmitDirectMessage(ctx, activeInput)
	if err != nil || steer.Message.Intent != core.MessageSteer || steer.Message.TargetTurnID != "first-turn" {
		t.Fatalf("active message=%+v err=%v", steer, err)
	}
	if replay, err := store.AdmitDirectMessage(ctx, idleInput); err != nil || replay.Created || replay.Message != idle.Message {
		t.Fatalf("idle replay changed after work started: %+v %v", replay, err)
	}
	changed := activeInput
	changed.Intent = core.MessageSteer
	if _, err := store.AdmitDirectMessage(ctx, changed); !errors.Is(err, core.ErrMessageReplayConflict) {
		t.Fatalf("changed requested intent did not conflict: %v", err)
	}
	// The latest user message may still be an undelivered Steer when Stop arrives.
	if err := store.RequestMessageInterrupt(ctx, job.ID, steer.Message.ID); err != nil {
		t.Fatal(err)
	}
	reopened := postgres.Store{DB: store.DB}
	selected, err := reopened.AgentMessage(ctx, job.ID)
	if err != nil || selected == nil || selected.MessageID != initial.Message.ID {
		t.Fatalf("interrupt did not take priority over queued input: %+v %v", selected, err)
	}
	observed, err := reopened.AgentMessageExecution(ctx, initial.Message.ID)
	if err != nil || !observed.AgentRun.InterruptRequested {
		t.Fatalf("interrupt was not durable: %+v %v", observed.AgentRun, err)
	}
	deliveries, err := reopened.Deliveries(ctx, job.ID)
	if err != nil || !deliveries[0].AgentRun.InterruptRequested || deliveries[1].AgentRun.InterruptRequested || !deliveries[2].AgentRun.InterruptRequested {
		t.Fatalf("interrupt projection did not follow the exact Turn: %+v %v", deliveries, err)
	}
	if err := reopened.BindAgentRun(ctx, initial.AgentRun.ID, "codex", "assistant-thread", "first-turn", "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.FailAgentRun(ctx, core.AgentRunID(steer.Message.ID), "steer target settled before delivery"); err != nil {
		t.Fatal(err)
	}
	next, err := codingDelivery(ctx, reopened, job.ID)
	if err != nil || next == nil || next.Message.ID != idle.Message.ID || next.AgentRun.ThreadID != "assistant-thread" {
		t.Fatalf("follow did not retain the conversation: %+v %v", next, err)
	}
	if err := reopened.PrepareAgentRun(ctx, next.AgentRun.ID, "codex", "first-turn"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.BindAgentRun(ctx, next.AgentRun.ID, "codex", "assistant-thread", "second-turn", "inProgress"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RequestMessageInterrupt(ctx, job.ID, steer.Message.ID); err != nil {
		t.Fatal(err)
	}
	observed, err = reopened.AgentMessageExecution(ctx, next.Message.ID)
	if err != nil || observed.AgentRun.InterruptRequested {
		t.Fatalf("old interrupt targeted successor: %+v %v", observed.AgentRun, err)
	}
	if replay, err := reopened.AdmitDirectMessage(ctx, activeInput); err != nil || replay.Created || replay.Message != steer.Message {
		t.Fatalf("auto replay retargeted successor: %+v %v", replay, err)
	}
	if err := reopened.RequestMessageInterrupt(ctx, "foreign-job", steer.Message.ID); !errors.Is(err, core.ErrMessageInterruptUnavailable) {
		t.Fatalf("foreign Job accepted interrupt: %v", err)
	}
}
