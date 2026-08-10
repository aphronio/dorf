package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/outcome"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/publication"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type ProposalGitHub interface {
	PullRequest(context.Context, githubapi.Authority, int64) (githubapi.PullRequest, error)
	IssueComments(context.Context, githubapi.Authority, int64) ([]githubapi.Comment, error)
}

// ProposalRuntime holds the coding proposal workflow's concrete dependencies.
// The durable Job remains authoritative; GitHub only supplies observations.
type ProposalRuntime struct {
	Publication  publication.Service
	GitHub       ProposalGitHub
	Outcome      outcome.Service
	Store        postgres.Store
	Client       *absurd.Client
	PollInterval time.Duration
}

type ProposalObservationResultV1 struct {
	Revision    string               `json:"revision"`
	Poll        int                  `json:"poll"`
	Outcome     spine.JobOutcomeKind `json:"outcome,omitempty"`
	NewMessages int                  `json:"new_messages"`
}

func (r ProposalRuntime) Observe(ctx context.Context, jobID, revision string, poll int) (ProposalObservationResultV1, error) {
	job, err := r.Store.Job(ctx, jobID)
	if err != nil {
		return ProposalObservationResultV1{}, err
	}
	if job.Revision != revision {
		return ProposalObservationResultV1{}, fmt.Errorf("proposal observation Revision=%s conflicts with Job Revision=%s", revision, job.Revision)
	}
	proposal, err := r.Store.Proposal(ctx, jobID)
	if err != nil {
		return ProposalObservationResultV1{}, err
	}
	if proposal == nil || proposal.Stale || proposal.ProposedRevision != job.Revision {
		return ProposalObservationResultV1{}, fmt.Errorf("Job proposal observation requires one exact current GitHub proposal")
	}
	authority := githubapi.Authority{Repository: proposal.Repository, InstallationID: proposal.InstallationID}
	pull, err := r.GitHub.PullRequest(ctx, authority, proposal.Number)
	if err != nil {
		return ProposalObservationResultV1{}, fmt.Errorf("observe exact GitHub pull request #%d: %w", proposal.Number, err)
	}
	if err := validateExactProposal(*proposal, pull); err != nil {
		return ProposalObservationResultV1{}, err
	}
	result := ProposalObservationResultV1{Revision: job.Revision, Poll: poll}
	if pull.State == "closed" {
		result.Outcome = spine.OutcomeRejected
		if pull.Merged {
			result.Outcome = spine.OutcomeAccepted
		}
		if _, _, err := r.Outcome.Record(ctx, jobID, result.Outcome); err != nil {
			return ProposalObservationResultV1{}, err
		}
		return result, nil
	}
	if pull.State != "open" {
		return ProposalObservationResultV1{}, fmt.Errorf("exact GitHub pull request #%d has unsupported state %q", pull.Number, pull.State)
	}
	comments, err := r.GitHub.IssueComments(ctx, authority, proposal.Number)
	if err != nil {
		return ProposalObservationResultV1{}, fmt.Errorf("observe comments on exact GitHub pull request #%d: %w", proposal.Number, err)
	}
	messages, err := r.Store.Messages(ctx, jobID)
	if err != nil {
		return ProposalObservationResultV1{}, fmt.Errorf("load admitted Messages before observing GitHub comments: %w", err)
	}
	seen := admittedGitHubComments(messages)
	for _, comment := range comments {
		if !trustedHumanComment(comment) {
			continue
		}
		fromID := fmt.Sprintf("github-comment:%d", comment.ID)
		if seen[fromID] {
			continue
		}
		if err := absurdruntime.RequireClaim(ctx); err != nil {
			return ProposalObservationResultV1{}, err
		}
		_, created, err := AdmitMessage(ctx, r.Store, r.Client, postgres.NewMessage{
			JobID: jobID, FromKind: spine.MessageFromHuman,
			FromID: fromID, Input: comment.Body,
			Intent: spine.MessageFollow,
		})
		if err != nil {
			return ProposalObservationResultV1{}, err
		}
		if created {
			result.NewMessages++
		}
		seen[fromID] = true
	}
	return result, nil
}

func admittedGitHubComments(messages []spine.MessageView) map[string]bool {
	seen := make(map[string]bool)
	for _, message := range messages {
		if message.FromKind == spine.MessageFromHuman && strings.HasPrefix(message.FromID, "github-comment:") {
			seen[message.FromID] = true
		}
	}
	return seen
}

func validateExactProposal(proposal spine.GitHubProposal, pull githubapi.PullRequest) error {
	if pull.Number != proposal.Number || pull.URL != proposal.URL || pull.Repository != proposal.Repository ||
		pull.Head != proposal.HeadBranch || pull.Base != proposal.BaseBranch || pull.HeadSHA != proposal.ProposedRevision {
		return fmt.Errorf("GitHub pull request conflicts with the exact stored proposal identity or proposed Revision")
	}
	return nil
}

func trustedHumanComment(comment githubapi.Comment) bool {
	if comment.ID < 1 || comment.UserType != "User" {
		return false
	}
	return comment.AuthorAssociation == "OWNER" || comment.AuthorAssociation == "COLLABORATOR"
}
