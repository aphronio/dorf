package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	policy "github.com/aphronio/dorf/internal/review"
)

func preparePublishedOutcomeJob(t *testing.T, store postgres.Store, label string) (coding.Job, coding.Proposal) {
	t.Helper()
	ctx := context.Background()
	job, revision, _ := prepareReviewIntegrationJob(t, store, "outcome-"+label+fmt.Sprintf("-%d", time.Now().UnixNano()))
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"docs/outcome.md"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReviewPolicy(ctx, coding.ReviewPlanRecord{
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
	proposal := coding.Proposal{
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

func TestPostgresPreProposalAbandonmentIsTerminalAndIdempotent(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, created, err := store.AdmitCoding(context.Background(), codingJobInput(
		"pre-proposal-abandon-"+fmt.Sprint(time.Now().UnixNano()),
		"stop this coding Job",
		strings.Repeat("a", 40),
		"dorf/pre-proposal-abandon",
	))
	if err != nil || !created {
		t.Fatalf("Job=%#v created=%v err=%v", job, created, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := coding.Outcome{JobID: job.ID, Kind: coding.OutcomeAbandoned, ObservedAt: now}
	stored, created, err := store.RecordOutcome(context.Background(), receipt)
	if err != nil || !created || stored.JobID != receipt.JobID || stored.Kind != receipt.Kind || !stored.ObservedAt.Equal(receipt.ObservedAt) {
		t.Fatalf("Outcome=%#v created=%v err=%v", stored, created, err)
	}
	job, err = store.Job(context.Background(), job.ID)
	if err != nil || !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		t.Fatalf("Outcome changed generic Message or cleanup lifecycle: Job=%#v err=%v", job, err)
	}
	repeated := receipt
	repeated.ObservedAt = now.Add(time.Hour)
	got, created, err := store.RecordOutcome(context.Background(), repeated)
	if err != nil || created || got != stored {
		t.Fatalf("idempotent Outcome=%#v created=%v err=%v", got, created, err)
	}
	if admitted, err := store.AdmitCodingMessage(context.Background(), core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "after-abandon", Input: "continue"}); err != nil || !admitted.Created {
		t.Fatalf("Outcome blocked Message before explicit cleanup: admitted=%#v err=%v", admitted, err)
	}
	if err := store.RequestCleanup(context.Background(), job.ID); err != nil {
		t.Fatalf("cleanup close after abandonment: %v", err)
	}
	job, err = store.Job(context.Background(), job.ID)
	if err != nil || job.AdmissionOpen || job.CleanupState != core.CleanupRequested {
		t.Fatalf("explicit cleanup did not close admission: Job=%#v err=%v", job, err)
	}
}

func TestPostgresPreProposalAbandonmentAndPublicationIntentSerializeToOneWinner(t *testing.T) {
	store, job, revision := preparePublicationRaceJob(t, "abandon-race")
	start := make(chan struct{})
	type result struct {
		kind string
		err  error
	}
	results := make(chan result, 2)
	go func() {
		<-start
		_, _, err := store.RecordOutcome(context.Background(), coding.Outcome{JobID: job.ID, Kind: coding.OutcomeAbandoned, ObservedAt: time.Now().UTC()})
		results <- result{kind: "abandon", err: err}
	}()
	go func() {
		<-start
		_, _, _, err := store.BeginPublication(context.Background(), job.ID, revision)
		results <- result{kind: "publication", err: err}
	}()
	close(start)

	errorsByKind := map[string]error{}
	for range 2 {
		result := <-results
		errorsByKind[result.kind] = result.err
	}
	outcome, outcomeErr := store.Outcome(context.Background(), job.ID)
	_, _, actionErr := store.PublicationActions(context.Background(), job.ID, revision)
	abandonWon := errorsByKind["abandon"] == nil
	publicationWon := errorsByKind["publication"] == nil
	if outcomeErr != nil || abandonWon == publicationWon || abandonWon != (outcome != nil) || publicationWon != (actionErr == nil) {
		t.Fatalf("abandonErr=%v publicationErr=%v Outcome=%#v outcomeErr=%v actionErr=%v", errorsByKind["abandon"], errorsByKind["publication"], outcome, outcomeErr, actionErr)
	}
}

func TestPostgresExactProposalAbandonmentPermitsActiveInputForCleanup(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, proposal := preparePublishedOutcomeJob(t, store, "active-abandon")
	message, err := store.AdmitCodingMessage(context.Background(), core.MessageAdmission{
		JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "active-before-abandon",
		Input: "continue working before the explicit stop",
	})
	if err != nil || !message.Created {
		t.Fatalf("Message=%#v err=%v", message, err)
	}
	delivery, err := codingDelivery(context.Background(), store, job.ID)
	if err != nil || delivery == nil || delivery.Message.ID != message.Message.ID {
		t.Fatalf("delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(context.Background(), delivery.AgentRun.ID, "codex", "turn-before-abandon"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(context.Background(), delivery.AgentRun.ID, "codex", delivery.AgentRun.ThreadID, "turn-active-abandon", "running"); err != nil {
		t.Fatal(err)
	}

	receipt := coding.Outcome{JobID: job.ID, Kind: coding.OutcomeAbandoned, ObservedState: "open", ObservedAt: time.Now().UTC()}
	stored, created, err := store.RecordOutcome(context.Background(), receipt)
	if err != nil || !created || stored.Kind != coding.OutcomeAbandoned || stored.ObservedState != "open" {
		t.Fatalf("Outcome=%#v created=%v err=%v", stored, created, err)
	}
	retained, err := store.Job(context.Background(), job.ID)
	if err != nil || !retained.AdmissionOpen || retained.CleanupState != core.CleanupPending {
		t.Fatalf("Outcome changed Message or cleanup lifecycle: Job=%#v err=%v", retained, err)
	}
	gotProposal, err := store.Proposal(context.Background(), job.ID)
	if err != nil || gotProposal == nil || *gotProposal != proposal {
		t.Fatalf("Proposal authority changed: %#v err=%v", gotProposal, err)
	}
	deliveries, err := store.Deliveries(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	activeForCleanup := false
	for _, candidate := range deliveries {
		activeForCleanup = activeForCleanup || candidate.AgentRun.ID == delivery.AgentRun.ID && candidate.AgentRun.State == core.AgentRunActive
	}
	if !activeForCleanup {
		t.Fatalf("active AgentRun was not left for cleanup reconciliation: %#v", deliveries)
	}
}

func TestPostgresOutcomeAndMessageAdmissionSerializeAtProposalBoundary(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, proposal := preparePublishedOutcomeJob(t, store, "admission-race")
	merge := coding.Outcome{
		JobID: job.ID, Kind: coding.OutcomeAccepted, ObservedState: "closed",
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
		admitted, err := store.AdmitCodingMessage(context.Background(), core.MessageAdmission{
			JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "github-comment-race",
			Input: "Please explain the tradeoff without changing code.", Intent: core.MessageFollow,
		})
		results <- result{kind: "message", created: admitted.Created, err: err}
	}()
	go func() {
		defer wg.Done()
		_, created, err := store.RecordOutcome(context.Background(), merge)
		results <- result{kind: "outcome", created: created, err: err}
	}()
	wg.Wait()
	close(results)

	resultsByKind := make(map[string]result, 2)
	for result := range results {
		resultsByKind[result.kind] = result
	}
	messageResult := resultsByKind["message"]
	outcomeResult := resultsByKind["outcome"]
	if messageResult.err != nil || !messageResult.created {
		t.Fatalf("Outcome race blocked generic Message admission: %#v", messageResult)
	}
	if outcomeResult.err == nil && !outcomeResult.created {
		t.Fatalf("Outcome returned neither creation nor a typed-fact conflict")
	}
	storedOutcome, err := store.Outcome(context.Background(), job.ID)
	storedProposal, proposalErr := store.Proposal(context.Background(), job.ID)
	if err != nil || proposalErr != nil || storedProposal == nil || storedProposal.ProposedRevision != proposal.ProposedRevision {
		t.Fatalf("Outcome=%#v Proposal=%#v err=%v proposalErr=%v", storedOutcome, storedProposal, err, proposalErr)
	}
	if (outcomeResult.err == nil) != (storedOutcome != nil) {
		t.Fatalf("Outcome result=%#v stored=%#v", outcomeResult, storedOutcome)
	}
}

func TestPostgresConcurrentConflictingOutcomesSerializeToOneReceipt(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, proposal := preparePublishedOutcomeJob(t, store, "concurrent")
	base := coding.Outcome{JobID: job.ID, ObservedState: "closed", ObservedAt: time.Now().UTC()}
	accepted := base
	accepted.Kind, accepted.ObservedMerged, accepted.MergeCommitOID = coding.OutcomeAccepted, true, strings.Repeat("c", 40)
	rejected := base
	rejected.Kind = coding.OutcomeRejected
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, receipt := range []coding.Outcome{accepted, rejected} {
		wg.Add(1)
		go func(receipt coding.Outcome) {
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
	repeated := *stored
	repeated.ObservedAt = repeated.ObservedAt.Add(time.Hour)
	got, created, err := store.RecordOutcome(context.Background(), repeated)
	if err != nil || created || got != *stored {
		t.Fatalf("repeat=%#v created=%t err=%v", got, created, err)
	}
	contradictory := accepted
	if stored.Kind == coding.OutcomeAccepted {
		contradictory = rejected
	}
	if _, _, err := store.RecordOutcome(context.Background(), contradictory); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("conflicting outcome error=%v", err)
	}
	acceptedAuthorityJobID := job.ID
	if stored.Kind != coding.OutcomeAccepted {
		acceptedJob, _ := preparePublishedOutcomeJob(t, store, "accepted-authority")
		acceptedAuthorityJobID = acceptedJob.ID
		accepted.JobID = acceptedAuthorityJobID
		if _, created, err := store.RecordOutcome(context.Background(), accepted); err != nil || !created {
			t.Fatalf("accepted authority setup created=%t err=%v", created, err)
		}
	}
	sameKindConflict := accepted
	sameKindConflict.JobID = acceptedAuthorityJobID
	sameKindConflict.MergeCommitOID = strings.Repeat("d", 40)
	if _, _, err := store.RecordOutcome(context.Background(), sameKindConflict); err == nil || !strings.Contains(err.Error(), "immutable accepted outcome authority") {
		t.Fatalf("same-kind accepted authority error=%v", err)
	}
	proposalAfter, err := store.Proposal(context.Background(), job.ID)
	if err != nil || proposalAfter == nil || *proposalAfter != proposal {
		t.Fatalf("proposal changed after outcome: %#v err=%v", proposalAfter, err)
	}
}

func TestPostgresOutcomeRejectsInvalidExternalObservation(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, _ := preparePublishedOutcomeJob(t, store, "mismatch")
	receipt := coding.Outcome{JobID: job.ID, Kind: coding.OutcomeAccepted, ObservedState: "closed", ObservedAt: time.Now().UTC()}
	if _, _, err := store.RecordOutcome(context.Background(), receipt); err == nil || !strings.Contains(err.Error(), "merged GitHub pull request") {
		t.Fatalf("observation mismatch error=%v", err)
	}
	if stored, err := store.Outcome(context.Background(), job.ID); err != nil || stored != nil {
		t.Fatalf("invalid outcome persisted=%#v err=%v", stored, err)
	}
}
