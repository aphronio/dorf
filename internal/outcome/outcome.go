package outcome

import (
	"context"
	"fmt"
	"time"

	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/spine"
)

type Store interface {
	Job(context.Context, string) (spine.Job, error)
	Proposal(context.Context, string) (*spine.GitHubProposal, error)
	Outcome(context.Context, string) (*spine.JobOutcome, error)
	RecordOutcome(context.Context, spine.JobOutcome) (spine.JobOutcome, bool, error)
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

func (s Service) Record(ctx context.Context, jobID string, requested spine.JobOutcomeKind) (spine.JobOutcome, bool, error) {
	var receipt spine.JobOutcome
	var created bool
	err := s.Store.WithJobFence(ctx, jobID, func() error {
		var err error
		receipt, created, err = s.recordFenced(ctx, jobID, requested)
		return err
	})
	return receipt, created, err
}

func (s Service) recordFenced(ctx context.Context, jobID string, requested spine.JobOutcomeKind) (spine.JobOutcome, bool, error) {
	if requested != spine.OutcomeAccepted && requested != spine.OutcomeRejected && requested != spine.OutcomeAbandoned {
		return spine.JobOutcome{}, false, fmt.Errorf("outcome must be accepted, rejected, or abandoned")
	}
	existing, err := s.Store.Outcome(ctx, jobID)
	if err != nil {
		return spine.JobOutcome{}, false, err
	}
	if existing != nil {
		if existing.Kind != requested {
			return spine.JobOutcome{}, false, fmt.Errorf("Job %s already has immutable %s outcome; refusing conflicting %s outcome", jobID, existing.Kind, requested)
		}
		return *existing, false, nil
	}
	job, err := s.Store.Job(ctx, jobID)
	if err != nil {
		return spine.JobOutcome{}, false, err
	}
	proposal, err := s.Store.Proposal(ctx, jobID)
	if err != nil {
		return spine.JobOutcome{}, false, err
	}
	receipt := spine.JobOutcome{JobID: job.ID, Kind: requested}
	if proposal == nil {
		if requested != spine.OutcomeAbandoned {
			return spine.JobOutcome{}, false, fmt.Errorf("accepted and rejected outcomes require one exact current GitHub proposal")
		}
	} else {
		if proposal.ProposedRevision != job.Revision {
			return spine.JobOutcome{}, false, fmt.Errorf("Job outcome requires one exact current GitHub proposal")
		}
		authority := githubapi.Authority{Repository: job.GitHubRepository, InstallationID: job.GitHubInstallation}
		pull, err := s.GitHub.PullRequest(ctx, authority, proposal.Number)
		if err != nil {
			return spine.JobOutcome{}, false, fmt.Errorf("observe exact GitHub pull request #%d: %w", proposal.Number, err)
		}
		if pull.Number != proposal.Number || pull.URL != proposal.URL || pull.Repository != job.GitHubRepository || pull.Head != job.Branch || pull.Base != job.BaseBranch || pull.HeadSHA != proposal.ProposedRevision {
			return spine.JobOutcome{}, false, fmt.Errorf("GitHub pull request conflicts with the exact stored proposal identity or proposed Revision")
		}
		switch requested {
		case spine.OutcomeAccepted:
			if pull.State != "closed" || !pull.Merged || pull.MergeCommitOID == "" {
				return spine.JobOutcome{}, false, fmt.Errorf("accepted outcome requires exact pull request #%d to report merged", pull.Number)
			}
		case spine.OutcomeRejected:
			if pull.State != "closed" || pull.Merged {
				return spine.JobOutcome{}, false, fmt.Errorf("rejected outcome requires exact pull request #%d to report closed and unmerged", pull.Number)
			}
		case spine.OutcomeAbandoned:
			if pull.Merged {
				return spine.JobOutcome{}, false, fmt.Errorf("abandoned outcome refuses exact pull request #%d because it is already merged", pull.Number)
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
		return spine.JobOutcome{}, false, fmt.Errorf("outcome authority check is not configured")
	}
	if err := s.claimCheck(ctx); err != nil {
		return spine.JobOutcome{}, false, err
	}
	return s.Store.RecordOutcome(ctx, receipt)
}
