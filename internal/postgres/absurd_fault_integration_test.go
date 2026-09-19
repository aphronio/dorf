package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	"github.com/jackc/pgx/v5"
)

type retryProofResult struct {
	SessionID string `json:"session_id"`
}

func TestRetryFailedSessionSchedulesOneMoreAttemptOnSameTask(t *testing.T) {
	_, store, defaultClient := testDatabase(t)
	defaultClient.Close()
	ctx := context.Background()
	queueName := fmt.Sprintf("dorf_retry_%d", time.Now().UnixNano())
	client := newFaultClient(t, store, queueName)
	const taskName = "dorf-retry-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(_ context.Context, params faultActionParams) (retryProofResult, error) {
		return retryProofResult{SessionID: params.SessionID}, errors.New("operator-repairable outage")
	}, absurd.TaskOptions{DefaultMaxAttempts: 1}))

	session := admitFaultSession(t, store, fmt.Sprintf("retry-%d", time.Now().UnixNano()))
	spawned, err := client.Spawn(ctx, taskName, faultActionParams{SessionID: session.ID}, absurd.SpawnOptions{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := attachTaskFixture(store, ctx, session.ID, spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	session, err = store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "retry-proof-first", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	failed, err := client.FetchTaskResult(ctx, queueName, session.CurrentTaskID)
	if err != nil || failed == nil || failed.State != absurd.TaskFailed {
		t.Fatalf("failed task=%#v err=%v", failed, err)
	}
	requestKey := "retry-request-" + session.ID
	receipt, err := (core.Application{Store: store, Tasks: client}).RetryFailedSession(ctx, session.ID, requestKey)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RequestKey != requestKey || receipt.SessionID != session.ID || receipt.TaskID != session.CurrentTaskID || receipt.Retry != "scheduled" || receipt.RunID == "" || receipt.Attempt != 2 || !receipt.Created {
		t.Fatalf("retry receipt=%#v", receipt)
	}
	replayed, err := (core.Application{Store: store, Tasks: client}).RetryFailedSession(ctx, session.ID, requestKey)
	if err != nil || replayed.RequestKey != receipt.RequestKey || replayed.SessionID != receipt.SessionID || replayed.TaskID != receipt.TaskID || replayed.RunID != receipt.RunID || replayed.Attempt != receipt.Attempt || replayed.Created {
		t.Fatalf("retry replay=%#v original=%#v err=%v", replayed, receipt, err)
	}
	other := admitFaultSession(t, store, fmt.Sprintf("retry-conflict-%d", time.Now().UnixNano()))
	if _, err := (core.Application{Store: store, Tasks: client}).RetryFailedSession(ctx, other.ID, requestKey); !errors.Is(err, core.ErrRetryReplayConflict) {
		t.Fatalf("changed Session replay error=%v", err)
	}
	pending, err := client.FetchTaskResult(ctx, queueName, session.CurrentTaskID)
	if err != nil || pending == nil || pending.State != absurd.TaskPending {
		t.Fatalf("scheduled task=%#v err=%v", pending, err)
	}
	if _, err := (core.Application{Store: store, Tasks: client}).RetryFailedSession(ctx, session.ID, requestKey+"-new"); !errors.Is(err, core.ErrRetryNotEligible) {
		t.Fatalf("non-failed retry error=%v", err)
	}
	after, err := store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != session {
		t.Fatalf("retry mutated Dorf Session facts: before=%#v after=%#v", session, after)
	}
}

func TestRetryFailedSessionTargetsAttachedCleanupTask(t *testing.T) {
	_, store, defaultClient := testDatabase(t)
	defaultClient.Close()
	ctx := context.Background()
	queueName := fmt.Sprintf("dorf_cleanup_retry_%d", time.Now().UnixNano())
	client := newFaultClient(t, store, queueName)
	const taskName = core.CleanupTaskName
	client.MustRegister(absurd.Task(taskName, func(_ context.Context, params faultActionParams) (retryProofResult, error) {
		return retryProofResult{SessionID: params.SessionID}, errors.New("operator-repairable cleanup outage")
	}, absurd.TaskOptions{DefaultMaxAttempts: 1}))

	session := admitFaultSession(t, store, fmt.Sprintf("cleanup-retry-%d", time.Now().UnixNano()))
	mainTaskID := "main-task-" + session.ID
	if err := attachTaskFixture(store, ctx, session.ID, mainTaskID, "dorf-main-proof-v1"); err != nil {
		t.Fatal(err)
	}
	session, err := store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := requestCleanupFixture(ctx, store, session.ID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, taskName, faultActionParams{SessionID: session.ID}, absurd.SpawnOptions{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := attachTaskFixture(store, ctx, session.ID, spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "cleanup-retry-proof-first", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := client.FetchTaskResult(ctx, queueName, before.CurrentTaskID)
	if err != nil || failed == nil || failed.State != absurd.TaskFailed {
		t.Fatalf("failed cleanup task=%#v err=%v", failed, err)
	}
	attachments, err := store.SessionTasks(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 2 || attachments[0].TaskID != mainTaskID || attachments[0].Sequence != 1 ||
		attachments[1].TaskID != before.CurrentTaskID || attachments[1].TaskName != taskName || attachments[1].Sequence != 2 {
		t.Fatalf("ordered task attachments=%#v", attachments)
	}

	receipt, err := (core.Application{Store: store, Tasks: client}).RetryFailedSession(ctx, session.ID, "cleanup-retry-"+session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SessionID != session.ID || receipt.TaskID != before.CurrentTaskID || receipt.Retry != "scheduled" || receipt.RunID == "" || receipt.Attempt != 2 {
		t.Fatalf("cleanup retry receipt=%#v", receipt)
	}
	pending, err := client.FetchTaskResult(ctx, queueName, before.CurrentTaskID)
	if err != nil || pending == nil || pending.State != absurd.TaskPending {
		t.Fatalf("scheduled cleanup task=%#v err=%v", pending, err)
	}
	after, err := store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("cleanup retry mutated Dorf Session facts: before=%#v after=%#v", before, after)
	}
}

type faultActionParams struct {
	SessionID string `json:"session_id"`
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

// faultActionExternals controls only the Sandbox-create effect exercised by
// this fault story. The nil embedded interface makes any unexpected external
// call fail the test instead of teaching this focused fake unrelated behavior.
type faultActionExternals struct {
	core.Externals
	effect *reconcilingFaultEffect
	runID  string
}

func (e faultActionExternals) SandboxCreate(context.Context, core.Session, core.Sandbox) (string, error) {
	e.effect.reconcile(e.runID)
	return "provider-fault-resource", nil
}

func sandboxCreateAction(actions []core.Action) (core.Action, bool) {
	for _, action := range actions {
		if action.Kind == core.ActionSandboxCreate {
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
			session, err := store.Session(workCtx, params.SessionID)
			if err != nil {
				return faultActionResultV1{}, err
			}
			sandbox, err := store.Sandbox(workCtx, core.MainSandboxName(params.SessionID))
			if err != nil {
				return faultActionResultV1{}, err
			}
			runID := task.RunID()
			execution := core.NewExecutionService(
				store,
				faultActionExternals{effect: effect, runID: runID},
				nil,
				func(claimCtx context.Context) error {
					err := absurdruntime.RequireClaim(claimCtx)
					effect.recordClaim(runID, err)
					return err
				},
			)
			if err := execution.ExecuteSandboxAction(workCtx, session.ID, sandbox.ID, core.ActionSandboxCreate); err != nil {
				return faultActionResultV1{}, err
			}
			return faultActionResultV1{ActionID: core.ScopedActionID(session.ID, core.ActionSandboxCreate, sandbox.ID)}, nil
		})
		if err != nil {
			return faultActionResultV1{}, err
		}
		return step.CompleteStep(ctx, result)
	}, absurd.TaskOptions{DefaultMaxAttempts: 2}))
}

func admitFaultSession(t *testing.T, store postgres.Store, suffix string) core.Session {
	t.Helper()
	session, created, err := admitDirectFixture(t, store, context.Background(), directSessionInput(
		"absurd-fault-"+suffix,
	))
	if err != nil || !created {
		t.Fatalf("admit fault Session=%#v created=%v err=%v", session, created, err)
	}
	return session
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
	session := admitFaultSession(t, store, fmt.Sprintf("cancel-%d", time.Now().UnixNano()))
	spawned, err := client.Spawn(context.Background(), taskName, faultActionParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: "cancel:" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := attachTaskFixture(store, context.Background(), session.ID, spawned.TaskID, taskName); err != nil {
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
	actions, actionsErr := store.Actions(context.Background(), session.ID)
	action, found := sandboxCreateAction(actions)
	passed, failed := effect.claims()
	if err != nil || actionsErr != nil || snapshot == nil || snapshot.State != absurd.TaskCancelled || !found || action.State != core.ActionUnsettled || effect.mutationCount() != 1 || len(passed) != 1 || passed[0] != firstRunID || len(failed) != 1 || failed[0] != firstRunID {
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

func workUntilTaskCompleted(ctx context.Context, client *absurd.Client, queueName, taskID string, options absurd.WorkBatchOptions) error {
	for {
		if err := client.WorkBatch(ctx, options); err != nil {
			return err
		}
		snapshot, err := client.FetchTaskResult(ctx, queueName, taskID)
		if err != nil {
			return err
		}
		if snapshot != nil && snapshot.State == absurd.TaskCompleted {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestAbsurdClaimExpirySandboxEffectFenceSerializesCleanupWithoutLateReceipt(t *testing.T) {
	_, store, defaultClient := testDatabase(t)
	defaultClient.Close()
	queueName := fmt.Sprintf("dorf_fault_claim_%d", time.Now().UnixNano())
	client := newFaultClient(t, store, queueName)
	effect := newReconcilingFaultEffect()
	t.Cleanup(effect.release)
	taskName := "dorf-fault-claim-v1"
	registerFaultActionTask(client, store, taskName, effect)
	session := admitFaultSession(t, store, fmt.Sprintf("claim-%d", time.Now().UnixNano()))
	spawned, err := client.Spawn(context.Background(), taskName, faultActionParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: "claim:" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := attachTaskFixture(store, context.Background(), session.ID, spawned.TaskID, taskName); err != nil {
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
	if err := client.CancelTask(context.Background(), queueName, spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	application := core.Application{Store: store, Tasks: client}
	application.RegisterCleanup()
	handle, err := application.OpenSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- handle.RequestCleanup(context.Background()) }()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup crossed an in-flight repository effect fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	effect.release()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	actions, err := store.Actions(context.Background(), session.ID)
	action, found := sandboxCreateAction(actions)
	passed, failed := effect.claims()
	closed, sessionErr := store.Session(context.Background(), session.ID)
	if err != nil || sessionErr != nil || !found || action.State != core.ActionUnsettled || effect.mutationCount() != 1 || len(passed) != 1 || passed[0] != firstRunID || len(failed) != 1 || failed[0] != firstRunID || closed.AdmissionOpen || closed.CleanupState != core.CleanupScheduled {
		t.Fatalf("serialized cleanup Session=%#v actions=%#v mutations=%d claims passed=%v failed=%v errors=%v/%v", closed, actions, effect.mutationCount(), passed, failed, err, sessionErr)
	}
}
