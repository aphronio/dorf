package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	policy "github.com/aphronio/dorf/internal/review"
)

func preparePublicationRaceJob(t *testing.T, label string) (postgres.Store, coding.Job, string) {
	t.Helper()
	_, store, _ := testDatabase(t)
	job, revision, _ := prepareReviewIntegrationJob(t, store, "cleanup-publication-"+label)
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"docs/cleanup.md"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReviewPolicy(context.Background(), coding.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}); err != nil {
		t.Fatal(err)
	}
	return store, job, revision
}

func TestExplicitCleanupAcceptsExistingCodingPublicationIntent(t *testing.T) {
	store, job, revision := preparePublicationRaceJob(t, "explicit-cleanup")
	if _, _, _, err := store.BeginPublication(context.Background(), job.ID, revision); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestCleanup(context.Background(), job.ID); err != nil {
		t.Fatalf("Core rejected explicit cleanup using coding Proposal eligibility: %v", err)
	}
	stored, err := store.Job(context.Background(), job.ID)
	if err != nil || stored.AdmissionOpen || stored.CleanupState != core.CleanupRequested {
		t.Fatalf("explicit cleanup request=%#v err=%v", stored, err)
	}
}

func TestSandboxScopedPullRequestActionIsNotPublicationIntent(t *testing.T) {
	collisionJob := func(t *testing.T, suffix string) (postgres.Store, core.Job) {
		t.Helper()
		_, store, job := actionIntegrationJob(t, "pull-request-collision-"+suffix)
		sandboxID := core.MainSandboxName(job.ID)
		action, err := store.GetOrCreateSandboxAction(context.Background(), sandboxID, coding.ActionGitHubPullRequest)
		if err != nil || action.Kind != coding.ActionGitHubPullRequest || action.Scope != sandboxID {
			t.Fatalf("Sandbox-scoped pull-request Action=%#v err=%v", action, err)
		}
		return store, job
	}

	t.Run("abandonment", func(t *testing.T) {
		store, job := collisionJob(t, "abandonment")
		outcome, created, err := store.RecordOutcome(context.Background(), coding.Outcome{
			JobID: job.ID, Kind: coding.OutcomeAbandoned, ObservedAt: time.Now().UTC(),
		})
		if err != nil || !created || outcome.Kind != coding.OutcomeAbandoned {
			t.Fatalf("abandonment Outcome=%#v created=%t err=%v", outcome, created, err)
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		store, job := collisionJob(t, "cleanup")
		if err := store.RequestCleanup(context.Background(), job.ID); err != nil {
			t.Fatalf("close admission for cleanup: %v", err)
		}
	})
}
