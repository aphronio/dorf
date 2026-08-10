package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/postgres"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

func preparePublishedOutcomeJob(t *testing.T, store postgres.Store, label string) (spine.Job, spine.GitHubProposal) {
	t.Helper()
	ctx := context.Background()
	job, revision, _ := prepareReviewIntegrationJob(t, store, "outcome-"+label+fmt.Sprintf("-%d", time.Now().UnixNano()))
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"docs/outcome.md"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReviewPolicy(ctx, spine.ReviewPlanRecord{
		JobID: job.ID, Revision: revision,
		Facts: facts,
		Plan:  plan,
	}); err != nil {
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
	stored, err := store.Proposal(ctx, job.ID)
	if err != nil || stored == nil || stored.ProposedRevision != revision {
		t.Fatalf("current Proposal=%#v err=%v", stored, err)
	}
	return job, proposal
}

func TestPostgresOutcomeAndMessageAdmissionSerializeAtProposalBoundary(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, proposal := preparePublishedOutcomeJob(t, store, "admission-race")
	merge := spine.JobOutcome{
		JobID: job.ID, Kind: spine.OutcomeAccepted, ObservedState: "closed",
		ObservedMerged: true, MergeCommitOID: strings.Repeat("b", 40), ObservedAt: time.Now().UTC(),
	}
	type result struct {
		kind    string
		created bool
		err     error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, created, err := store.AdmitMessage(context.Background(), postgres.NewMessage{
			JobID: job.ID, FromKind: spine.MessageFromHuman, FromID: "github-comment-race",
			Input: "Please explain the tradeoff without changing code.", Intent: spine.MessageFollow,
		})
		results <- result{kind: "message", created: created, err: err}
	}()
	go func() {
		defer wg.Done()
		_, created, err := store.RecordOutcome(context.Background(), merge)
		results <- result{kind: "outcome", created: created, err: err}
	}()
	wg.Wait()
	close(results)

	winner := ""
	for result := range results {
		if result.err == nil && result.created {
			if winner != "" {
				t.Fatalf("both admission and outcome succeeded; second=%s", result.kind)
			}
			winner = result.kind
		} else if result.err == nil {
			t.Fatalf("%s returned neither a creation nor a conflict", result.kind)
		}
	}
	storedOutcome, err := store.Outcome(context.Background(), job.ID)
	storedProposal, proposalErr := store.Proposal(context.Background(), job.ID)
	if err != nil || proposalErr != nil || winner == "" || storedProposal == nil || storedProposal.ProposedRevision != proposal.ProposedRevision {
		t.Fatalf("winner=%q Outcome=%#v Proposal=%#v err=%v proposalErr=%v", winner, storedOutcome, storedProposal, err, proposalErr)
	}
	if winner == "outcome" && storedOutcome == nil || winner == "message" && storedOutcome != nil {
		t.Fatalf("winner=%q Outcome=%#v", winner, storedOutcome)
	}
}

func TestOutcomeRequiresObservedTerminalTargetSteerFallback(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, _ := preparePublishedOutcomeJob(t, store, "steer-fallback")
	threadID := "thread-" + job.ID
	priorTurnID := "turn-" + job.ID

	follow, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: spine.MessageFromHuman, FromID: "outcome-follow", Input: "continue before outcome"})
	if err != nil || !created {
		t.Fatalf("follow=%#v created=%v err=%v", follow, created, err)
	}
	target, err := store.NextDelivery(ctx, job.ID)
	if err != nil || target == nil || target.Message.ID != follow.ID {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", priorTurnID); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-outcome-target-" + job.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: spine.MessageFromHuman, FromID: "outcome-steer-fallback", Input: "finish in a new Turn", Intent: spine.MessageSteer})
	if err != nil || !created {
		t.Fatalf("steer=%#v created=%v err=%v", steer, created, err)
	}
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	fallback, err := store.NextDelivery(ctx, job.ID)
	if err != nil || fallback == nil || fallback.Message.ID != steer.ID {
		t.Fatalf("fallback delivery=%#v err=%v", fallback, err)
	}
	if err := store.PrepareAgentRun(ctx, fallback.AgentRun.ID, "codex", targetTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, fallback.AgentRun.ID, "codex", threadID, "turn-outcome-fallback-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	candidate, ready, err := store.RevisionCandidate(ctx, job.ID, job.Revision)
	if err != nil || !ready || candidate.ID != fallback.AgentRun.ID {
		t.Fatalf("Revision candidate=%#v ready=%v err=%v", candidate, ready, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	observed := integrationEvidence(candidate.ID, "git-revision", "", "", job.Revision, "e")
	observed.AgentRunID = candidate.ID
	revision := spine.RevisionObservation{ComparisonBase: job.Revision, Revision: job.Revision, Tree: strings.Repeat("d", 40), Branch: job.Branch, StartedAt: now, FinishedAt: now}
	if recorded, err := store.RecordRevisionObservation(ctx, job.ID, candidate.ID, revision, observed); err != nil || !recorded {
		t.Fatalf("fallback observation recorded=%v err=%v", recorded, err)
	}
	merge := spine.JobOutcome{
		JobID: job.ID, Kind: spine.OutcomeAccepted, ObservedState: "closed",
		ObservedMerged: true, MergeCommitOID: strings.Repeat("b", 40), ObservedAt: now,
	}
	if _, created, err := store.RecordOutcome(ctx, merge); err != nil || !created {
		t.Fatalf("observed fallback outcome created=%v err=%v", created, err)
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
	contradictory := receipt
	contradictory.MergeCommitOID = strings.Repeat("c", 40)
	if _, _, err := store.RecordOutcome(context.Background(), contradictory); err == nil || !strings.Contains(err.Error(), "immutable accepted outcome authority") {
		t.Fatalf("contradictory same-kind authority error=%v", err)
	}
	conflict := receipt
	conflict.Kind, conflict.ObservedMerged, conflict.MergeCommitOID = spine.OutcomeRejected, false, ""
	if _, _, err := store.RecordOutcome(context.Background(), conflict); err == nil || !strings.Contains(err.Error(), "immutable accepted outcome authority") {
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
