package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestDirectAutomaticMessagesAndExactInterruptReconciliation(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	job, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{
		AdmissionKey:   fmt.Sprintf("assistant-%d", time.Now().UnixNano()),
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
	if replay, err := store.AdmitDirectMessage(ctx, idleInput); err != nil || replay.Created || !reflect.DeepEqual(replay.Message, idle.Message) {
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
	selected, err := store.AgentMessage(ctx, job.ID)
	if err != nil || selected == nil || selected.MessageID != initial.Message.ID {
		t.Fatalf("interrupt did not take priority over queued input: %+v %v", selected, err)
	}
	observed, err := store.AgentMessageExecution(ctx, initial.Message.ID)
	if err != nil || !observed.AgentRun.InterruptRequested {
		t.Fatalf("interrupt was not durable: %+v %v", observed.AgentRun, err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || !deliveries[0].AgentRun.InterruptRequested || deliveries[1].AgentRun.InterruptRequested || !deliveries[2].AgentRun.InterruptRequested {
		t.Fatalf("interrupt projection did not follow the exact Turn: %+v %v", deliveries, err)
	}
	native := &interruptIntegrationOperation{
		integrationAgentOperation: integrationAgentOperation{externals: &integrationExternals{
			turns: []core.HarnessTurn{{ID: "first-turn", Status: "inProgress"}},
		}},
		expectedRunID: initial.AgentRun.ID,
	}
	execution := core.NewExecutionService(store, nil, nil, absurdruntime.RequireClaim).
		WithAgentExecution(resultBoundaryAgentExecution{operation: native})
	taskName := "dorf-message-interrupt-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		for _, observation := range []struct {
			turn, outcome string
			wantErr       bool
			wantState     core.AgentRunState
		}{
			{"foreign-turn", "completed", true, core.AgentRunActive},
			{"first-turn", "inProgress", false, core.AgentRunActive},
			{"first-turn", "interrupted", false, core.AgentRunInterrupted},
		} {
			native.binding = core.HarnessBinding{Harness: "codex", ThreadID: "assistant-thread",
				Turn: core.HarnessTurn{ID: observation.turn, Status: observation.outcome}}
			_, reconcileErr := execution.ReconcileJobAgent(taskCtx, job.ID)
			if (reconcileErr != nil) != observation.wantErr {
				return core.TaskResultV1{}, fmt.Errorf("interrupt observation %s/%s: %v", observation.turn, observation.outcome, reconcileErr)
			}
			observed, err := store.AgentMessageExecution(taskCtx, initial.Message.ID)
			if err != nil || observed.AgentRun.State != observation.wantState || observed.AgentRun.TurnID != "first-turn" {
				return core.TaskResultV1{}, fmt.Errorf("interrupt observation changed wrong state or target: %+v %v", observed.AgentRun, err)
			}
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "interrupted"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "interrupt-proof", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailAgentRun(ctx, core.AgentRunID(steer.Message.ID), "steer target settled before delivery"); err != nil {
		t.Fatal(err)
	}
	next, err := codingDelivery(ctx, store, job.ID)
	if err != nil || next == nil || next.Message.ID != idle.Message.ID || next.AgentRun.ThreadID != "assistant-thread" {
		t.Fatalf("follow did not retain the conversation: %+v %v", next, err)
	}
	if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, "codex", "first-turn"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", "assistant-thread", "second-turn", "inProgress"); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestMessageInterrupt(ctx, job.ID, steer.Message.ID); err != nil {
		t.Fatal(err)
	}
	observed, err = store.AgentMessageExecution(ctx, next.Message.ID)
	if err != nil || observed.AgentRun.InterruptRequested {
		t.Fatalf("old interrupt targeted successor: %+v %v", observed.AgentRun, err)
	}
	if replay, err := store.AdmitDirectMessage(ctx, activeInput); err != nil || replay.Created || !reflect.DeepEqual(replay.Message, steer.Message) {
		t.Fatalf("auto replay retargeted successor: %+v %v", replay, err)
	}
	if err := store.RequestMessageInterrupt(ctx, "foreign-job", steer.Message.ID); !errors.Is(err, core.ErrMessageInterruptUnavailable) {
		t.Fatalf("foreign Job accepted interrupt: %v", err)
	}
}

// Only the native boundary is simulated; selection, claims, and recording use the worker and PostgreSQL.
type interruptIntegrationOperation struct {
	integrationAgentOperation
	expectedRunID string
	binding       core.HarnessBinding
}

func (o *interruptIntegrationOperation) Interrupt(_ context.Context, run core.AgentRun) (core.HarnessBinding, error) {
	if run.ID != o.expectedRunID || run.Harness != "codex" || run.ThreadID != "assistant-thread" || run.TurnID != "first-turn" {
		return core.HarnessBinding{}, fmt.Errorf("interrupt targeted a different run: %+v", run)
	}
	return o.binding, nil
}
