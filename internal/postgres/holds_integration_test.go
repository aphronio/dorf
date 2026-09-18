package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestDeliveryHoldPreservesAdmissionDrainAndRestart(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	session, _, err := admitDirectFixture(t, store, ctx, core.SessionAdmission{AdmissionKey: fmt.Sprintf("hold-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := core.MainSandboxName(session.ID)
	current, err := nextDelivery(ctx, store, session.ID)
	if err != nil || current == nil {
		t.Fatalf("initial delivery: %v", err)
	}
	if err := store.PrepareAgentRun(ctx, current.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, current.AgentRun.ID, "codex", "held-thread", "held-turn", "running"); err != nil {
		t.Fatal(err)
	}
	admit := func(key string, intent core.MessageDeliveryIntent) (core.MessageAdmissionResult, error) {
		return store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: sandboxID, FromKind: core.MessageFromHuman, FromID: key, Input: key, Intent: intent})
	}
	steer, err := admit("pre-hold-steer", core.MessageSteer)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := store.HoldSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, session.ID+":upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HoldSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, session.ID+":competing"); err == nil {
		t.Fatal("competing hold accepted")
	}
	if _, err := admit("explicit-steer", core.MessageSteer); !errors.Is(err, core.ErrMessageSteerUnavailable) {
		t.Fatalf("held explicit steer: %v", err)
	}
	var queued []core.Message
	for i := range 3 {
		input, err := admit(fmt.Sprintf("queued-%d", i), core.MessageAuto)
		if err != nil || input.Message.Intent != core.MessageFollow {
			t.Fatalf("automatic input did not queue: %v", err)
		}
		replay, err := admit(fmt.Sprintf("queued-%d", i), core.MessageAuto)
		if err != nil || replay.Created || replay.Message.ID != input.Message.ID {
			t.Fatalf("accepted Message replay: %v", err)
		}
		queued = append(queued, input.Message)
	}
	selected, err := store.AgentMessage(ctx, session.ID)
	if err != nil || selected == nil || selected.MessageID != steer.Message.ID {
		t.Fatalf("pre-hold steer did not drain: %v", err)
	}
	if err := store.PrepareAgentRun(ctx, core.AgentRunID(steer.Message.ID), "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSteer(ctx, core.AgentRunID(steer.Message.ID), "held-turn", "running"); err != nil {
		t.Fatal(err)
	}
	selected, err = store.AgentMessage(ctx, session.ID)
	if err != nil || selected == nil || selected.MessageID != current.Message.ID {
		t.Fatalf("active observation blocked: %v", err)
	}
	if err := store.BindAgentRun(ctx, current.AgentRun.ID, "codex", "held-thread", "held-turn", "completed"); err != nil {
		t.Fatal(err)
	}
	// Open a new database connection, as a restarted controller would.
	db, err := sql.Open("pgx", os.Getenv("DORF_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted := postgres.Store{DB: db}
	held, err := restarted.SandboxDeliveryHeld(ctx, sandboxID)
	if err != nil || !held {
		t.Fatalf("restart lost hold: %v", err)
	}
	if ready, err := restarted.HasImmediatelyEligibleAgentMessage(ctx, session.ID); err != nil || ready {
		t.Fatalf("held FIFO became runnable: %v", err)
	}
	if selected, err := restarted.AgentMessage(ctx, session.ID); err != nil || selected != nil {
		t.Fatalf("held FIFO dispatched: %v", err)
	}
	if err := restarted.ReleaseSandboxDelivery(ctx, "missing_queue", session.ID, sandboxID, hold.ID); err == nil {
		t.Fatal("release succeeded without durable wake")
	}
	if held, err := restarted.SandboxDeliveryHeld(ctx, sandboxID); err != nil || !held {
		t.Fatal("failed wake committed release")
	}
	before, err := restarted.SessionExecutionWakeRevision(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReleaseSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, hold.ID); err != nil {
		t.Fatal(err)
	}
	after, err := restarted.SessionExecutionWakeRevision(ctx, session.ID)
	if err != nil || after != before+1 {
		t.Fatalf("release did not wake delivery: %v", err)
	}
	for _, input := range queued {
		selected, err := restarted.AgentMessage(ctx, session.ID)
		if err != nil || selected == nil || selected.MessageID != input.ID {
			t.Fatalf("FIFO changed after release: %v", err)
		}
		runID := core.AgentRunID(input.ID)
		if err := restarted.PrepareAgentRun(ctx, runID, "codex", "held-turn"); err != nil {
			t.Fatal(err)
		}
		if err := restarted.BindAgentRun(ctx, runID, "codex", "held-thread", "turn-"+input.ID, "completed"); err != nil {
			t.Fatal(err)
		}
	}
	second, err := restarted.HoldSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, session.ID+":second")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReleaseSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, hold.ID); err != nil {
		t.Fatal(err)
	}
	if held, err := restarted.SandboxDeliveryHeld(ctx, sandboxID); err != nil || !held {
		t.Fatal("stale release cleared newer hold")
	}
	old, err := restarted.HoldSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, hold.ID)
	if err != nil || old.ReleasedAt.IsZero() {
		t.Fatal("old request replay reopened hold")
	}
	if _, err := db.ExecContext(ctx, "update dorf.sessions set sandbox_last_active_at=clock_timestamp()-interval '2 minutes' where id=$1", session.ID); err != nil {
		t.Fatal(err)
	}
	if idle, err := restarted.SandboxIdleFor(ctx, session.ID, time.Minute); err != nil || idle {
		t.Fatal("idle pause ignored delivery hold")
	}
	if err := restarted.ReleaseSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, second.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryHoldThroughDurableWorkerRestartAndCleanup(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	harness := newResponsivenessHarness()
	harness.submitStatus = "completed"
	external := &integrationExternals{}
	configure := func(tasks *absurd.Client) core.Application {
		execution := core.NewExecutionService(store, external, nil, absurdruntime.RequireClaim).WithAgentExecution(harness)
		resolver := integrationRuntimeResolver{execution: execution, profile: "incus"}
		app := core.Application{Store: store, Tasks: tasks, SandboxRuntimes: resolver, CleanupRuntimes: resolver}
		direct.Register(app, store, resolver)
		app.RegisterCleanup()
		return app
	}
	configure(client)
	session, _, err := store.AdmitDirect(ctx, core.SessionAdmission{AdmissionKey: fmt.Sprintf("held-worker-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"}, client.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := core.MainSandboxName(session.ID)
	hold, err := store.HoldSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, session.ID+":upgrade")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		_, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: sandboxID, FromKind: core.MessageFromHuman, FromID: fmt.Sprintf("queued-%d", i), Input: "synthetic queued input", Intent: core.MessageAuto})
		if err != nil {
			t.Fatal(err)
		}
	}
	start := func(tasks *absurd.Client) func() {
		workCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- tasks.RunWorker(workCtx, absurd.WorkerOptions{WorkerID: "held-worker", ClaimTimeout: time.Minute, BatchSize: 1, Concurrency: 1, PollInterval: 10 * time.Millisecond})
		}()
		var once sync.Once
		finish := func() {
			once.Do(func() {
				stop()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("worker: %v", err)
				}
			})
		}
		t.Cleanup(finish)
		return finish
	}
	wait := func(ready func() bool) {
		for !ready() {
			select {
			case <-ctx.Done():
				t.Fatal("durable hold proof timed out")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	stop := start(client)
	wait(func() bool {
		state, err := client.FetchTaskResult(ctx, client.QueueName(), session.CurrentTaskID)
		return err == nil && state != nil && state.State == absurd.TaskSleeping
	})
	stop()
	harness.mu.Lock()
	submitted := harness.submissions
	harness.mu.Unlock()
	if submitted != 0 {
		t.Fatal("held input reached native Submit")
	}
	restarted, err := absurd.New(absurd.Options{DB: store.DB, QueueName: client.QueueName()})
	if err != nil {
		t.Fatal(err)
	}
	configure(restarted)
	start(restarted)
	if err := store.ReleaseSandboxDelivery(ctx, restarted.QueueName(), session.ID, sandboxID, hold.ID); err != nil {
		t.Fatal(err)
	}
	wait(func() bool {
		deliveries, err := store.Deliveries(ctx, session.ID)
		return err == nil && len(deliveries) == 2 && deliveries[0].AgentRun.State == core.AgentRunCompleted && deliveries[1].AgentRun.State == core.AgentRunCompleted
	})
	harness.mu.Lock()
	submitted = harness.submissions
	harness.mu.Unlock()
	if submitted != 2 {
		t.Fatalf("native submissions=%d; want exactly two", submitted)
	}
	if _, err := store.HoldSandboxDelivery(ctx, restarted.QueueName(), session.ID, sandboxID, session.ID+":cleanup-hold"); err != nil {
		t.Fatal(err)
	}
	if err := store.ScheduleCleanup(ctx, restarted.QueueName(), session.ID, ""); err != nil {
		t.Fatal(err)
	}
	cleaning, err := store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AwaitTaskResult(ctx, restarted.QueueName(), cleaning.CurrentTaskID); err != nil {
		t.Fatal(err)
	}
	if held, err := store.SandboxDeliveryHeld(ctx, sandboxID); err != nil || held {
		t.Fatal("completed cleanup retained an active hold")
	}
}
