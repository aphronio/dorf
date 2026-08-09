package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	publicationapp "github.com/aphronio/dorf/internal/publication"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func readyPublicationJob(t *testing.T, store postgres.Store) spine.Job {
	t.Helper()
	input := postgres.NewJob{
		AdmissionKey:         fmt.Sprintf("publication-public-%d", time.Now().UnixNano()),
		Goal:                 "publish through the public Absurd boundary",
		Repository:           "https://github.com/aphronio/dorf.git",
		Revision:             strings.Repeat("a", 40),
		Branch:               "dorf/public-absurd-publication",
		ProviderConnection:   "primary",
		ProviderGatewayState: "/tmp/dorf-provider-gateway-test",
		Model:                "gpt-5.6-sol",
		ReasoningEffort:      "high",
		GitHubRepository:     "aphronio/dorf",
		GitHubInstallation:   "42",
		BaseBranch:           "greenfield",
	}
	job, created, err := store.Admit(context.Background(), input)
	if err != nil || !created {
		t.Fatalf("admit job=%#v created=%v err=%v", job, created, err)
	}
	if _, err := store.DB.ExecContext(context.Background(), `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	job, err = store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestPostgresPublicationScheduleUsesOneStablePublicSpawn(t *testing.T) {
	_, store, client := testDatabase(t)
	publicationapp.Register(client, publicationapp.Service{Store: store})
	ctx := context.Background()
	job := readyPublicationJob(t, store)

	params, taskID, created, err := publicationapp.Schedule(ctx, store, client, nil, job.ID, job.Revision)
	if err != nil || !created || taskID == "" || params.JobID != job.ID || params.Revision != job.Revision {
		t.Fatalf("first schedule params=%#v task=%s created=%v err=%v", params, taskID, created, err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, taskID) })
	repeated, repeatedID, repeatedCreated, err := publicationapp.Schedule(ctx, store, client, nil, job.ID, job.Revision)
	if err != nil || repeatedCreated || repeated != params || repeatedID != taskID {
		t.Fatalf("repeated schedule params=%#v task=%s created=%v err=%v", repeated, repeatedID, repeatedCreated, err)
	}
	snapshot, err := client.FetchTaskResult(ctx, config.QueueName, taskID)
	if err != nil || snapshot == nil || snapshot.State != absurd.TaskPending {
		t.Fatalf("public task snapshot=%#v err=%v", snapshot, err)
	}
	stored, err := store.Job(ctx, job.ID)
	push, pull, actionsErr := store.PublicationActions(ctx, job.ID, job.Revision)
	if err != nil || actionsErr != nil || stored.PublicationTaskID != taskID || stored.WorkflowPhase != "publishing" || push.ID == "" || pull.ID == "" {
		t.Fatalf("stored=%#v push=%#v pull=%#v errors=%v/%v", stored, push, pull, err, actionsErr)
	}
}

func TestPublicationWorkerRecoversPublicSpawnBeforeDorfAttachment(t *testing.T) {
	_, store, client := testDatabase(t)
	publicationapp.Register(client, publicationapp.Service{Store: store})
	ctx := context.Background()
	job := readyPublicationJob(t, store)
	if err := store.WithJobFence(ctx, job.ID, func() error {
		_, _, _, spawn, err := store.BeginPublication(ctx, job.ID, job.Revision)
		if err != nil {
			return err
		}
		if !spawn {
			return fmt.Errorf("publication intent did not require its stable public Spawn")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, publicationapp.TaskName, publicationapp.Params{JobID: job.ID, Revision: job.Revision}, absurd.SpawnOptions{IdempotencyKey: postgres.PublicationTaskKey(job.ID, job.Revision)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, spawned.TaskID) })
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "publication-spawn-before-attach", BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	attached, err := store.Job(ctx, job.ID)
	if err != nil || attached.PublicationTaskID != spawned.TaskID {
		t.Fatalf("publication worker attachment=%#v spawn=%#v err=%v", attached, spawned, err)
	}
}

func TestPostgresPublicationRetryIsPublicInPlaceAndPreservesActions(t *testing.T) {
	_, store, client := testDatabase(t)
	publicationapp.Register(client, publicationapp.Service{Store: store})
	ctx := context.Background()
	job := readyPublicationJob(t, store)
	_, taskID, _, err := publicationapp.Schedule(ctx, store, client, nil, job.ID, job.Revision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, taskID) })
	pushBefore, pullBefore, err := store.PublicationActions(ctx, job.ID, job.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BlockPublication(ctx, job.ID, job.Revision, "operator-remediable publication conflict"); err != nil {
		t.Fatal(err)
	}
	for range postgres.PublicationTaskMaxAttempts {
		if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "publication-public-retry", ClaimTimeout: 30 * time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	failed, err := client.FetchTaskResult(ctx, config.QueueName, taskID)
	if err != nil || failed == nil || failed.State != absurd.TaskFailed {
		t.Fatalf("failed snapshot=%#v err=%v", failed, err)
	}
	params, retried, err := publicationapp.Retry(ctx, store, client, job.ID, job.Revision)
	if err != nil || params.JobID != job.ID || retried.TaskID != taskID || retried.Created || retried.Attempt != postgres.PublicationTaskMaxAttempts+1 {
		t.Fatalf("retry params=%#v result=%#v err=%v", params, retried, err)
	}
	pending, err := client.FetchTaskResult(ctx, config.QueueName, taskID)
	pushAfter, pullAfter, actionsErr := store.PublicationActions(ctx, job.ID, job.Revision)
	stored, storeErr := store.Job(ctx, job.ID)
	if err != nil || actionsErr != nil || storeErr != nil || pending == nil || pending.State != absurd.TaskPending || stored.WorkflowPhase != "publishing" || pushAfter.ID != pushBefore.ID || pullAfter.ID != pullBefore.ID {
		t.Fatalf("pending=%#v stored=%#v actions=%#v/%#v errors=%v/%v/%v", pending, stored, pushAfter, pullAfter, err, actionsErr, storeErr)
	}
}
