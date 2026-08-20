package coding

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/gitworkspace"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

const commandEvidenceProducer = "dorf-git-observer"

const (
	ActionRepositoryPush    spine.ActionKind = "repository-push"
	ActionGitHubPullRequest spine.ActionKind = "github-pull-request"
	ActionReviewCheckout    spine.ActionKind = "review-checkout"
)

type Store interface {
	RecordRevisionObservation(context.Context, string, string, spine.RevisionObservation, spine.Evidence) error
	SetWorkflowAttention(context.Context, string, string, string) error
	Evidence(context.Context, string) ([]spine.Evidence, error)
	Actions(context.Context, string) ([]spine.Action, error)
	RecordReviewPolicy(context.Context, spine.ReviewPlanRecord) error
	ReviewRun(context.Context, string) (spine.ReviewRunView, error)
	RecordReviewFeedback(context.Context, string, spine.HarnessTurn, spine.Evidence) (spine.Message, bool, error)
	PrepareAgentRun(context.Context, string, string, string) error
	BindAgentRun(context.Context, string, string, string, string, string) error
	UncertainAgentRun(context.Context, string, string) error
	AgentRunAttention(context.Context, string, string) error
	FailAgentRun(context.Context, string, string) error
	RecordSandboxActionSuccess(context.Context, string) error
}

type Externals interface {
	Harness() string
	RepositoryChangeFacts(context.Context, spine.CodingJob) (policy.ChangeFacts, error)
	PrepareReviewCheckout(context.Context, spine.CodingJob, spine.ReviewRunView) error
	VerifyReviewCheckout(context.Context, spine.CodingJob, spine.ReviewRunView) (spine.ReviewCheckoutObservation, error)
	ReviewInitialTurn(context.Context, spine.CodingJob, spine.ReviewRunView) (spine.HarnessBinding, error)
	ReviewRecover(context.Context, spine.CodingJob, spine.ReviewRunView) (spine.HarnessBinding, error)
	ReviewTurns(context.Context, spine.CodingJob, spine.ReviewRunView) (spine.HarnessHistory, error)
	ReviewWait(context.Context, spine.CodingJob, spine.ReviewRunView, string) (spine.HarnessBinding, error)
}

type GitWorkspace interface {
	gitworkspace.Execution
	ObserveRevision(context.Context, spine.Job, string, string) (spine.RevisionObservation, error)
}

// Service owns Revision and review execution for the
// coding-to-proposal workflow while delegating shared custody to Core.
type Service struct {
	GitWorkspace
	store      Store
	externals  Externals
	blobs      blob.Store
	barrier    spine.FaultBarrier
	claimCheck func(context.Context) error
}

func NewService(workspace GitWorkspace, store Store, externals Externals, blobs blob.Store, barrier spine.FaultBarrier, claimCheck func(context.Context) error) Service {
	return Service{GitWorkspace: workspace, store: store, externals: externals, blobs: blobs, barrier: barrier, claimCheck: claimCheck}
}

func (s Service) BlobStore() blob.Store { return s.blobs }

func (s Service) ObserveRevision(ctx context.Context, job spine.CodingJob, run spine.AgentRun) error {
	observation, err := s.GitWorkspace.ObserveRevision(ctx, job.Job, job.Branch, job.Revision)
	if err != nil {
		if attentionNeeded(err) {
			return s.setWorkflowAttention(ctx, job.ID, run.ID, err)
		}
		return err
	}
	observation.StartedAt = observation.StartedAt.UTC().Truncate(time.Microsecond)
	observation.FinishedAt = observation.FinishedAt.UTC().Truncate(time.Microsecond)
	artifact, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	evidenceRecord, err := s.retainEvidence(run.ID, "git-revision", "", run.ID, observation.Revision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return s.store.RecordRevisionObservation(ctx, job.ID, run.ID, observation, evidenceRecord)
}

func (s Service) requireClaim(ctx context.Context) error {
	if s.claimCheck == nil {
		return errors.New("durable executor claim check is not configured")
	}
	return s.claimCheck(ctx)
}

func (s Service) recordAgentRun(ctx context.Context, record func() error) error {
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return record()
}

func (s Service) agentRunAttention(ctx context.Context, runID, detail string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.AgentRunAttention(ctx, runID, detail) })
}

func (s Service) failAgentRun(ctx context.Context, runID, reason string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.FailAgentRun(ctx, runID, reason) })
}

func (s Service) uncertainAgentRun(ctx context.Context, runID, reason string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.UncertainAgentRun(ctx, runID, reason) })
}

func (s Service) setWorkflowAttention(ctx context.Context, jobID, source string, cause error) error {
	if err := s.requireClaim(ctx); err != nil {
		return errors.Join(cause, err)
	}
	if err := s.store.SetWorkflowAttention(ctx, jobID, source, cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func attentionNeeded(err error) bool {
	var attention interface{ AttentionNeeded() bool }
	return errors.As(err, &attention) && attention.AttentionNeeded()
}

func (s Service) retainEvidence(ownerID, kind, actionID, agentRunID, revision string, startedAt, finishedAt time.Time, contents []byte) (spine.Evidence, error) {
	stored, err := s.blobs.Put(contents)
	if err != nil {
		return spine.Evidence{}, err
	}
	return spine.Evidence{ID: spine.EvidenceID(ownerID, kind), Digest: stored.Digest, ByteSize: stored.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: kind, ActionID: actionID, AgentRunID: agentRunID, Revision: revision, StartedAt: startedAt.UTC().Truncate(time.Microsecond), FinishedAt: finishedAt.UTC().Truncate(time.Microsecond)}, nil
}

func (s Service) reach(ctx context.Context, point string, delivery spine.Delivery) error {
	if s.barrier == nil {
		return nil
	}
	return s.barrier.Reach(ctx, point, delivery)
}
