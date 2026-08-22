package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	policy "github.com/aphronio/dorf/internal/review"
)

const commandEvidenceProducer = "dorf-git-observer"

const (
	ActionRepositoryPush    core.ActionKind = "repository-push"
	ActionGitHubPullRequest core.ActionKind = "github-pull-request"
	ActionReviewCheckout    core.ActionKind = "review-checkout"
)

type Store interface {
	Job(context.Context, string) (core.Job, error)
	JobExists(context.Context, string) (bool, error)
	AdmitCoding(context.Context, Admission) (core.Job, bool, error)
	NextWakeSequence(context.Context, string) (int64, error)
	CodingJob(context.Context, string) (Job, error)
	Sandboxes(context.Context, string) ([]core.Sandbox, error)
	CodingMessages(context.Context, string) ([]MessageRecord, []ReviewRunView, error)
	Revisions(context.Context, string) ([]Revision, error)
	ReviewPlans(context.Context, string) ([]ReviewPlanRecord, error)
	Proposal(context.Context, string) (*Proposal, error)
	Outcome(context.Context, string) (*Outcome, error)
	CodingAgentMessage(context.Context, string) (*core.AgentMessageWork, error)
	BeginPublication(context.Context, string, string) (Job, core.Action, core.Action, error)
	RecordRevisionObservation(context.Context, string, string, gitworkspace.Observation, core.Evidence) error
	SetWorkflowAttention(context.Context, string, string, string) error
	Evidence(context.Context, string) ([]core.Evidence, error)
	Actions(context.Context, string) ([]core.Action, error)
	RecordReviewPolicy(context.Context, ReviewPlanRecord) error
	ReviewRun(context.Context, string) (ReviewRunView, error)
	RecordReviewFeedback(context.Context, string, core.HarnessTurn, core.Evidence) (core.Message, bool, error)
}

type Externals interface {
	Harness() string
	RepositoryChangeFacts(context.Context, Job) (policy.ChangeFacts, error)
	PrepareReviewCheckout(context.Context, Job, ReviewRunView) error
	VerifyReviewCheckout(context.Context, Job, ReviewRunView) (ReviewCheckoutObservation, error)
	ReviewInitialTurn(context.Context, Job, ReviewRunView) (core.HarnessBinding, error)
	ReviewRecover(context.Context, Job, ReviewRunView) (core.HarnessBinding, error)
	ReviewTurns(context.Context, Job, ReviewRunView) (core.HarnessHistory, error)
}

type GitWorkspace interface {
	gitworkspace.Execution
	ObserveRevision(context.Context, core.Job, string, string) (gitworkspace.Observation, error)
}

// Service owns Revision and review execution for the
// coding-to-proposal workflow while delegating shared custody to Core.
type Service struct {
	GitWorkspace
	store      Store
	externals  Externals
	blobs      blob.Store
	claimCheck func(context.Context) error
}

func NewService(workspace GitWorkspace, store Store, externals Externals, blobs blob.Store, claimCheck func(context.Context) error) Service {
	return Service{GitWorkspace: workspace, store: store, externals: externals, blobs: blobs, claimCheck: claimCheck}
}

func (s Service) BlobStore() blob.Store { return s.blobs }

func (s Service) ObserveRevision(ctx context.Context, job Job, messageID string) error {
	messages, _, err := s.store.CodingMessages(ctx, job.ID)
	if err != nil {
		return err
	}
	var producer MessageRecord
	for _, message := range messages {
		if message.Message.ID == messageID {
			producer = message
			break
		}
	}
	if producer.Message.ID == "" || producer.Message.JobID != job.ID || producer.InputRevision != job.Revision || producer.ProducerID == "" || producer.Outcome != "completed" || !producer.StartsTurn {
		return fmt.Errorf("Revision observation has no exact completed implementation Message %s", messageID)
	}
	observation, err := s.GitWorkspace.ObserveRevision(ctx, job.Job, job.Branch, job.Revision)
	if err != nil {
		if attentionNeeded(err) {
			return s.setWorkflowAttention(ctx, job.ID, messageID, err)
		}
		return err
	}
	observation.StartedAt = observation.StartedAt.UTC().Truncate(time.Microsecond)
	observation.FinishedAt = observation.FinishedAt.UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	evidenceRecord, err := s.retainEvidence(producer.ProducerID, "git-revision", "", producer.ProducerID, observation.Revision, observation.StartedAt, observation.FinishedAt, payload)
	if err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return s.store.RecordRevisionObservation(ctx, job.ID, producer.ProducerID, observation, evidenceRecord)
}

func (s Service) requireClaim(ctx context.Context) error {
	if s.claimCheck == nil {
		return errors.New("durable executor claim check is not configured")
	}
	return s.claimCheck(ctx)
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

func (s Service) retainEvidence(ownerID, kind, actionID, agentRunID, revision string, startedAt, finishedAt time.Time, contents []byte) (core.Evidence, error) {
	stored, err := s.blobs.Put(contents)
	if err != nil {
		return core.Evidence{}, err
	}
	return core.Evidence{ID: core.EvidenceID(ownerID, kind), Digest: stored.Digest, ByteSize: stored.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: kind, ActionID: actionID, AgentRunID: agentRunID, Revision: revision, StartedAt: startedAt.UTC().Truncate(time.Microsecond), FinishedAt: finishedAt.UTC().Truncate(time.Microsecond)}, nil
}
