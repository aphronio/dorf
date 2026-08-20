package outcome

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/spine"
)

type outcomeStore struct {
	job      coding.Job
	proposal *coding.Proposal
	outcome  *coding.Outcome
	writes   int
}

func (s *outcomeStore) CodingJob(context.Context, string) (coding.Job, error) { return s.job, nil }
func (s *outcomeStore) Proposal(context.Context, string) (*coding.Proposal, error) {
	return s.proposal, nil
}
func (s *outcomeStore) Outcome(context.Context, string) (*coding.Outcome, error) {
	return s.outcome, nil
}
func (s *outcomeStore) RecordOutcome(_ context.Context, receipt coding.Outcome) (coding.Outcome, bool, error) {
	s.writes++
	s.outcome = &receipt
	return receipt, true, nil
}
func (s *outcomeStore) WithJobFence(_ context.Context, _ string, fn func() error) error { return fn() }

type outcomeGitHub struct {
	pull  githubapi.PullRequest
	err   error
	calls int
}

func (g *outcomeGitHub) PullRequest(_ context.Context, authority githubapi.Authority, number int64) (githubapi.PullRequest, error) {
	g.calls++
	if authority != (githubapi.Authority{Repository: "aphronio/dorf", InstallationID: "42"}) || number != 39 {
		return githubapi.PullRequest{}, errors.New("wrong exact authority")
	}
	return g.pull, g.err
}

func outcomeFixture() (*outcomeStore, *outcomeGitHub, Service) {
	revision := strings.Repeat("a", 40)
	store := &outcomeStore{
		job:      coding.Job{Job: spine.Job{ID: "job-exact"}, Revision: revision, GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield", Branch: "dorf/issue-39"},
		proposal: &coding.Proposal{JobID: "job-exact", Number: 39, URL: "https://github.com/aphronio/dorf/pull/39", ProposedRevision: revision},
	}
	github := &outcomeGitHub{pull: githubapi.PullRequest{Number: 39, URL: store.proposal.URL, Repository: "aphronio/dorf", Head: "dorf/issue-39", Base: "greenfield", HeadSHA: revision}}
	service := (Service{Store: store, GitHub: github, Now: func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) }}).WithClaimCheck(func(context.Context) error { return nil })
	return store, github, service
}

func TestExactGitHubAuthorityRecordsThreeDistinctOutcomes(t *testing.T) {
	for _, test := range []struct {
		kind   coding.OutcomeKind
		state  string
		merged bool
		merge  string
	}{
		{coding.OutcomeAccepted, "closed", true, strings.Repeat("b", 40)},
		{coding.OutcomeRejected, "closed", false, ""},
		{coding.OutcomeAbandoned, "open", false, ""},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			store, github, service := outcomeFixture()
			github.pull.State, github.pull.Merged, github.pull.MergeCommitOID = test.state, test.merged, test.merge
			receipt, created, err := service.Record(context.Background(), store.job.ID, test.kind)
			if err != nil || !created || store.writes != 1 || github.calls != 1 {
				t.Fatalf("receipt=%#v created=%t writes=%d calls=%d err=%v", receipt, created, store.writes, github.calls, err)
			}
			if receipt.Kind != test.kind || receipt.ObservedState != test.state || receipt.ObservedMerged != test.merged || receipt.MergeCommitOID != test.merge {
				t.Fatalf("inexact outcome receipt=%#v", receipt)
			}
		})
	}
}

func TestAbandonBeforeProposalRecordsNoInventedGitHubObservation(t *testing.T) {
	store, github, service := outcomeFixture()
	store.proposal = nil

	receipt, created, err := service.Record(context.Background(), store.job.ID, coding.OutcomeAbandoned)
	if err != nil || !created || store.writes != 1 || github.calls != 0 {
		t.Fatalf("receipt=%#v created=%t writes=%d calls=%d err=%v", receipt, created, store.writes, github.calls, err)
	}
	if receipt.ObservedState != "" || receipt.ObservedMerged || receipt.MergeCommitOID != "" {
		t.Fatalf("pre-Proposal abandonment invented GitHub authority: %#v", receipt)
	}
	if _, _, err := func() (coding.Outcome, bool, error) {
		store.outcome = nil
		return service.Record(context.Background(), store.job.ID, coding.OutcomeRejected)
	}(); err == nil || github.calls != 0 {
		t.Fatalf("rejected outcome without Proposal reached GitHub: calls=%d err=%v", github.calls, err)
	}
}

func TestOutcomeFailsClosedForWrongAuthorityOrGitHubState(t *testing.T) {
	for _, test := range []struct {
		name   string
		kind   coding.OutcomeKind
		mutate func(*outcomeStore, *outcomeGitHub)
	}{
		{"accepted-unmerged", coding.OutcomeAccepted, func(_ *outcomeStore, g *outcomeGitHub) { g.pull.State = "closed" }},
		{"rejected-open", coding.OutcomeRejected, func(_ *outcomeStore, g *outcomeGitHub) { g.pull.State = "open" }},
		{"abandoned-merged", coding.OutcomeAbandoned, func(_ *outcomeStore, g *outcomeGitHub) {
			g.pull.State, g.pull.Merged, g.pull.MergeCommitOID = "closed", true, strings.Repeat("b", 40)
		}},
		{"wrong-number", coding.OutcomeAbandoned, func(_ *outcomeStore, g *outcomeGitHub) { g.pull.State, g.pull.Number = "open", 40 }},
		{"wrong-revision", coding.OutcomeAbandoned, func(_ *outcomeStore, g *outcomeGitHub) {
			g.pull.State, g.pull.HeadSHA = "open", strings.Repeat("c", 40)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, github, service := outcomeFixture()
			test.mutate(store, github)
			if _, _, err := service.Record(context.Background(), store.job.ID, test.kind); err == nil || store.writes != 0 {
				t.Fatalf("invalid authority/state wrote outcome: writes=%d err=%v", store.writes, err)
			}
		})
	}
}

func TestOutcomeSameRequestIsIdempotentAndConflictAvoidsGitHub(t *testing.T) {
	store, github, service := outcomeFixture()
	github.pull.State = "open"
	first, _, err := service.Record(context.Background(), store.job.ID, coding.OutcomeAbandoned)
	if err != nil {
		t.Fatal(err)
	}
	github.err = errors.New("must not re-observe after immutable first write")
	repeated, created, err := service.Record(context.Background(), store.job.ID, coding.OutcomeAbandoned)
	if err != nil || created || repeated != first || github.calls != 1 || store.writes != 1 {
		t.Fatalf("repeat=%#v created=%t calls=%d writes=%d err=%v", repeated, created, github.calls, store.writes, err)
	}
	if _, _, err := service.Record(context.Background(), store.job.ID, coding.OutcomeRejected); err == nil || github.calls != 1 || store.writes != 1 {
		t.Fatalf("conflict calls=%d writes=%d err=%v", github.calls, store.writes, err)
	}
}

func TestOutcomeRequiresAuthorityCheckBeforeWrite(t *testing.T) {
	for _, test := range []struct {
		name  string
		check func(context.Context) error
	}{
		{name: "missing"},
		{name: "claim lost", check: func(context.Context) error { return errors.New("claim lost") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, github, service := outcomeFixture()
			github.pull.State, github.pull.Merged, github.pull.MergeCommitOID = "closed", true, strings.Repeat("b", 40)
			service.claimCheck = test.check

			if _, _, err := service.Record(context.Background(), store.job.ID, coding.OutcomeAccepted); err == nil || store.writes != 0 {
				t.Fatalf("unauthorized outcome write: writes=%d err=%v", store.writes, err)
			}
		})
	}
}
