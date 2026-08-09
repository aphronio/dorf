package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	"github.com/jackc/pgx/v5"
)

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

func (e *reconcilingFaultEffect) reconcile(runID, actionID string) spine.Receipt {
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
	return spine.Receipt{ExternalID: "accepted:" + actionID}
}

func (e *reconcilingFaultEffect) mutationCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mutations
}

func repositoryCloneAction(actions []postgres.ActionView) (postgres.ActionView, bool) {
	for _, action := range actions {
		if action.Kind == spine.ActionRepositoryClone {
			return action, true
		}
	}
	return postgres.ActionView{}, false
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
			action, err := store.BeginAction(workCtx, params.JobID, spine.ActionRepositoryClone)
			if err != nil {
				return faultActionResultV1{}, err
			}
			receipt := effect.reconcile(task.RunID(), action.ID)
			if err := store.CompleteAction(workCtx, action.ID, receipt); err != nil {
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

func admitFaultJob(t *testing.T, store postgres.Store, suffix string) spine.Job {
	t.Helper()
	job, created, err := store.Admit(context.Background(), postgres.NewJob{
		AdmissionKey:         "absurd-fault-" + suffix,
		Goal:                 "prove late work cannot duplicate one logical external effect",
		Repository:           "https://github.com/aphronio/dorf.git",
		Revision:             "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c",
		Branch:               "dorf/absurd-fault-" + suffix,
		GitHubRepository:     "aphronio/dorf",
		GitHubInstallation:   "42",
		BaseBranch:           "greenfield",
		ProviderConnection:   "primary",
		ProviderGatewayState: "/tmp/dorf-provider-gateway-test",
		Model:                "gpt-5.6-sol",
		ReasoningEffort:      "high",
	})
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

func TestAbsurdCancellationKeepsLateReconciledActionSingleAndTruthful(t *testing.T) {
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
	<-effect.firstRun
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
	if err != nil || actionsErr != nil || snapshot == nil || snapshot.State != absurd.TaskCancelled || !found || action.State != spine.ActionSucceeded || action.ExternalID != "accepted:"+action.ID || effect.mutationCount() != 1 {
		t.Fatalf("cancelled snapshot=%#v actions=%#v mutations=%d errors=%v/%v", snapshot, actions, effect.mutationCount(), err, actionsErr)
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
	if err != nil || !found || action.State != spine.ActionSucceeded || action.ExternalID != "accepted:"+action.ID || action.Attempts != 2 || effect.mutationCount() != 1 {
		t.Fatalf("reconciled actions=%#v mutations=%d err=%v", actions, effect.mutationCount(), err)
	}
}
