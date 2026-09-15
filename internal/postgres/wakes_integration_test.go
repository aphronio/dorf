package postgres_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestJobExecutionWakeSerializesDeduplicatesAndRollsBackEmitFailure(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	job, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("wake-revision-%d", time.Now().UnixNano()), SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	if revision, err := store.JobExecutionWakeRevision(ctx, job.ID); err != nil || revision != 0 {
		t.Fatalf("initial wake revision=%d err=%v", revision, err)
	}

	const signals = 12
	revisions := make(chan int64, signals)
	errors := make(chan error, signals)
	var group sync.WaitGroup
	for index := range signals {
		group.Add(1)
		go func() {
			defer group.Done()
			revision, err := store.SignalJobExecutionWake(ctx, client.QueueName(), job.ID, fmt.Sprintf("message:concurrent-%02d", index))
			if err != nil {
				errors <- err
				return
			}
			revisions <- revision
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	close(revisions)
	var got []int
	for revision := range revisions {
		got = append(got, int(revision))
	}
	sort.Ints(got)
	for index, revision := range got {
		if revision != index+1 {
			t.Fatalf("serialized revisions=%v", got)
		}
	}
	replayed, err := store.SignalJobExecutionWake(ctx, client.QueueName(), job.ID, "message:concurrent-00")
	if err != nil || replayed < 1 || replayed > signals {
		t.Fatalf("duplicate wake revision=%d err=%v", replayed, err)
	}
	if revision, err := store.JobExecutionWakeRevision(ctx, job.ID); err != nil || revision != signals {
		t.Fatalf("deduplicated wake revision=%d err=%v", revision, err)
	}
	var causes int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from dorf.job_execution_wake_causes where job_id=$1`, job.ID).Scan(&causes); err != nil || causes != signals {
		t.Fatalf("wake causes=%d err=%v", causes, err)
	}

	rollbackJob, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("wake-rollback-%d", time.Now().UnixNano()), SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SignalJobExecutionWake(ctx, "missing_wake_queue", rollbackJob.ID, "message:rollback"); err == nil {
		t.Fatal("wake signal unexpectedly succeeded without an Absurd queue")
	}
	if revision, err := store.JobExecutionWakeRevision(ctx, rollbackJob.ID); err != nil || revision != 0 {
		t.Fatalf("failed emit committed revision=%d err=%v", revision, err)
	}
	if err := store.DB.QueryRowContext(ctx, `select count(*) from dorf.job_execution_wake_causes where job_id=$1`, rollbackJob.ID).Scan(&causes); err != nil || causes != 0 {
		t.Fatalf("failed emit committed causes=%d err=%v", causes, err)
	}
}

func TestJobExecutionWakeIsDurableBeforeWaitAndTimeoutReloads(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	job, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("wake-await-%d", time.Now().UnixNano()), SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	application := core.Application{Store: store, Tasks: client}
	for _, test := range []struct {
		name     string
		revision int64
		timeout  time.Duration
		signal   bool
	}{
		{name: "emit-before-wait", revision: 1, timeout: time.Second, signal: true},
		{name: "timeout", revision: 2, timeout: 25 * time.Millisecond},
	} {
		taskName := "dorf-job-execution-wake-proof-" + test.name
		client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
			err := application.AwaitJobExecutionWake(taskCtx, job.ID, test.revision, "test/wake/"+test.name, test.timeout)
			return core.TaskResultV1{JobID: job.ID, Outcome: test.name}, err
		}))
		spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID})
		if err != nil {
			t.Fatal(err)
		}
		if test.signal {
			if revision, err := store.SignalJobExecutionWake(ctx, client.QueueName(), job.ID, "message:before-wait"); err != nil || revision != test.revision {
				t.Fatalf("signal revision=%d err=%v", revision, err)
			}
		}
		workerCtx, stopWorker := context.WithCancel(ctx)
		workerDone := make(chan error, 1)
		go func() {
			workerDone <- client.RunWorker(workerCtx, absurd.WorkerOptions{WorkerID: "wake-" + test.name, BatchSize: 1, Concurrency: 1, ClaimTimeout: time.Minute})
		}()
		resultCtx, cancelResult := context.WithTimeout(ctx, 5*time.Second)
		result, resultErr := client.AwaitTaskResult(resultCtx, client.QueueName(), spawned.TaskID)
		cancelResult()
		stopWorker()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Fatal("wake worker did not stop")
		}
		if resultErr != nil || result.State != absurd.TaskCompleted {
			t.Fatalf("wake task=%#v err=%v", result, resultErr)
		}
	}
}

func TestNativeTerminalWakeAcceptsFastBindRaceAndRejectsForeignOrClosedTargets(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	job, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("native-wake-%d", time.Now().UnixNano()), SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := codingDelivery(ctx, store, job.ID)
	if err != nil || delivery == nil {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
	target := core.NativeTerminalWakeTarget{
		JobID: job.ID, SandboxID: delivery.AgentRun.SandboxID, AgentRunID: delivery.AgentRun.ID,
		ThreadID: "fast-thread", TurnID: "fast-turn",
	}
	if signaled, err := store.SignalNativeTerminalWake(ctx, client.QueueName(), target); err != nil || !signaled {
		t.Fatalf("pre-bind terminal signal=%t err=%v", signaled, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, delivery.AgentRun.ID, "codex", target.ThreadID, target.TurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	if signaled, err := store.SignalNativeTerminalWake(ctx, client.QueueName(), target); err != nil || !signaled {
		t.Fatalf("terminal replay signal=%t err=%v", signaled, err)
	}
	if revision, err := store.JobExecutionWakeRevision(ctx, job.ID); err != nil || revision != 1 {
		t.Fatalf("terminal replay revision=%d err=%v", revision, err)
	}
	foreign := target
	foreign.TurnID = "foreign-turn"
	if _, err := store.SignalNativeTerminalWake(ctx, client.QueueName(), foreign); err == nil {
		t.Fatal("foreign terminal binding was accepted")
	}
	if err := store.RequestCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if signaled, err := store.SignalNativeTerminalWake(ctx, client.QueueName(), target); err != nil || signaled {
		t.Fatalf("closed Job terminal signal=%t err=%v", signaled, err)
	}
}

func TestStopWakeReturnsAndReemitsOriginalTurnTarget(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	job, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("stop-wake-%d", time.Now().UnixNano()), SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := codingDelivery(ctx, store, job.ID)
	if err != nil || delivery == nil {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, delivery.AgentRun.ID, "codex", "thread", "turn", "inProgress"); err != nil {
		t.Fatal(err)
	}
	application := core.Application{Store: store, Tasks: client}
	first, err := application.RequestMessageInterrupt(ctx, job.ID, delivery.Message.ID)
	if err != nil || first.AgentRunID != delivery.AgentRun.ID || first.JobID != job.ID || !first.InterruptRequested {
		t.Fatalf("first Stop target=%+v err=%v", first, err)
	}
	replayed, err := application.RequestMessageInterrupt(ctx, job.ID, delivery.Message.ID)
	if err != nil || replayed != first {
		t.Fatalf("replayed Stop target=%+v err=%v", replayed, err)
	}
	if revision, err := store.JobExecutionWakeRevision(ctx, job.ID); err != nil || revision != 1 {
		t.Fatalf("Stop replay revision=%d err=%v", revision, err)
	}
}
