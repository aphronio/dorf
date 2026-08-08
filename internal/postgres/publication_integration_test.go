package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	publicationapi "github.com/aphronio/dorf/internal/publication"
	"github.com/aphronio/dorf/internal/spine"
)

func publicationInput(key string) postgres.NewJob {
	return postgres.NewJob{
		AdmissionKey: key, Goal: "publish one exact Revision", Repository: "https://github.com/aphronio/dorf.git",
		Revision: strings.Repeat("a", 40), Branch: "dorf/issue-43", ProviderConnection: "primary",
		Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf",
		GitHubInstallation: "42", BaseBranch: "greenfield",
	}
}

func TestPostgresPublicationIdentityFreshnessAndSamePullRequestRefresh(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	publicationapi.Register(client, publicationapi.Service{Store: store})
	input := publicationInput(fmt.Sprintf("publication-integration-%d", time.Now().UnixNano()))
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	params, taskID, created, err := publicationapi.Schedule(ctx, store, client, job.ID, input.Revision)
	if err != nil || !created || params.Attempt != 0 {
		t.Fatalf("schedule params=%#v task=%s created=%v err=%v", params, taskID, created, err)
	}
	taskIDs := []string{taskID}
	t.Cleanup(func() {
		for _, id := range taskIDs {
			_ = client.CancelTask(context.Background(), config.QueueName, id)
		}
	})
	job, err = store.Job(ctx, job.ID)
	if err != nil || job.WorkflowPhase != "publishing" || job.PublicationTaskID != taskID {
		t.Fatalf("begin=%#v err=%v", job, err)
	}
	push, pull, err := store.PublicationActions(ctx, job.ID, input.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if push.ID != spine.ScopedActionID(job.ID, spine.ActionRepositoryPush, input.Revision) || pull.ID != spine.ScopedActionID(job.ID, spine.ActionGitHubPullRequest, input.Revision) {
		t.Fatal("publication Actions do not have stable Job/Revision identities")
	}
	repeatedParams, repeatedTaskID, repeatedCreated, err := publicationapi.Schedule(ctx, store, client, job.ID, input.Revision)
	repeatedPush, repeatedPull, actionErr := store.PublicationActions(ctx, job.ID, input.Revision)
	if err != nil || actionErr != nil || repeatedCreated || repeatedParams.Attempt != params.Attempt || repeatedTaskID != taskID || repeatedPush.ID != push.ID || repeatedPull.ID != pull.ID {
		t.Fatalf("repeated params=%#v task=%s created=%v push=%#v pull=%#v err=%v actionErr=%v", repeatedParams, repeatedTaskID, repeatedCreated, repeatedPush, repeatedPull, err, actionErr)
	}
	if err := store.RecordPush(ctx, push.ID, input.Revision); err != nil {
		t.Fatal(err)
	}
	digest1 := strings.Repeat("1", 64)
	proposal := spine.GitHubProposal{JobID: job.ID, Repository: input.GitHubRepository, InstallationID: input.GitHubInstallation, BaseBranch: input.BaseBranch, HeadBranch: input.Branch, Number: 43, URL: "https://github.com/aphronio/dorf/pull/43", ProposedRevision: input.Revision, ObservedRemoteHead: input.Revision, BodyDigest: digest1}
	if err := store.RecordProposal(ctx, pull.ID, proposal); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Proposal(ctx, job.ID)
	if err != nil || stored == nil || stored.Stale || stored.Number != 43 || stored.BodyDigest != digest1 {
		t.Fatalf("proposal=%#v err=%v", stored, err)
	}
	_, sameTaskID, sameCreated, err := publicationapi.Schedule(ctx, store, client, job.ID, input.Revision)
	idempotent, jobErr := store.Job(ctx, job.ID)
	samePush, samePull, actionErr := store.PublicationActions(ctx, job.ID, input.Revision)
	if err != nil || jobErr != nil || actionErr != nil || sameCreated || sameTaskID != taskID || idempotent.WorkflowPhase != "published" || samePush.ID != push.ID || samePull.ID != pull.ID {
		t.Fatalf("published idempotency job=%#v task=%s created=%v push=%#v pull=%#v err=%v jobErr=%v actionErr=%v", idempotent, sameTaskID, sameCreated, samePush, samePull, err, jobErr, actionErr)
	}

	later := strings.Repeat("b", 40)
	if _, err := db.ExecContext(ctx, `insert into dorf.revisions(job_id,oid,parent_oid,branch,generation) values($1,$2,$3,$4,1)`, job.ID, later, input.Revision, input.Branch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.jobs set revision=$2,workflow_phase='ready' where id=$1`, job.ID, later); err != nil {
		t.Fatal(err)
	}
	stale, err := store.Proposal(ctx, job.ID)
	if err != nil || stale == nil || !stale.Stale {
		t.Fatalf("later Revision reused stale proposal proof: %#v err=%v", stale, err)
	}
	_, laterTaskID, laterCreated, err := publicationapi.Schedule(ctx, store, client, job.ID, later)
	taskIDs = append(taskIDs, laterTaskID)
	laterPush, laterPull, actionErr := store.PublicationActions(ctx, job.ID, later)
	if err != nil || actionErr != nil || !laterCreated || laterPush.ID == push.ID || laterPull.ID == pull.ID {
		t.Fatalf("later task=%s created=%v Actions push=%#v pull=%#v err=%v actionErr=%v", laterTaskID, laterCreated, laterPush, laterPull, err, actionErr)
	}
	if err := store.RecordPush(ctx, laterPush.ID, later); err != nil {
		t.Fatal(err)
	}
	digest2 := strings.Repeat("2", 64)
	proposal.ProposedRevision, proposal.ObservedRemoteHead, proposal.BodyDigest = later, later, digest2
	if err := store.RecordProposal(ctx, laterPull.ID, proposal); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.Proposal(ctx, job.ID)
	if err != nil || refreshed == nil || refreshed.Stale || refreshed.Number != 43 || refreshed.ProposedRevision != later || refreshed.BodyDigest != digest2 {
		t.Fatalf("same-PR refresh=%#v err=%v", refreshed, err)
	}
}

func TestPostgresFreshAdmissionRequiresCompleteGitHubAuthorityAndSchemaHasNoCredentialColumns(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	input := publicationInput(fmt.Sprintf("publication-admission-%d", time.Now().UnixNano()))
	input.BaseBranch = ""
	if _, _, err := store.Admit(ctx, input); err == nil || !strings.Contains(err.Error(), "explicit base branch") {
		t.Fatalf("missing base admission error=%v", err)
	}
	input = publicationInput(fmt.Sprintf("publication-scope-%d", time.Now().UnixNano()))
	input.GitHubInstallation = ""
	if _, _, err := store.Admit(ctx, input); err == nil {
		t.Fatal("partial GitHub installation authority was admitted")
	}
	var secretColumns int
	if err := db.QueryRowContext(ctx, `select count(*) from information_schema.columns where table_schema='dorf' and (column_name ~ '(github|access|installation)_token' or column_name ilike '%private_key%' or column_name ilike '%credential%' or column_name ilike '%password%')`).Scan(&secretColumns); err != nil {
		t.Fatal(err)
	}
	if secretColumns != 0 {
		t.Fatalf("Dorf PostgreSQL schema exposes %d credential-shaped columns", secretColumns)
	}
}

func TestPostgresGuardedDogfoodBindingIsNarrowImmutableAndIdempotent(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	jobID := fmt.Sprintf("job-dogfood-bind-%d", time.Now().UnixNano())
	revision := "a09e08da11cc89aacd8aee6d33a4a38c45d53824"
	branch := "dorf/issue-43-durable-github-publication"
	if _, err := db.ExecContext(ctx, `insert into dorf.jobs(id,admission_key,goal,repository,revision,starting_revision,branch,provider_connection,model,reasoning_effort) values($1,$2,'issue 43 dogfood','https://github.com/aphronio/dorf.git',$3,$3,$4,'primary','gpt-5.6-sol','high')`, jobID, jobID, revision, branch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.revisions(job_id,oid,branch,generation) values($1,$2,$3,0)`, jobID, revision, branch); err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindDogfoodPublicationAuthority(ctx, jobID, "aphronio/dorf", "42", "greenfield")
	if err != nil || bound.GitHubRepository != "aphronio/dorf" || bound.GitHubInstallation != "42" || bound.BaseBranch != "greenfield" {
		t.Fatalf("bound=%#v err=%v", bound, err)
	}
	repeated, err := store.BindDogfoodPublicationAuthority(ctx, jobID, "aphronio/dorf", "42", "greenfield")
	if err != nil || repeated.GitHubInstallation != "42" {
		t.Fatalf("idempotent bind=%#v err=%v", repeated, err)
	}
	if _, err := store.BindDogfoodPublicationAuthority(ctx, jobID, "aphronio/dorf", "43", "greenfield"); err == nil {
		t.Fatal("guarded dogfood authority was rebound")
	}
}

func TestPostgresExhaustedPublicationTaskAdvancesOneGenerationConcurrentlyAndPreservesActions(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	publicationapi.Register(client, publicationapi.Service{Store: store})
	input := publicationInput(fmt.Sprintf("publication-exhaustion-%d", time.Now().UnixNano()))
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	initial, initialTaskID, initialCreated, err := publicationapi.Schedule(ctx, store, client, job.ID, input.Revision)
	if err != nil || !initialCreated || initial.Attempt != 0 || initialTaskID == "" {
		t.Fatalf("initial=%#v task=%s created=%v err=%v", initial, initialTaskID, initialCreated, err)
	}
	taskIDs := []string{initialTaskID}
	t.Cleanup(func() {
		for _, id := range taskIDs {
			_ = client.CancelTask(context.Background(), config.QueueName, id)
		}
	})
	replayed, replayedTaskID, replayedCreated, err := publicationapi.Schedule(ctx, store, client, job.ID, input.Revision)
	if err != nil || replayedCreated || replayed.Attempt != initial.Attempt || replayedTaskID != initialTaskID {
		t.Fatalf("active replay=%#v task=%s created=%v err=%v", replayed, replayedTaskID, replayedCreated, err)
	}
	push, pull, err := store.PublicationActions(ctx, job.ID, input.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPush(ctx, push.ID, input.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update absurd.r_dorf_jobs r set state='failed',attempt=$2 from absurd.t_dorf_jobs t where t.task_id=$1::uuid and r.run_id=t.last_attempt_run and r.task_id=t.task_id`, initialTaskID, postgres.PublicationTaskMaxAttempts); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update absurd.t_dorf_jobs set state='failed',attempts=$2 where task_id=$1::uuid`, initialTaskID, postgres.PublicationTaskMaxAttempts); err != nil {
		t.Fatal(err)
	}
	oldEvidence, err := store.TaskEvidence(ctx, initialTaskID)
	if err != nil || oldEvidence.State != "failed" || oldEvidence.Attempts != postgres.PublicationTaskMaxAttempts {
		t.Fatalf("old task Evidence=%#v err=%v", oldEvidence, err)
	}

	const callers = 8
	type result struct {
		params  publicationapi.Params
		taskID  string
		created bool
		err     error
	}
	results := make(chan result, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			params, taskID, created, err := publicationapi.Schedule(ctx, store, client, job.ID, input.Revision)
			results <- result{params: params, taskID: taskID, created: created, err: err}
		}()
	}
	wait.Wait()
	close(results)
	var nextTaskID string
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.params.Attempt != initial.Attempt+1 || result.taskID == "" || result.taskID == initialTaskID {
			t.Fatalf("retry result=%#v", result)
		}
		if nextTaskID == "" {
			nextTaskID = result.taskID
		} else if result.taskID != nextTaskID {
			t.Fatalf("concurrent retry created multiple task identities: %s and %s", nextTaskID, result.taskID)
		}
		if result.created {
			createdCount++
		}
	}
	taskIDs = append(taskIDs, nextTaskID)
	if createdCount != 1 {
		t.Fatalf("concurrent retry created %d tasks, want exactly one", createdCount)
	}
	var taskCount int
	var oldKey, nextKey string
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.t_dorf_jobs where task_name=$1 and params->>'job_id'=$2 and params->>'revision'=$3`, postgres.PublicationTaskName, job.ID, input.Revision).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `select idempotency_key from absurd.t_dorf_jobs where task_id=$1::uuid`, initialTaskID).Scan(&oldKey); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `select idempotency_key from absurd.t_dorf_jobs where task_id=$1::uuid`, nextTaskID).Scan(&nextKey); err != nil {
		t.Fatal(err)
	}
	if taskCount != 2 || oldKey != postgres.PublicationTaskKey(job.ID, input.Revision, initial.Attempt) || nextKey != postgres.PublicationTaskKey(job.ID, input.Revision, initial.Attempt+1) || oldKey == nextKey {
		t.Fatalf("tasks=%d oldKey=%q nextKey=%q", taskCount, oldKey, nextKey)
	}
	retainedPush, retainedPull, err := store.PublicationActions(ctx, job.ID, input.Revision)
	if err != nil || retainedPush.ID != push.ID || retainedPush.State != spine.ActionSucceeded || retainedPush.ExternalID != input.Revision || retainedPull.ID != pull.ID {
		t.Fatalf("retained push=%#v pull=%#v err=%v", retainedPush, retainedPull, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || job.PublicationAttempt != initial.Attempt+1 || job.PublicationTaskID != nextTaskID || job.WorkflowPhase != "publishing" {
		t.Fatalf("advanced Job=%#v err=%v", job, err)
	}
	again, againTaskID, againCreated, err := publicationapi.Schedule(ctx, store, client, job.ID, input.Revision)
	if err != nil || againCreated || again.Attempt != initial.Attempt+1 || againTaskID != nextTaskID {
		t.Fatalf("new active replay=%#v task=%s created=%v err=%v", again, againTaskID, againCreated, err)
	}
}
