package outcome

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	githubapi "github.com/aphronio/dorf/internal/github"
)

var ErrUnavailable = errors.New("outcome unavailable")

type Store interface {
	CodingJob(context.Context, string) (coding.Job, error)
	Proposal(context.Context, string) (*coding.Proposal, error)
	Outcome(context.Context, string) (*coding.Outcome, error)
	RecordOutcome(context.Context, coding.Outcome) (coding.Outcome, bool, error)
	WithJobFence(context.Context, string, func() error) error
}

type GitHub interface {
	PullRequest(context.Context, githubapi.Authority, int64) (githubapi.PullRequest, error)
}

type Service struct {
	Store      Store
	GitHub     GitHub
	Now        func() time.Time
	claimCheck func(context.Context) error
}

// WithClaimCheck returns a Service bound to the authority that may record the
// observed terminal outcome.
func (s Service) WithClaimCheck(check func(context.Context) error) Service {
	s.claimCheck = check
	return s
}

func (s Service) Record(ctx context.Context, jobID string, requested coding.OutcomeKind) (coding.Outcome, bool, error) {
	var receipt coding.Outcome
	var created bool
	err := s.Store.WithJobFence(ctx, jobID, func() error {
		var err error
		receipt, created, err = s.recordFenced(ctx, jobID, requested)
		return err
	})
	return receipt, created, err
}

func (s Service) recordFenced(ctx context.Context, jobID string, requested coding.OutcomeKind) (coding.Outcome, bool, error) {
	if requested != coding.OutcomeAccepted && requested != coding.OutcomeRejected && requested != coding.OutcomeAbandoned {
		return coding.Outcome{}, false, fmt.Errorf("outcome must be accepted, rejected, or abandoned")
	}
	existing, err := s.Store.Outcome(ctx, jobID)
	if err != nil {
		return coding.Outcome{}, false, err
	}
	if existing != nil {
		if existing.Kind != requested {
			return coding.Outcome{}, false, fmt.Errorf("%w: Job %s already has immutable %s outcome; refusing conflicting %s outcome", ErrUnavailable, jobID, existing.Kind, requested)
		}
		return *existing, false, nil
	}
	job, err := s.Store.CodingJob(ctx, jobID)
	if err != nil {
		return coding.Outcome{}, false, err
	}
	proposal, err := s.Store.Proposal(ctx, jobID)
	if err != nil {
		return coding.Outcome{}, false, err
	}
	receipt := coding.Outcome{JobID: job.ID, Kind: requested}
	if proposal == nil {
		if requested != coding.OutcomeAbandoned {
			return coding.Outcome{}, false, fmt.Errorf("accepted and rejected outcomes require one exact current GitHub proposal")
		}
	} else {
		if proposal.ProposedRevision != job.Revision {
			return coding.Outcome{}, false, fmt.Errorf("Job outcome requires one exact current GitHub proposal")
		}
		authority := githubapi.Authority{Repository: job.GitHubRepository, InstallationID: job.GitHubInstallation}
		pull, err := s.GitHub.PullRequest(ctx, authority, proposal.Number)
		if err != nil {
			return coding.Outcome{}, false, fmt.Errorf("observe exact GitHub pull request #%d: %w", proposal.Number, err)
		}
		if pull.Number != proposal.Number || pull.URL != proposal.URL || pull.Repository != job.GitHubRepository || pull.Head != job.Branch || pull.Base != job.BaseBranch || pull.HeadSHA != proposal.ProposedRevision {
			return coding.Outcome{}, false, fmt.Errorf("GitHub pull request conflicts with the exact stored proposal identity or proposed Revision")
		}
		switch requested {
		case coding.OutcomeAccepted:
			if pull.State != "closed" || !pull.Merged || pull.MergeCommitOID == "" {
				return coding.Outcome{}, false, fmt.Errorf("accepted outcome requires exact pull request #%d to report merged", pull.Number)
			}
		case coding.OutcomeRejected:
			if pull.State != "closed" || pull.Merged {
				return coding.Outcome{}, false, fmt.Errorf("rejected outcome requires exact pull request #%d to report closed and unmerged", pull.Number)
			}
		case coding.OutcomeAbandoned:
			if pull.Merged {
				return coding.Outcome{}, false, fmt.Errorf("%w: abandoned outcome refuses exact pull request #%d because it is already merged", ErrUnavailable, pull.Number)
			}
		}
		receipt.ObservedState = pull.State
		receipt.ObservedMerged = pull.Merged
		receipt.MergeCommitOID = pull.MergeCommitOID
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	receipt.ObservedAt = now().UTC()
	if s.claimCheck == nil {
		return coding.Outcome{}, false, fmt.Errorf("outcome authority check is not configured")
	}
	if err := s.claimCheck(ctx); err != nil {
		return coding.Outcome{}, false, err
	}
	return s.Store.RecordOutcome(ctx, receipt)
}
