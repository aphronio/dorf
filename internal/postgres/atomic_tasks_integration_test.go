package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	"github.com/jackc/pgx/v5"
)

func atomicAdmissionInput(t *testing.T) core.JobAdmission {
	t.Helper()
	return core.JobAdmission{
		AdmissionKey: fmt.Sprintf("atomic-%s-%d", t.Name(), time.Now().UnixNano()),
		Goal:         "commit admission and task together", SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "high",
	}
}

// Reject one Job's attachment after the public Absurd spawn has executed.
// The constraint is scoped to this fixture and does not reject concurrent tests.
func rejectTaskAttachment(t *testing.T, store postgres.Store, jobID string, after int) func() {
	t.Helper()
	name := pgx.Identifier{fmt.Sprintf("atomic_attachment_%d", time.Now().UnixNano())}.Sanitize()
	literal := "'" + strings.ReplaceAll(jobID, "'", "''") + "'"
	_, err := store.DB.ExecContext(context.Background(), fmt.Sprintf(
		`alter table dorf.job_tasks add constraint %s check (job_id <> %s or sequence <= %d) not valid`, name, literal, after))
	if err != nil {
		t.Fatal(err)
	}
	remove := func() {
		if _, err := store.DB.ExecContext(context.Background(), "alter table dorf.job_tasks drop constraint if exists "+name); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(remove)
	return remove
}

func TestAtomicAdmissionRollsBackJobAndUnattachedTask(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := atomicAdmissionInput(t)
	jobID := core.JobID(input.AdmissionKey)
	remove := rejectTaskAttachment(t, store, jobID, 0)
	if _, _, err := store.AdmitDirect(ctx, input, client.QueueName()); err == nil {
		t.Fatal("admission succeeded despite attachment rejection")
	}
	if exists, err := store.JobExists(ctx, jobID); err != nil || exists {
		t.Fatalf("failed admission left Job: exists=%t err=%v", exists, err)
	}
	// Public Spawn must create a fresh task: a failed admission retained neither
	// a runnable task nor its idempotency key inside Absurd.
	spawned, err := client.Spawn(ctx, direct.TaskName, core.JobTaskParams{JobID: jobID}, absurd.SpawnOptions{
		QueueName: client.QueueName(), IdempotencyKey: direct.TaskKey(jobID),
	})
	if err != nil || !spawned.Created {
		t.Fatalf("failed admission retained Absurd task: %#v err=%v", spawned, err)
	}
	remove()
	// The probe also models an older writer that spawned before attaching.
	job, created, err := store.AdmitDirect(ctx, input, client.QueueName())
	if err != nil || !created || job.CurrentTaskID != spawned.TaskID {
		t.Fatalf("atomic admission=%#v created=%t err=%v", job, created, err)
	}
}

func TestConcurrentAtomicAdmissionIsAttachedBeforeWorkerCanRun(t *testing.T) {
	_, store, _ := testDatabase(t)
	client := newFaultClient(t, store, fmt.Sprintf("dorf_atomic_concurrent_%d", time.Now().UnixNano()))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var executions atomic.Int32
	client.MustRegister(absurd.Task(direct.TaskName, func(ctx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		executions.Add(1)
		task, _ := absurd.TaskFromContext(ctx)
		job, err := store.Job(ctx, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		history, err := store.JobTasks(ctx, job.ID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		if job.CurrentTaskID != task.TaskID() || len(history) != 1 || history[0].TaskID != task.TaskID() || params.PreviousTaskID != "" {
			return core.TaskResultV1{}, fmt.Errorf("worker observed incomplete admission: Job=%#v history=%#v", job, history)
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "attached"}, nil
	}))
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- client.RunWorker(workerCtx, absurd.WorkerOptions{WorkerID: "atomic-admission", Concurrency: 1, BatchSize: 1, ClaimTimeout: time.Minute})
	}()
	defer func() { stop(); <-done }()
	input := atomicAdmissionInput(t)
	type receipt struct {
		job     core.Job
		created bool
		err     error
	}
	results := make(chan receipt, 8)
	start := make(chan struct{})
	for range 8 {
		go func() {
			<-start
			job, created, err := store.AdmitDirect(ctx, input, client.QueueName())
			results <- receipt{job, created, err}
		}()
	}
	close(start)
	createdCount, taskID := 0, ""
	for range 8 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.created {
			createdCount++
		}
		if taskID == "" {
			taskID = result.job.CurrentTaskID
		}
		if taskID == "" || result.job.CurrentTaskID != taskID {
			t.Fatalf("conflicting task receipt: %#v", result)
		}
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), taskID); err != nil {
		t.Fatal(err)
	}
	if createdCount != 1 || executions.Load() != 1 {
		t.Fatalf("created=%d executions=%d", createdCount, executions.Load())
	}
}

func TestAtomicCleanupRollsBackCancellationAndAppendsOneTask(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, _, err := store.AdmitDirect(ctx, atomicAdmissionInput(t), client.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	before, err := client.FetchTaskResult(ctx, client.QueueName(), job.CurrentTaskID)
	if err != nil || before == nil {
		t.Fatalf("initial task=%#v err=%v", before, err)
	}
	remove := rejectTaskAttachment(t, store, job.ID, 1)
	if err := store.ScheduleCleanup(ctx, client.QueueName(), job.ID, ""); err == nil {
		t.Fatal("cleanup succeeded despite attachment rejection")
	}
	after, err := store.Job(ctx, job.ID)
	if err != nil || after != job {
		t.Fatalf("failed cleanup changed Job: %#v err=%v", after, err)
	}
	task, err := client.FetchTaskResult(ctx, client.QueueName(), job.CurrentTaskID)
	if err != nil || task == nil || task.State != before.State {
		t.Fatalf("failed cleanup cancelled task: %#v err=%v", task, err)
	}
	spawned, err := client.Spawn(ctx, core.CleanupTaskName, core.JobTaskParams{JobID: job.ID, PreviousTaskID: job.CurrentTaskID},
		absurdruntime.TaskSpawnOptions(client.QueueName(), "cleanup:v3:"+job.ID))
	if err != nil || !spawned.Created {
		t.Fatalf("failed cleanup retained Absurd task: %#v err=%v", spawned, err)
	}
	remove()
	// Retain the probe as an older writer's unattached cleanup task, then race
	// requests that must reuse it without cancelling the winner.
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- store.ScheduleCleanup(ctx, client.QueueName(), job.ID, "") }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	history, err := store.JobTasks(ctx, job.ID)
	if err != nil || len(history) != 2 || history[0].TaskID != job.CurrentTaskID || history[1].TaskName != core.CleanupTaskName || history[1].TaskID != spawned.TaskID {
		t.Fatalf("cleanup history=%#v err=%v", history, err)
	}
	task, err = client.FetchTaskResult(ctx, client.QueueName(), job.CurrentTaskID)
	if err != nil || task == nil || task.State != absurd.TaskCancelled {
		t.Fatalf("predecessor=%#v err=%v", task, err)
	}
	task, err = client.FetchTaskResult(ctx, client.QueueName(), spawned.TaskID)
	if err != nil || task == nil || task.State != absurd.TaskPending {
		t.Fatalf("winning cleanup=%#v err=%v", task, err)
	}
}

func TestAtomicOrdinaryTaskHandoffPreservesHistoryAndPredecessor(t *testing.T) {
	_, store, _ := testDatabase(t)
	client := newFaultClient(t, store, fmt.Sprintf("dorf_atomic_handoff_%d", time.Now().UnixNano()))
	application := core.Application{Store: store, Tasks: client}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, _, err := store.AdmitDirect(ctx, atomicAdmissionInput(t), client.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	client.MustRegister(absurd.Task(direct.TaskName, func(ctx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		if err := application.VerifyAttachedTask(ctx, job.ID, direct.TaskName, params.PreviousTaskID); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("predecessor retained execution authority after handoff")
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "superseded"}, nil
	}))
	client.MustRegister(absurd.Task("handoff", func(ctx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		if params.PreviousTaskID != job.CurrentTaskID {
			return core.TaskResultV1{}, fmt.Errorf("handoff predecessor=%q", params.PreviousTaskID)
		}
		if err := application.VerifyAttachedTask(ctx, job.ID, "handoff", params.PreviousTaskID); err != nil {
			return core.TaskResultV1{}, err
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "handoff"}, nil
	}))
	var current core.Job
	for range 2 {
		current, err = application.ScheduleJobTask(ctx, job, "handoff", "handoff:"+job.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	history, err := store.JobTasks(ctx, job.ID)
	if err != nil || len(history) != 2 || history[0].TaskID != job.CurrentTaskID || history[1].TaskID != current.CurrentTaskID {
		t.Fatalf("handoff history=%#v err=%v", history, err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "handoff", BatchSize: 2, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	task, err := client.FetchTaskResult(ctx, client.QueueName(), current.CurrentTaskID)
	if err != nil || task == nil || task.State != absurd.TaskCompleted {
		t.Fatalf("handoff task=%#v err=%v", task, err)
	}
	task, err = client.FetchTaskResult(ctx, client.QueueName(), job.CurrentTaskID)
	if err != nil || task == nil || task.State != absurd.TaskCompleted {
		t.Fatalf("predecessor task=%#v err=%v", task, err)
	}
}

func TestExecutingTaskCanRequestAtomicCleanupWithoutCancellingItself(t *testing.T) {
	_, store, _ := testDatabase(t)
	client := newFaultClient(t, store, fmt.Sprintf("dorf_atomic_self_%d", time.Now().UnixNano()))
	application := core.Application{Store: store, Tasks: client}
	client.MustRegister(absurd.Task(direct.TaskName, func(ctx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		handle, err := application.OpenJob(ctx, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		if err := handle.RequestCleanup(ctx); err != nil {
			return core.TaskResultV1{}, err
		}
		if err := application.VerifyAttachedTask(ctx, params.JobID, direct.TaskName, params.PreviousTaskID); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("old task retained execution authority after requesting cleanup")
		}
		return core.TaskResultV1{JobID: params.JobID, Outcome: "cleanup-requested"}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job, _, err := store.AdmitDirect(ctx, atomicAdmissionInput(t), client.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "self-cleanup", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	task, err := client.FetchTaskResult(ctx, client.QueueName(), job.CurrentTaskID)
	if err != nil || task == nil || task.State != absurd.TaskCompleted {
		t.Fatalf("requesting task=%#v err=%v", task, err)
	}
	current, err := store.Job(ctx, job.ID)
	if err != nil || current.AdmissionOpen || current.CleanupState != core.CleanupScheduled || current.CurrentTaskID == job.CurrentTaskID {
		t.Fatalf("self cleanup did not commit: %#v err=%v", current, err)
	}
}
