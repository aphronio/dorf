package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/coding"
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
	AddEyesReaction(context.Context, githubapi.Authority, int64) error
	CreateIssueComment(context.Context, githubapi.Authority, int64, string) (githubapi.Comment, error)
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
	Revision    string             `json:"revision"`
	Outcome     coding.OutcomeKind `json:"outcome,omitempty"`
	NewMessages int                `json:"new_messages"`
}

func (r ProposalRuntime) Observe(ctx context.Context, jobID, revision string) (ProposalObservationResultV1, error) {
	job, err := r.Store.CodingJob(ctx, jobID)
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
	if proposal == nil || proposal.ProposedRevision != job.Revision {
		return ProposalObservationResultV1{}, fmt.Errorf("Job proposal observation requires one exact current GitHub proposal")
	}
	authority := githubapi.Authority{Repository: job.GitHubRepository, InstallationID: job.GitHubInstallation}
	pull, err := r.GitHub.PullRequest(ctx, authority, proposal.Number)
	if err != nil {
		return ProposalObservationResultV1{}, fmt.Errorf("observe exact GitHub pull request #%d: %w", proposal.Number, err)
	}
	if err := validateExactProposal(job, *proposal, pull); err != nil {
		return ProposalObservationResultV1{}, err
	}
	result := ProposalObservationResultV1{Revision: job.Revision}
	if pull.State == "closed" {
		result.Outcome = coding.OutcomeRejected
		if pull.Merged {
			result.Outcome = coding.OutcomeAccepted
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
	deliveries, err := r.Store.Deliveries(ctx, jobID)
	if err != nil {
		return ProposalObservationResultV1{}, fmt.Errorf("load admitted Messages before observing GitHub comments: %w", err)
	}
	admitted := admittedGitHubComments(deliveries)
	for _, comment := range comments {
		if !trustedHumanComment(comment) {
			continue
		}
		fromID := fmt.Sprintf("github-comment:%d", comment.ID)
		delivery, exists := admitted[fromID]
		if exists {
			if delivery.AgentRun.State != spine.AgentRunCompleted || hasFeedbackReply(comments, jobID, comment.ID) {
				continue
			}
			if err := absurdruntime.RequireClaim(ctx); err != nil {
				return ProposalObservationResultV1{}, err
			}
			if _, err := r.GitHub.CreateIssueComment(ctx, authority, proposal.Number, feedbackReply(jobID, comment, job.Revision)); err != nil {
				return ProposalObservationResultV1{}, fmt.Errorf("report completed GitHub feedback comment %d: %w", comment.ID, err)
			}
			continue
		}
		if err := absurdruntime.RequireClaim(ctx); err != nil {
			return ProposalObservationResultV1{}, err
		}
		if err := r.GitHub.AddEyesReaction(ctx, authority, comment.ID); err != nil {
			return ProposalObservationResultV1{}, fmt.Errorf("acknowledge GitHub feedback comment %d: %w", comment.ID, err)
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
	}
	return result, nil
}

func admittedGitHubComments(deliveries []spine.Delivery) map[string]spine.Delivery {
	admitted := make(map[string]spine.Delivery)
	for _, delivery := range deliveries {
		message := delivery.Message
		if message.FromKind == spine.MessageFromHuman && strings.HasPrefix(message.FromID, "github-comment:") {
			admitted[message.FromID] = delivery
		}
	}
	return admitted
}

func feedbackReply(jobID string, comment githubapi.Comment, revision string) string {
	quoted := "> " + strings.ReplaceAll(strings.TrimSpace(comment.Body), "\n", "\n> ")
	return fmt.Sprintf("Regarding feedback from @%s:\n\n%s\n\nDorf handled this feedback in exact Revision `%s`.\n\n%s", comment.Login, quoted, revision, feedbackReplyMarker(jobID, comment.ID))
}

func feedbackReplyMarker(jobID string, commentID int64) string {
	return fmt.Sprintf("<!-- dorf-feedback:%s:%d -->", jobID, commentID)
}

func hasFeedbackReply(comments []githubapi.Comment, jobID string, commentID int64) bool {
	marker := feedbackReplyMarker(jobID, commentID)
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			return true
		}
	}
	return false
}

func validateExactProposal(job coding.Job, proposal coding.Proposal, pull githubapi.PullRequest) error {
	if pull.Number != proposal.Number || pull.URL != proposal.URL || pull.Repository != job.GitHubRepository ||
		pull.Head != job.Branch || pull.Base != job.BaseBranch || pull.HeadSHA != proposal.ProposedRevision {
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
