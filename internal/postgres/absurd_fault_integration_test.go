package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	"github.com/jackc/pgx/v5"
)

type retryProofResult struct {
	JobID string `json:"job_id"`
}

func TestRetryFailedJobSchedulesOneMoreAttemptOnSameTask(t *testing.T) {
	_, store, defaultClient := testDatabase(t)
	defaultClient.Close()
	ctx := context.Background()
	queueName := fmt.Sprintf("dorf_retry_%d", time.Now().UnixNano())
	client := newFaultClient(t, store, queueName)
	const taskName = "dorf-retry-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(_ context.Context, params faultActionParams) (retryProofResult, error) {
		return retryProofResult{JobID: params.JobID}, errors.New("operator-repairable outage")
	}, absurd.TaskOptions{DefaultMaxAttempts: 1}))

	job := admitFaultJob(t, store, fmt.Sprintf("retry-%d", time.Now().UnixNano()))
	spawned, err := client.Spawn(ctx, taskName, faultActionParams{JobID: job.ID}, absurd.SpawnOptions{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, job.CurrentTaskID, spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "retry-proof-first", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	failed, err := client.FetchTaskResult(ctx, queueName, job.CurrentTaskID)
	if err != nil || failed == nil || failed.State != absurd.TaskFailed {
		t.Fatalf("failed task=%#v err=%v", failed, err)
	}
	receipt, err := (core.Application{Store: store, Tasks: client}).RetryFailedJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.JobID != job.ID || receipt.TaskID != job.CurrentTaskID || receipt.Retry != "scheduled" || receipt.RunID == "" || receipt.Attempt != 2 {
		t.Fatalf("retry receipt=%#v", receipt)
	}
	pending, err := client.FetchTaskResult(ctx, queueName, job.CurrentTaskID)
	if err != nil || pending == nil || pending.State != absurd.TaskPending {
		t.Fatalf("scheduled task=%#v err=%v", pending, err)
	}
	if _, err := (core.Application{Store: store, Tasks: client}).RetryFailedJob(ctx, job.ID); err == nil || !strings.Contains(err.Error(), "not currently failed") {
		t.Fatalf("non-failed retry error=%v", err)
	}
	after, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != job {
		t.Fatalf("retry mutated Dorf Job facts: before=%#v after=%#v", job, after)
	}
}

func TestRetryFailedJobTargetsAttachedCleanupTask(t *testing.T) {
	_, store, defaultClient := testDatabase(t)
	defaultClient.Close()
	ctx := context.Background()
	queueName := fmt.Sprintf("dorf_cleanup_retry_%d", time.Now().UnixNano())
	client := newFaultClient(t, store, queueName)
	const taskName = "dorf-cleanup-retry-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(_ context.Context, params faultActionParams) (retryProofResult, error) {
		return retryProofResult{JobID: params.JobID}, errors.New("operator-repairable cleanup outage")
	}, absurd.TaskOptions{DefaultMaxAttempts: 1}))

	job := admitFaultJob(t, store, fmt.Sprintf("cleanup-retry-%d", time.Now().UnixNano()))
	mainTaskID := "main-task-" + job.ID
	if err := store.AttachJobTask(ctx, job.ID, "", mainTaskID, "dorf-main-proof-v1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAdmissionForCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, taskName, faultActionParams{JobID: job.ID}, absurd.SpawnOptions{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachCleanupTask(ctx, job.ID, job.CurrentTaskID, spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "cleanup-retry-proof-first", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := client.FetchTaskResult(ctx, queueName, before.CurrentTaskID)
	if err != nil || failed == nil || failed.State != absurd.TaskFailed {
		t.Fatalf("failed cleanup task=%#v err=%v", failed, err)
	}
	attachments, err := store.JobTasks(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 2 || attachments[0].TaskID != mainTaskID || attachments[0].Sequence != 1 ||
		attachments[1].TaskID != before.CurrentTaskID || attachments[1].TaskName != taskName || attachments[1].Sequence != 2 {
		t.Fatalf("ordered task attachments=%#v", attachments)
	}

	receipt, err := (core.Application{Store: store, Tasks: client}).RetryFailedJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.JobID != job.ID || receipt.TaskID != before.CurrentTaskID || receipt.Retry != "scheduled" || receipt.RunID == "" || receipt.Attempt != 2 {
		t.Fatalf("cleanup retry receipt=%#v", receipt)
	}
	pending, err := client.FetchTaskResult(ctx, queueName, before.CurrentTaskID)
	if err != nil || pending == nil || pending.State != absurd.TaskPending {
		t.Fatalf("scheduled cleanup task=%#v err=%v", pending, err)
	}
	after, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("cleanup retry mutated Dorf Job facts: before=%#v after=%#v", before, after)
	}
}

type faultActionParams struct {
	JobID string `json:"job_id"`
}

type faultActionResultV1 struct {
	ActionID string `json:"action_id"`
}

// reconcilingFaultEffect models an authority that accepts one logical effect
// under a stable Action identity. A replacement attempt observes and adopts
// that accepted effect instead of issuing it twice.
type reconcilingFaultEffect struct {
	mu           sync.Mutex
	accepted     bool
	mutations    int
	claimPassed  []string
	claimFailed  []string
	firstRun     chan string
	releaseFirst chan struct{}
	releaseOnce  sync.Once
}

func (e *reconcilingFaultEffect) release() {
	e.releaseOnce.Do(func() { close(e.releaseFirst) })
}

func newReconcilingFaultEffect() *reconcilingFaultEffect {
	return &reconcilingFaultEffect{firstRun: make(chan string, 1), releaseFirst: make(chan struct{})}
}

func (e *reconcilingFaultEffect) reconcile(runID string) {
	e.mu.Lock()
	first := !e.accepted
	if first {
		e.accepted = true
		e.mutations++
	}
	e.mu.Unlock()
	if first {
		e.firstRun <- runID
		<-e.releaseFirst
	}
}

func (e *reconcilingFaultEffect) mutationCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mutations
}

func (e *reconcilingFaultEffect) recordClaim(runID string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.claimPassed = append(e.claimPassed, runID)
		return
	}
	e.claimFailed = append(e.claimFailed, runID)
}

func (e *reconcilingFaultEffect) claims() (passed, failed []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.claimPassed...), append([]string(nil), e.claimFailed...)
}

// faultActionExternals controls only the repository-clone effect exercised by
// this fault story. The nil embedded interface makes any unexpected external
// call fail the test instead of teaching this focused fake unrelated behavior.
type faultActionExternals struct {
	core.Externals
	gitworkspace.GitExternals
	effect *reconcilingFaultEffect
	runID  string
}

func (e faultActionExternals) RepositoryClone(context.Context, core.Job, core.Sandbox, string, string, string) error {
	e.effect.reconcile(e.runID)
	return nil
}

func repositoryCloneAction(actions []core.Action) (core.Action, bool) {
	for _, action := range actions {
		if action.Kind == gitworkspace.ActionRepositoryClone {
			return action, true
		}
	}
	return core.Action{}, false
}

func registerFaultActionTask(client *absurd.Client, store postgres.Store, taskName string, effect *reconcilingFaultEffect) {
	client.MustRegister(absurd.Task(taskName, func(ctx context.Context, params faultActionParams) (faultActionResultV1, error) {
		step, err := absurd.BeginStep[faultActionResultV1](ctx, "dorf/fault-action/v1")
		if err != nil || step.Done {
			return step.State, err
		}
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return faultActionResultV1{}, absurd.ErrNoTaskContext
		}
		result, err := absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (faultActionResultV1, error) {
			job, err := store.CodingJob(workCtx, params.JobID)
			if err != nil {
				return faultActionResultV1{}, err
			}
			sandbox, err := store.Sandbox(workCtx, core.MainSandboxName(params.JobID))
			if err != nil {
				return faultActionResultV1{}, err
			}
			action, err := store.GetOrCreateSandboxAction(workCtx, core.MainSandboxName(params.JobID), gitworkspace.ActionRepositoryClone)
			if err != nil {
				return faultActionResultV1{}, err
			}
			runID := task.RunID()
			execution := core.NewExecutionService(
				store,
				faultActionExternals{effect: effect, runID: runID},
				blob.Store{},
				nil,
				func(claimCtx context.Context) error {
					err := absurdruntime.RequireClaim(claimCtx)
					effect.recordClaim(runID, err)
					return err
				},
			)
			service := gitworkspace.NewExecutor(execution, store, faultActionExternals{effect: effect, runID: runID}, func(claimCtx context.Context) error {
				err := absurdruntime.RequireClaim(claimCtx)
				effect.recordClaim(runID, err)
				return err
			})
			if err := service.ExecuteRepositoryClone(workCtx, job.Job, sandbox, action, job.Repository, job.Revision, job.Branch); err != nil {
				return faultActionResultV1{}, err
			}
			return faultActionResultV1{ActionID: action.ID}, nil
		})
		if err != nil {
			return faultActionResultV1{}, err
		}
		return step.CompleteStep(ctx, result)
	}, absurd.TaskOptions{DefaultMaxAttempts: 2}))
}

func admitFaultJob(t *testing.T, store postgres.Store, suffix string) core.Job {
	t.Helper()
	job, created, err := store.AdmitCoding(context.Background(), codingJobInput(
		"absurd-fault-"+suffix,
		"prove late work cannot duplicate one logical external effect",
		"2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c",
		"dorf/absurd-fault-"+suffix,
	))
	if err != nil || !created {
		t.Fatalf("admit fault Job=%#v created=%v err=%v", job, created, err)
	}
	return job
}

func newFaultClient(t *testing.T, dbStore postgres.Store, queueName string) *absurd.Client {
	t.Helper()
	client, err := absurd.New(absurd.Options{DB: dbStore.DB, QueueName: queueName})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CreateQueue(context.Background(), queueName); err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.DropQueue(context.Background(), queueName)
		_ = client.Close()
	})
	return client
}

func TestAbsurdCancellationCannotRecordLateActionSuccess(t *testing.T) {
	_, store, defaultClient := testDatabase(t)
	defaultClient.Close()
	queueName := fmt.Sprintf("dorf_fault_cancel_%d", time.Now().UnixNano())
	client := newFaultClient(t, store, queueName)
	effect := newReconcilingFaultEffect()
	t.Cleanup(effect.release)
	taskName := "dorf-fault-cancel-v1"
	registerFaultActionTask(client, store, taskName, effect)
	job := admitFaultJob(t, store, fmt.Sprintf("cancel-%d", time.Now().UnixNano()))
	spawned, err := client.Spawn(context.Background(), taskName, faultActionParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: "cancel:" + job.ID})
	if err != nil {
		t.Fatal(err)
	}

	workerDone := make(chan error, 1)
	go func() {
		workerDone <- client.WorkBatch(context.Background(), absurd.WorkBatchOptions{WorkerID: "fault-cancel", BatchSize: 1, ClaimTimeout: time.Minute})
	}()
	firstRunID := <-effect.firstRun
	if err := client.CancelTask(context.Background(), queueName, spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	effect.release()
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}

	snapshot, err := client.FetchTaskResult(context.Background(), queueName, spawned.TaskID)
	actions, actionsErr := store.Actions(context.Background(), job.ID)
	action, found := repositoryCloneAction(actions)
	passed, failed := effect.claims()
	if err != nil || actionsErr != nil || snapshot == nil || snapshot.State != absurd.TaskCancelled || !found || action.State != core.ActionUnsettled || effect.mutationCount() != 1 || len(passed) != 0 || len(failed) != 1 || failed[0] != firstRunID {
		t.Fatalf("cancelled snapshot=%#v actions=%#v mutations=%d claims passed=%v failed=%v errors=%v/%v", snapshot, actions, effect.mutationCount(), passed, failed, err, actionsErr)
	}
}

// forceAbsurd050ClaimExpiry is the one intentional white-box fault hook in
// Dorf's tests. Absurd 0.5.0 has no public lease-expiry injector; all behavior
// assertions remain on public task results and Dorf Action facts.
func forceAbsurd050ClaimExpiry(ctx context.Context, store postgres.Store, queueName, runID string) error {
	runsTable := pgx.Identifier{"absurd", "r_" + queueName}.Sanitize()
	result, err := store.DB.ExecContext(ctx, fmt.Sprintf(`update %s set claim_expires_at=clock_timestamp()-interval '1 second' where run_id=$1 and state='running'`, runsTable), runID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("Absurd 0.5.0 fault hook expired %d runs, want 1", rows)
	}
	return nil
}

func TestAbsurdClaimExpiryReconcilesEffectWithoutLateOverwrite(t *testing.T) {
	_, store, defaultClient := testDatabase(t)
	defaultClient.Close()
	queueName := fmt.Sprintf("dorf_fault_claim_%d", time.Now().UnixNano())
	client := newFaultClient(t, store, queueName)
	effect := newReconcilingFaultEffect()
	t.Cleanup(effect.release)
	taskName := "dorf-fault-claim-v1"
	registerFaultActionTask(client, store, taskName, effect)
	job := admitFaultJob(t, store, fmt.Sprintf("claim-%d", time.Now().UnixNano()))
	spawned, err := client.Spawn(context.Background(), taskName, faultActionParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: "claim:" + job.ID})
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- client.WorkBatch(context.Background(), absurd.WorkBatchOptions{WorkerID: "fault-first", BatchSize: 1, ClaimTimeout: time.Minute})
	}()
	firstRunID := <-effect.firstRun
	if err := forceAbsurd050ClaimExpiry(context.Background(), store, queueName, firstRunID); err != nil {
		t.Fatal(err)
	}
	var snapshot *absurd.TaskResultSnapshot
	for range 10 {
		if err := client.WorkBatch(context.Background(), absurd.WorkBatchOptions{WorkerID: "fault-replacement", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
		snapshot, err = client.FetchTaskResult(context.Background(), queueName, spawned.TaskID)
		if err != nil || snapshot != nil && snapshot.State == absurd.TaskCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || snapshot == nil || snapshot.State != absurd.TaskCompleted {
		t.Fatalf("replacement public result=%#v err=%v", snapshot, err)
	}
	effect.release()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	actions, err := store.Actions(context.Background(), job.ID)
	action, found := repositoryCloneAction(actions)
	passed, failed := effect.claims()
	if err != nil || !found || action.State != core.ActionSucceeded || effect.mutationCount() != 1 || len(passed) != 1 || passed[0] == firstRunID || len(failed) != 1 || failed[0] != firstRunID {
		t.Fatalf("reconciled actions=%#v mutations=%d claims passed=%v failed=%v err=%v", actions, effect.mutationCount(), passed, failed, err)
	}
}
