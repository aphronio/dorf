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
	"github.com/aphronio/dorf/internal/workflow"
)

func publicationInput(key string) postgres.NewJob {
	return postgres.NewJob{
		AdmissionKey: key, Goal: "publish one exact Revision", Repository: "https://github.com/aphronio/dorf.git",
		Revision: strings.Repeat("a", 40), Branch: "dorf/issue-43", ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test",
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
	params, taskID, created, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, input.Revision)
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
	repeatedParams, repeatedTaskID, repeatedCreated, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, input.Revision)
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
	_, sameTaskID, sameCreated, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, input.Revision)
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
	_, laterTaskID, laterCreated, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, later)
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

func TestPreProposalPublicationBlockedAllowsExplicitCleanupWithoutOutcome(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	publicationapi.Register(client, publicationapi.Service{Store: store})
	input := publicationInput(fmt.Sprintf("cleanup-blocked-publication-%d", time.Now().UnixNano()))
	job, created, err := workflow.Admit(ctx, store, client, input)
	if err != nil || !created {
		t.Fatalf("admit created=%t err=%v", created, err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	_, publicationTaskID, created, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, job.Revision)
	if err != nil || !created {
		t.Fatalf("publication task=%s created=%t err=%v", publicationTaskID, created, err)
	}
	if err := store.BlockPublication(ctx, job.ID, job.Revision, "pre-proposal failure"); err != nil {
		t.Fatal(err)
	}
	_, pull, err := store.PublicationActions(ctx, job.ID, job.Revision)
	if err != nil || pull.State != spine.ActionPending {
		t.Fatalf("pre-proposal pull Action=%#v err=%v", pull, err)
	}
	cleaning, err := workflow.ScheduleCleanup(ctx, store, client, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range []string{job.TaskID, publicationTaskID, cleaning.CleanupTaskID} {
			_ = client.CancelTask(context.Background(), config.QueueName, id)
		}
	})
	outcome, err := store.Outcome(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := store.Proposal(ctx, job.ID)
	if err != nil || outcome != nil || proposal != nil || cleaning.AdmissionOpen || cleaning.CleanupState != spine.CleanupScheduled {
		t.Fatalf("cleanup=%#v outcome=%#v proposal=%#v err=%v", cleaning, outcome, proposal, err)
	}
}

func TestCleanupSettlesOnlyItsExactAttachedPublicationAndOrdinaryTasks(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	publicationapi.Register(client, publicationapi.Service{Store: store})
	prepare := func(label string) (spine.Job, string) {
		input := publicationInput(fmt.Sprintf("cleanup-publication-%s-%d", label, time.Now().UnixNano()))
		input.Branch = "dorf/cleanup-publication-" + label
		job, created, err := workflow.Admit(ctx, store, client, input)
		if err != nil || !created {
			t.Fatalf("admit %s created=%t err=%v", label, created, err)
		}
		if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
			t.Fatal(err)
		}
		_, taskID, created, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, job.Revision)
		if err != nil || !created {
			t.Fatalf("publication %s task=%s created=%t err=%v", label, taskID, created, err)
		}
		push, pull, err := store.PublicationActions(ctx, job.ID, job.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordPush(ctx, push.ID, job.Revision); err != nil {
			t.Fatal(err)
		}
		proposal := spine.GitHubProposal{JobID: job.ID, Repository: input.GitHubRepository, InstallationID: input.GitHubInstallation, BaseBranch: input.BaseBranch, HeadBranch: input.Branch, Number: 39, URL: "https://github.com/aphronio/dorf/pull/39", ProposedRevision: job.Revision, ObservedRemoteHead: job.Revision, BodyDigest: strings.Repeat("4", 64)}
		if err := store.RecordProposal(ctx, pull.ID, proposal); err != nil {
			t.Fatal(err)
		}
		job, _ = store.Job(ctx, job.ID)
		return job, taskID
	}
	target, targetPublication := prepare("target")
	sentinel, sentinelPublication := prepare("sentinel")
	if _, err := workflow.ScheduleCleanup(ctx, store, client, target.ID); err == nil || !strings.Contains(err.Error(), "without a recorded accepted, rejected, or explicitly abandoned outcome") {
		t.Fatalf("live proposal cleanup error=%v", err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='publication-blocked',revision=$2 where id=$1`, sentinel.ID, strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.ScheduleCleanup(ctx, store, client, sentinel.ID); err == nil || !strings.Contains(err.Error(), "without a recorded accepted, rejected, or explicitly abandoned outcome") {
		t.Fatalf("stale proposal cleanup error=%v", err)
	}
	targetProposal, err := store.Proposal(ctx, target.ID)
	if err != nil || targetProposal == nil {
		t.Fatalf("target proposal=%#v err=%v", targetProposal, err)
	}
	if _, created, err := store.RecordOutcome(ctx, spine.JobOutcome{
		JobID: target.ID, Kind: spine.OutcomeAbandoned, Repository: targetProposal.Repository,
		InstallationID: targetProposal.InstallationID, BaseBranch: targetProposal.BaseBranch,
		HeadBranch: targetProposal.HeadBranch, Number: targetProposal.Number, URL: targetProposal.URL,
		ProposedRevision: targetProposal.ProposedRevision, ObservedHead: targetProposal.ProposedRevision,
		ObservedState: "open", ObservedAt: time.Now().UTC(),
	}); err != nil || !created {
		t.Fatalf("explicit abandonment created=%t err=%v", created, err)
	}
	cleaning, err := workflow.ScheduleCleanup(ctx, store, client, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range []string{cleaning.CleanupTaskID, target.TaskID, targetPublication, sentinel.TaskID, sentinelPublication} {
			_ = client.CancelTask(context.Background(), config.QueueName, id)
		}
	})
	state := func(taskID string) string {
		var value string
		if err := db.QueryRowContext(ctx, `select state from absurd.t_dorf_jobs where task_id=$1::uuid`, taskID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if state(target.TaskID) != "cancelled" || state(targetPublication) != "cancelled" {
		t.Fatalf("target tasks ordinary=%s publication=%s", state(target.TaskID), state(targetPublication))
	}
	if state(sentinel.TaskID) == "cancelled" || state(sentinelPublication) == "cancelled" {
		t.Fatalf("sentinel tasks changed ordinary=%s publication=%s", state(sentinel.TaskID), state(sentinelPublication))
	}
	sentinelAfter, err := store.Job(ctx, sentinel.ID)
	if err != nil || !sentinelAfter.AdmissionOpen || sentinelAfter.CleanupState != spine.CleanupPending || sentinelAfter.PublicationTaskID != sentinelPublication {
		t.Fatalf("sentinel changed=%#v err=%v", sentinelAfter, err)
	}
}

func TestCleanupUsesAbsurdRunStateInsteadOfHistoricalClaimMetadata(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	publicationapi.Register(client, publicationapi.Service{Store: store})
	prepare := func(label string) (spine.Job, string) {
		input := publicationInput(fmt.Sprintf("cleanup-run-liveness-%s-%d", label, time.Now().UnixNano()))
		input.Branch = "dorf/cleanup-run-liveness-" + label
		job, created, err := workflow.Admit(ctx, store, client, input)
		if err != nil || !created {
			t.Fatalf("admit %s created=%t err=%v", label, created, err)
		}
		if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
			t.Fatal(err)
		}
		_, publicationTaskID, created, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, job.Revision)
		if err != nil || !created {
			t.Fatalf("publication %s task=%s created=%t err=%v", label, publicationTaskID, created, err)
		}
		push, pull, err := store.PublicationActions(ctx, job.ID, job.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordPush(ctx, push.ID, job.Revision); err != nil {
			t.Fatal(err)
		}
		number := int64(45)
		if label == "nonterminal" {
			number = 46
		}
		proposal := spine.GitHubProposal{JobID: job.ID, Repository: input.GitHubRepository, InstallationID: input.GitHubInstallation, BaseBranch: input.BaseBranch, HeadBranch: input.Branch, Number: number, URL: fmt.Sprintf("https://github.com/aphronio/dorf/pull/%d", number), ProposedRevision: job.Revision, ObservedRemoteHead: job.Revision, BodyDigest: strings.Repeat("5", 64)}
		if err := store.RecordProposal(ctx, pull.ID, proposal); err != nil {
			t.Fatal(err)
		}
		if _, created, err := store.RecordOutcome(ctx, spine.JobOutcome{JobID: job.ID, Kind: spine.OutcomeAbandoned, Repository: proposal.Repository, InstallationID: proposal.InstallationID, BaseBranch: proposal.BaseBranch, HeadBranch: proposal.HeadBranch, Number: proposal.Number, URL: proposal.URL, ProposedRevision: proposal.ProposedRevision, ObservedHead: proposal.ProposedRevision, ObservedState: "open", ObservedAt: time.Now().UTC()}); err != nil || !created {
			t.Fatalf("record abandonment created=%t err=%v", created, err)
		}
		cleaning, err := workflow.ScheduleCleanup(ctx, store, client, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, cleaning.CleanupTaskID) })
		return cleaning, publicationTaskID
	}
	setRun := func(taskID, state string, retainClaim bool) {
		claimedBy, lease := any(nil), any(nil)
		if retainClaim {
			claimedBy, lease = "historical-worker", time.Now().UTC().Add(time.Hour)
		}
		result, err := db.ExecContext(ctx, `update absurd.r_dorf_jobs set state=$2,claimed_by=$3,claim_expires_at=$4 where task_id=$1::uuid`, taskID, state, claimedBy, lease)
		if err != nil {
			t.Fatal(err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows == 0 {
			t.Fatalf("task %s run update rows=%d err=%v", taskID, rows, err)
		}
	}

	terminal, terminalPublication := prepare("terminal-history")
	setRun(terminal.TaskID, "failed", true)
	setRun(terminalPublication, "cancelled", true)
	ordinaryEvidence, err := store.TaskEvidence(ctx, terminal.TaskID)
	if err != nil || ordinaryEvidence.LiveClaims != 0 {
		t.Fatalf("terminal ordinary task evidence=%#v err=%v", ordinaryEvidence, err)
	}
	publicationEvidence, err := store.PublicationTaskHistory(ctx, terminal)
	if err != nil || len(publicationEvidence) != 1 || publicationEvidence[0].LiveClaims != 0 {
		t.Fatalf("terminal publication evidence=%#v err=%v", publicationEvidence, err)
	}
	if err := store.CompleteCleanup(ctx, terminal.ID); err != nil {
		t.Fatalf("terminal historical claim metadata blocked cleanup: %v", err)
	}
	for _, taskID := range []string{terminal.TaskID, terminalPublication} {
		var total, retained int
		if err := db.QueryRowContext(ctx, `select count(*),count(*) filter(where claimed_by='historical-worker' and claim_expires_at is not null) from absurd.r_dorf_jobs where task_id=$1::uuid`, taskID).Scan(&total, &retained); err != nil {
			t.Fatal(err)
		}
		if total == 0 || retained != total {
			t.Fatalf("cleanup rewrote historical claimant/lease metadata for task %s: retained=%d total=%d", taskID, retained, total)
		}
	}

	live, livePublication := prepare("nonterminal")
	setRun(live.TaskID, "running", false)
	if err := store.CompleteCleanup(ctx, live.ID); err == nil || !strings.Contains(err.Error(), "ordinary Job task") || !strings.Contains(err.Error(), "live run claims") {
		t.Fatalf("nonterminal ordinary run cleanup error=%v", err)
	}
	setRun(live.TaskID, "failed", true)
	setRun(livePublication, "sleeping", false)
	if err := store.CompleteCleanup(ctx, live.ID); err == nil || !strings.Contains(err.Error(), "attached publication task") || !strings.Contains(err.Error(), "live run claims") {
		t.Fatalf("nonterminal publication run cleanup error=%v", err)
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
	initial, initialTaskID, initialCreated, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, input.Revision)
	if err != nil || !initialCreated || initial.Attempt != 0 || initialTaskID == "" {
		t.Fatalf("initial=%#v task=%s created=%v err=%v", initial, initialTaskID, initialCreated, err)
	}
	taskIDs := []string{initialTaskID}
	t.Cleanup(func() {
		for _, id := range taskIDs {
			_ = client.CancelTask(context.Background(), config.QueueName, id)
		}
	})
	replayed, replayedTaskID, replayedCreated, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, input.Revision)
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
			params, taskID, created, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, input.Revision)
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
	history, err := store.PublicationTaskHistory(ctx, job)
	if err != nil || len(history) != 2 {
		t.Fatalf("publication task history=%#v err=%v", history, err)
	}
	if history[0].TaskID != initialTaskID || history[0].IdempotencyKey != oldKey || history[0].State != "failed" || history[0].Attempts != postgres.PublicationTaskMaxAttempts || history[0].Attempt != initial.Attempt || history[0].Current {
		t.Fatalf("retained exhausted generation=%#v", history[0])
	}
	if history[1].TaskID != nextTaskID || history[1].IdempotencyKey != nextKey || history[1].Attempt != initial.Attempt+1 || !history[1].Current {
		t.Fatalf("current publication generation=%#v", history[1])
	}
	again, againTaskID, againCreated, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, input.Revision)
	if err != nil || againCreated || again.Attempt != initial.Attempt+1 || againTaskID != nextTaskID {
		t.Fatalf("new active replay=%#v task=%s created=%v err=%v", again, againTaskID, againCreated, err)
	}
}

func TestPostgresPublicationSpawnBeforeAttachWorkerWinIsAdoptedAfterPublished(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	publicationapi.Register(client, publicationapi.Service{Store: store})
	input := publicationInput(fmt.Sprintf("publication-worker-win-%d", time.Now().UnixNano()))
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	barrier := &oneShotPublicationScheduleBarrier{point: spine.BarrierPublicationSpawn}
	params, taskID, created, err := publicationapi.Schedule(ctx, store, client, barrier, job.ID, input.Revision)
	if err == nil || !created || taskID == "" || params.Attempt != 0 {
		t.Fatalf("spawn loss params=%#v task=%s created=%v err=%v", params, taskID, created, err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, taskID) })
	push, pull, err := store.PublicationActions(ctx, job.ID, input.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPush(ctx, push.ID, input.Revision); err != nil {
		t.Fatal(err)
	}
	proposal := spine.GitHubProposal{JobID: job.ID, Repository: input.GitHubRepository, InstallationID: input.GitHubInstallation, BaseBranch: input.BaseBranch, HeadBranch: input.Branch, Number: 43, URL: "https://github.com/aphronio/dorf/pull/43", ProposedRevision: input.Revision, ObservedRemoteHead: input.Revision, BodyDigest: strings.Repeat("1", 64)}
	if err := store.RecordProposal(ctx, pull.ID, proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update absurd.r_dorf_jobs r set state='completed',attempt=1 from absurd.t_dorf_jobs t where t.task_id=$1::uuid and r.run_id=t.last_attempt_run and r.task_id=t.task_id`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update absurd.t_dorf_jobs set state='completed',attempts=1 where task_id=$1::uuid`, taskID); err != nil {
		t.Fatal(err)
	}

	recovered, recoveredTaskID, recoveredCreated, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, input.Revision)
	if err != nil || recoveredCreated || recovered.Attempt != params.Attempt || recoveredTaskID != taskID {
		t.Fatalf("published adoption params=%#v task=%s created=%v err=%v", recovered, recoveredTaskID, recoveredCreated, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || job.WorkflowPhase != "published" || job.PublicationTaskID != taskID || job.PublicationAttempt != params.Attempt {
		t.Fatalf("published attachment Job=%#v err=%v", job, err)
	}
	history, err := store.PublicationTaskHistory(ctx, job)
	if err != nil || len(history) != 1 || !history[0].Current || history[0].State != "completed" || history[0].TaskID != taskID {
		t.Fatalf("completed publication evidence=%#v err=%v", history, err)
	}
	var tasks, proposals, actions int
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.t_dorf_jobs where task_name=$1 and params->>'job_id'=$2`, postgres.PublicationTaskName, job.ID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.github_proposals where job_id=$1`, job.ID).Scan(&proposals); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.actions where job_id=$1 and scope_key=$2 and kind in ('repository-push','github-pull-request')`, job.ID, input.Revision).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || proposals != 1 || actions != 2 {
		t.Fatalf("worker-win recovery tasks=%d proposals=%d actions=%d", tasks, proposals, actions)
	}
}

type cancelPublicationAttachContext struct{ cancel context.CancelFunc }

func (b cancelPublicationAttachContext) ReachWorkflow(_ context.Context, point, _, _ string) error {
	if point == spine.BarrierPublicationSpawn {
		b.cancel()
	}
	return nil
}

func TestPostgresPublicationTransientAttachFailureRetainsAttemptTask(t *testing.T) {
	db, store, client := testDatabase(t)
	publicationapi.Register(client, publicationapi.Service{Store: store})
	input := publicationInput(fmt.Sprintf("publication-attach-transient-%d", time.Now().UnixNano()))
	job, created, err := store.Admit(context.Background(), input)
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	if _, err := db.ExecContext(context.Background(), `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	firstCtx, cancel := context.WithCancel(context.Background())
	params, taskID, created, err := publicationapi.Schedule(firstCtx, store, client, cancelPublicationAttachContext{cancel: cancel}, job.ID, input.Revision)
	if err == nil || !created || taskID == "" || params.Attempt != 0 {
		t.Fatalf("transient attach params=%#v task=%s created=%v err=%v", params, taskID, created, err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, taskID) })
	evidence, err := store.TaskEvidence(context.Background(), taskID)
	if err != nil || evidence.State == "cancelled" || evidence.State == "missing" {
		t.Fatalf("attach error poisoned task=%#v err=%v", evidence, err)
	}
	job, err = store.Job(context.Background(), job.ID)
	if err != nil || job.WorkflowPhase != "publishing" || job.PublicationTaskID != "" || job.PublicationAttempt != params.Attempt {
		t.Fatalf("unattached Job=%#v err=%v", job, err)
	}
	recovered, recoveredTaskID, recoveredCreated, err := publicationapi.Schedule(context.Background(), store, client, nil, job.ID, input.Revision)
	if err != nil || recoveredCreated || recovered.Attempt != params.Attempt || recoveredTaskID != taskID {
		t.Fatalf("same-key attach retry params=%#v task=%s created=%v err=%v", recovered, recoveredTaskID, recoveredCreated, err)
	}
	job, err = store.Job(context.Background(), job.ID)
	if err != nil || job.PublicationTaskID != taskID {
		t.Fatalf("recovered attachment Job=%#v err=%v", job, err)
	}
}

type oneShotPublicationScheduleBarrier struct {
	mu     sync.Mutex
	point  string
	failed bool
}

func (b *oneShotPublicationScheduleBarrier) ReachWorkflow(_ context.Context, point, _, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if point == b.point && !b.failed {
		b.failed = true
		return fmt.Errorf("simulated worker loss at %s", point)
	}
	return nil
}

func TestPostgresPublicationSchedulingWindowsRecoverOneTaskAttachmentAndActions(t *testing.T) {
	for _, point := range []string{spine.BarrierPublicationBegin, spine.BarrierPublicationSpawn} {
		t.Run(point, func(t *testing.T) {
			db, store, client := testDatabase(t)
			ctx := context.Background()
			publicationapi.Register(client, publicationapi.Service{Store: store})
			input := publicationInput(fmt.Sprintf("publication-window-%s-%d", point, time.Now().UnixNano()))
			job, created, err := store.Admit(ctx, input)
			if err != nil || !created {
				t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
			}
			if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				rows, err := db.QueryContext(context.Background(), `select task_id::text from absurd.t_dorf_jobs where task_name=$1 and params->>'job_id'=$2`, postgres.PublicationTaskName, job.ID)
				if err != nil {
					return
				}
				defer rows.Close()
				for rows.Next() {
					var taskID string
					if rows.Scan(&taskID) == nil {
						_ = client.CancelTask(context.Background(), config.QueueName, taskID)
					}
				}
			})
			barrier := &oneShotPublicationScheduleBarrier{point: point}
			initial, lostTaskID, lostCreated, err := publicationapi.Schedule(ctx, store, client, barrier, job.ID, input.Revision)
			if err == nil || initial.Attempt != 0 {
				t.Fatalf("loss params=%#v task=%s created=%v err=%v", initial, lostTaskID, lostCreated, err)
			}
			job, err = store.Job(ctx, job.ID)
			if err != nil || job.WorkflowPhase != "publishing" || job.PublicationAttempt != 0 || job.PublicationTaskID != "" {
				t.Fatalf("pre-attach Job=%#v err=%v", job, err)
			}
			push, pull, err := store.PublicationActions(ctx, job.ID, input.Revision)
			if err != nil || push.ID != spine.ScopedActionID(job.ID, spine.ActionRepositoryPush, input.Revision) || pull.ID != spine.ScopedActionID(job.ID, spine.ActionGitHubPullRequest, input.Revision) {
				t.Fatalf("initial Actions push=%#v pull=%#v err=%v", push, pull, err)
			}
			var tasksBefore int
			if err := db.QueryRowContext(ctx, `select count(*) from absurd.t_dorf_jobs where task_name=$1 and params->>'job_id'=$2 and params->>'revision'=$3 and idempotency_key=$4`, postgres.PublicationTaskName, job.ID, input.Revision, postgres.PublicationTaskKey(job.ID, input.Revision, 0)).Scan(&tasksBefore); err != nil {
				t.Fatal(err)
			}
			if point == spine.BarrierPublicationBegin && (tasksBefore != 0 || lostTaskID != "" || lostCreated) {
				t.Fatalf("pre-Spawn loss tasks=%d task=%s created=%v", tasksBefore, lostTaskID, lostCreated)
			}
			if point == spine.BarrierPublicationSpawn && (tasksBefore != 1 || lostTaskID == "" || !lostCreated) {
				t.Fatalf("pre-Attach loss tasks=%d task=%s created=%v", tasksBefore, lostTaskID, lostCreated)
			}

			const callers = 8
			type scheduleResult struct {
				params  publicationapi.Params
				taskID  string
				created bool
				err     error
			}
			results := make(chan scheduleResult, callers)
			var wait sync.WaitGroup
			for range callers {
				wait.Add(1)
				go func() {
					defer wait.Done()
					params, taskID, created, err := publicationapi.Schedule(ctx, store, client, barrier, job.ID, input.Revision)
					results <- scheduleResult{params: params, taskID: taskID, created: created, err: err}
				}()
			}
			wait.Wait()
			close(results)
			finalTaskID := lostTaskID
			createdCount := 0
			for result := range results {
				if result.err != nil || result.params.Attempt != 0 || result.taskID == "" {
					t.Fatalf("recovery result=%#v", result)
				}
				if finalTaskID == "" {
					finalTaskID = result.taskID
				} else if result.taskID != finalTaskID {
					t.Fatalf("retries diverged across task IDs %s and %s", finalTaskID, result.taskID)
				}
				if result.created {
					createdCount++
				}
			}
			wantCreated := 1
			if point == spine.BarrierPublicationSpawn {
				wantCreated = 0
			}
			if createdCount != wantCreated {
				t.Fatalf("recovery created=%d tasks, want %d", createdCount, wantCreated)
			}
			var tasksAfter, attachments int
			var key string
			if err := db.QueryRowContext(ctx, `select count(*),min(idempotency_key) from absurd.t_dorf_jobs where task_name=$1 and params->>'job_id'=$2 and params->>'revision'=$3`, postgres.PublicationTaskName, job.ID, input.Revision).Scan(&tasksAfter, &key); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `select count(*) from dorf.jobs where id=$1 and publication_task_id=$2 and publication_attempt=0 and workflow_phase='publishing'`, job.ID, finalTaskID).Scan(&attachments); err != nil {
				t.Fatal(err)
			}
			if tasksAfter != 1 || attachments != 1 || key != postgres.PublicationTaskKey(job.ID, input.Revision, 0) {
				t.Fatalf("tasks=%d attachments=%d key=%q final=%s", tasksAfter, attachments, key, finalTaskID)
			}
			retainedPush, retainedPull, err := store.PublicationActions(ctx, job.ID, input.Revision)
			if err != nil || retainedPush.ID != push.ID || retainedPush.State != spine.ActionPending || retainedPull.ID != pull.ID || retainedPull.State != spine.ActionPending {
				t.Fatalf("retained Actions push=%#v pull=%#v err=%v", retainedPush, retainedPull, err)
			}
			var actionCount int
			if err := db.QueryRowContext(ctx, `select count(*) from dorf.actions where job_id=$1 and scope_key=$2 and kind in ('repository-push','github-pull-request')`, job.ID, input.Revision).Scan(&actionCount); err != nil {
				t.Fatal(err)
			}
			if actionCount != 2 {
				t.Fatalf("scheduling recovery created %d GitHub Actions, want exactly two", actionCount)
			}
		})
	}
}
