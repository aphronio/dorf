package outcome

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/spine"
)

type outcomeStore struct {
	job      spine.Job
	proposal *spine.GitHubProposal
	outcome  *spine.JobOutcome
	writes   int
}

func (s *outcomeStore) Job(context.Context, string) (spine.Job, error) { return s.job, nil }
func (s *outcomeStore) Proposal(context.Context, string) (*spine.GitHubProposal, error) {
	return s.proposal, nil
}
func (s *outcomeStore) Outcome(context.Context, string) (*spine.JobOutcome, error) {
	return s.outcome, nil
}
func (s *outcomeStore) RecordOutcome(_ context.Context, receipt spine.JobOutcome) (spine.JobOutcome, bool, error) {
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
		job:      spine.Job{ID: "job-exact", Revision: revision, GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield", Branch: "dorf/issue-39"},
		proposal: &spine.GitHubProposal{JobID: "job-exact", Number: 39, URL: "https://github.com/aphronio/dorf/pull/39", ProposedRevision: revision},
	}
	github := &outcomeGitHub{pull: githubapi.PullRequest{Number: 39, URL: store.proposal.URL, Repository: "aphronio/dorf", Head: "dorf/issue-39", Base: "greenfield", HeadSHA: revision}}
	service := (Service{Store: store, GitHub: github, Now: func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) }}).WithClaimCheck(func(context.Context) error { return nil })
	return store, github, service
}

func TestExactGitHubAuthorityRecordsThreeDistinctOutcomes(t *testing.T) {
	for _, test := range []struct {
		kind   spine.JobOutcomeKind
		state  string
		merged bool
		merge  string
	}{
		{spine.OutcomeAccepted, "closed", true, strings.Repeat("b", 40)},
		{spine.OutcomeRejected, "closed", false, ""},
		{spine.OutcomeAbandoned, "open", false, ""},
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

	receipt, created, err := service.Record(context.Background(), store.job.ID, spine.OutcomeAbandoned)
	if err != nil || !created || store.writes != 1 || github.calls != 0 {
		t.Fatalf("receipt=%#v created=%t writes=%d calls=%d err=%v", receipt, created, store.writes, github.calls, err)
	}
	if receipt.ObservedState != "" || receipt.ObservedMerged || receipt.MergeCommitOID != "" {
		t.Fatalf("pre-Proposal abandonment invented GitHub authority: %#v", receipt)
	}
	if _, _, err := func() (spine.JobOutcome, bool, error) {
		store.outcome = nil
		return service.Record(context.Background(), store.job.ID, spine.OutcomeRejected)
	}(); err == nil || github.calls != 0 {
		t.Fatalf("rejected outcome without Proposal reached GitHub: calls=%d err=%v", github.calls, err)
	}
}

func TestOutcomeFailsClosedForWrongAuthorityOrGitHubState(t *testing.T) {
	for _, test := range []struct {
		name   string
		kind   spine.JobOutcomeKind
		mutate func(*outcomeStore, *outcomeGitHub)
	}{
		{"accepted-unmerged", spine.OutcomeAccepted, func(_ *outcomeStore, g *outcomeGitHub) { g.pull.State = "closed" }},
		{"rejected-open", spine.OutcomeRejected, func(_ *outcomeStore, g *outcomeGitHub) { g.pull.State = "open" }},
		{"abandoned-merged", spine.OutcomeAbandoned, func(_ *outcomeStore, g *outcomeGitHub) {
			g.pull.State, g.pull.Merged, g.pull.MergeCommitOID = "closed", true, strings.Repeat("b", 40)
		}},
		{"wrong-number", spine.OutcomeAbandoned, func(_ *outcomeStore, g *outcomeGitHub) { g.pull.State, g.pull.Number = "open", 40 }},
		{"wrong-revision", spine.OutcomeAbandoned, func(_ *outcomeStore, g *outcomeGitHub) {
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
	first, _, err := service.Record(context.Background(), store.job.ID, spine.OutcomeAbandoned)
	if err != nil {
		t.Fatal(err)
	}
	github.err = errors.New("must not re-observe after immutable first write")
	repeated, created, err := service.Record(context.Background(), store.job.ID, spine.OutcomeAbandoned)
	if err != nil || created || repeated != first || github.calls != 1 || store.writes != 1 {
		t.Fatalf("repeat=%#v created=%t calls=%d writes=%d err=%v", repeated, created, github.calls, store.writes, err)
	}
	if _, _, err := service.Record(context.Background(), store.job.ID, spine.OutcomeRejected); err == nil || github.calls != 1 || store.writes != 1 {
		t.Fatalf("conflict calls=%d writes=%d err=%v", github.calls, store.writes, err)
	}
}

func TestOutcomeRequiresClaimImmediatelyBeforeWrite(t *testing.T) {
	store, github, service := outcomeFixture()
	github.pull.State, github.pull.Merged, github.pull.MergeCommitOID = "closed", true, strings.Repeat("b", 40)
	service = service.WithClaimCheck(func(context.Context) error { return errors.New("claim lost") })

	if _, _, err := service.Record(context.Background(), store.job.ID, spine.OutcomeAccepted); err == nil || store.writes != 0 {
		t.Fatalf("lost claim wrote outcome: writes=%d err=%v", store.writes, err)
	}
}

func TestOutcomeRefusesWriteWithoutAuthorityCheck(t *testing.T) {
	store, github, service := outcomeFixture()
	github.pull.State = "open"
	service.claimCheck = nil

	if _, _, err := service.Record(context.Background(), store.job.ID, spine.OutcomeAbandoned); err == nil || store.writes != 0 {
		t.Fatalf("missing authority check wrote outcome: writes=%d err=%v", store.writes, err)
	}
}
