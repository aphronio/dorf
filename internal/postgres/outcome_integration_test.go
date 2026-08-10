package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
)

func preparePublishedOutcomeJob(t *testing.T, store postgres.Store, label string) (spine.Job, spine.GitHubProposal) {
	t.Helper()
	ctx := context.Background()
	revision := strings.Repeat("a", 40)
	input := postgres.NewJob{
		AdmissionKey: "outcome-" + label + fmt.Sprintf("-%d", time.Now().UnixNano()),
		Goal:         "record one exact Job outcome", Repository: "https://github.com/aphronio/dorf.git",
		Revision: revision, Branch: "dorf/outcome-" + label,
		ProviderConnection: "primary",
		Model:              "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf",
		GitHubInstallation: "42", BaseBranch: "greenfield",
	}
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit created=%t err=%v", created, err)
	}
	if _, err := store.DB.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	job, push, pull, err := store.BeginPublication(ctx, job.ID, revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPush(ctx, push.ID, revision); err != nil {
		t.Fatal(err)
	}
	proposal := spine.GitHubProposal{
		JobID: job.ID, Number: 39, URL: "https://github.com/aphronio/dorf/pull/39",
		ProposedRevision: revision, BodyDigest: strings.Repeat("1", 64),
	}
	if err := store.RecordProposal(ctx, pull.ID, proposal); err != nil {
		t.Fatal(err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || job.WorkflowPhase != "published" {
		t.Fatalf("published Job=%#v err=%v", job, err)
	}
	return job, proposal
}

func TestPostgresPublishedFollowReopensAndUnchangedRunReturnsToProposal(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, _ := preparePublishedOutcomeJob(t, store, "follow-unchanged")
	message, created, err := store.AdmitMessage(context.Background(), postgres.NewMessage{
		JobID: job.ID, FromKind: spine.MessageFromHuman, FromID: "github-comment-1",
		Input: "Please explain the tradeoff without changing code.", Intent: spine.MessageFollow,
	})
	if err != nil || !created {
		t.Fatalf("admit published follow=%#v created=%t err=%v", message, created, err)
	}
	merge := spine.JobOutcome{
		JobID: job.ID, Kind: spine.OutcomeAccepted, ObservedState: "closed",
		ObservedMerged: true, MergeCommitOID: strings.Repeat("b", 40), ObservedAt: time.Now().UTC(),
	}
	if _, _, err := store.RecordOutcome(context.Background(), merge); err == nil {
		t.Fatal("recorded an Outcome while proposal feedback was still being handled")
	}
	if _, err := store.DB.ExecContext(context.Background(), `update dorf.agent_runs set state='completed',harness='codex',thread_id='thread-completed',turn_id='turn-completed',turn_outcome='completed' where job_id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteUnchangedRun(context.Background(), job.ID, spine.AgentRunID(message.ID), job.Revision, "no code change")
	if err != nil || !completed {
		t.Fatalf("complete unchanged=%t err=%v", completed, err)
	}
	stored, err := store.Job(context.Background(), job.ID)
	if err != nil || stored.WorkflowPhase != "published" || stored.WorkflowAttention != "" {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestPostgresOutcomeFirstWriteWinsAndLeavesProposalAuthorityUntouched(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, proposal := preparePublishedOutcomeJob(t, store, "first-write")
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := spine.JobOutcome{
		JobID: job.ID, Kind: spine.OutcomeAccepted, ObservedState: "closed",
		ObservedMerged: true, MergeCommitOID: strings.Repeat("b", 40), ObservedAt: now,
	}
	stored, created, err := store.RecordOutcome(context.Background(), receipt)
	if err != nil || !created || stored.Kind != spine.OutcomeAccepted || stored.MergeCommitOID != receipt.MergeCommitOID {
		t.Fatalf("stored=%#v created=%t err=%v", stored, created, err)
	}
	repeated := receipt
	repeated.ObservedAt = now.Add(time.Hour)
	got, created, err := store.RecordOutcome(context.Background(), repeated)
	if err != nil || created || got != stored {
		t.Fatalf("repeat=%#v created=%t err=%v", got, created, err)
	}
	conflict := receipt
	conflict.Kind, conflict.ObservedMerged, conflict.MergeCommitOID = spine.OutcomeRejected, false, ""
	if _, _, err := store.RecordOutcome(context.Background(), conflict); err == nil || !strings.Contains(err.Error(), "immutable accepted") {
		t.Fatalf("conflicting outcome error=%v", err)
	}
	proposalAfter, err := store.Proposal(context.Background(), job.ID)
	if err != nil || proposalAfter == nil || proposalAfter.ProposedRevision != proposal.ProposedRevision || proposalAfter.Number != proposal.Number {
		t.Fatalf("proposal changed after outcome: %#v err=%v", proposalAfter, err)
	}
}

func TestPostgresConcurrentConflictingOutcomesSerializeToOneReceipt(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, _ := preparePublishedOutcomeJob(t, store, "concurrent")
	base := spine.JobOutcome{JobID: job.ID, ObservedState: "closed", ObservedAt: time.Now().UTC()}
	accepted := base
	accepted.Kind, accepted.ObservedMerged, accepted.MergeCommitOID = spine.OutcomeAccepted, true, strings.Repeat("c", 40)
	rejected := base
	rejected.Kind = spine.OutcomeRejected
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, receipt := range []spine.JobOutcome{accepted, rejected} {
		wg.Add(1)
		go func(receipt spine.JobOutcome) {
			defer wg.Done()
			_, _, err := store.RecordOutcome(context.Background(), receipt)
			errors <- err
		}(receipt)
	}
	wg.Wait()
	close(errors)
	successes, conflicts := 0, 0
	for err := range errors {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "immutable") {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	stored, err := store.Outcome(context.Background(), job.ID)
	if err != nil || stored == nil || successes != 1 || conflicts != 1 {
		t.Fatalf("stored=%#v successes=%d conflicts=%d err=%v", stored, successes, conflicts, err)
	}
}

func TestPostgresOutcomeRejectsInvalidExternalObservation(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, _ := preparePublishedOutcomeJob(t, store, "mismatch")
	receipt := spine.JobOutcome{JobID: job.ID, Kind: spine.OutcomeAccepted, ObservedState: "closed", ObservedAt: time.Now().UTC()}
	if _, _, err := store.RecordOutcome(context.Background(), receipt); err == nil || !strings.Contains(err.Error(), "merged GitHub pull request") {
		t.Fatalf("observation mismatch error=%v", err)
	}
	if stored, err := store.Outcome(context.Background(), job.ID); err != nil || stored != nil {
		t.Fatalf("invalid outcome persisted=%#v err=%v", stored, err)
	}
}
