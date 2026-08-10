package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/postgres"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

func preparePublicationRaceJob(t *testing.T, label string) (postgres.Store, spine.Job, string) {
	t.Helper()
	_, store, _ := testDatabase(t)
	job, revision, _ := prepareReviewIntegrationJob(t, store, "cleanup-publication-"+label)
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"docs/cleanup.md"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReviewPolicy(context.Background(), spine.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}); err != nil {
		t.Fatal(err)
	}
	return store, job, revision
}

func TestPostgresCleanupAndPublicationSerializeToOneWinner(t *testing.T) {
	store, job, revision := preparePublicationRaceJob(t, "race")
	start := make(chan struct{})
	type result struct {
		kind string
		err  error
	}
	results := make(chan result, 2)
	go func() {
		<-start
		results <- result{kind: "cleanup", err: store.CloseAdmissionForCleanup(context.Background(), job.ID)}
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
	stored, err := store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleanupWon := errorsByKind["cleanup"] == nil
	publicationWon := errorsByKind["publication"] == nil
	_, _, publicationActionsErr := store.PublicationActions(context.Background(), job.ID, revision)
	if cleanupWon == publicationWon || cleanupWon == stored.AdmissionOpen || publicationWon != (publicationActionsErr == nil) {
		t.Fatalf("cleanupErr=%v publicationErr=%v publicationActionsErr=%v Job=%#v", errorsByKind["cleanup"], errorsByKind["publication"], publicationActionsErr, stored)
	}
}

func TestPostgresRecordedAbandonmentAllowsCleanupAdmissionClose(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, _ := preparePublishedOutcomeJob(t, store, "cleanup-abandoned")
	_, created, err := store.RecordOutcome(context.Background(), spine.JobOutcome{JobID: job.ID, Kind: spine.OutcomeAbandoned, ObservedState: "open", ObservedAt: time.Now().UTC()})
	if err != nil || !created {
		t.Fatalf("abandonment created=%v err=%v", created, err)
	}
	if err := store.CloseAdmissionForCleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := store.Job(context.Background(), job.ID)
	if err != nil || closed.AdmissionOpen {
		t.Fatalf("closed Job=%#v err=%v", closed, err)
	}
}
