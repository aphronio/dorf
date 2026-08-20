package coding

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/repository"
	"github.com/aphronio/dorf/internal/spine"
)

const commandEvidenceProducer = "dorf-command-observer"

type Store interface {
	spine.CodingStore
	spine.ReviewStore
	PrepareAgentRun(context.Context, string, string, string) error
	BindAgentRun(context.Context, string, string, string, string, string) error
	UncertainAgentRun(context.Context, string, string) error
	AgentRunAttention(context.Context, string, string) error
	FailAgentRun(context.Context, string, string) error
	RecordSandboxActionSuccess(context.Context, string) error
}

type Externals interface {
	Harness() string
	spine.RepositoryExternals
	spine.ReviewExternals
}

type RepositoryExecution interface {
	repository.Execution
	ObserveRevision(context.Context, spine.Job, string, string) (spine.RevisionObservation, error)
}

// Service owns setup, Revision, Check, and review execution for the
// coding-to-proposal workflow while delegating shared custody to Core.
type Service struct {
	RepositoryExecution
	store      Store
	externals  Externals
	blobs      blob.Store
	barrier    spine.FaultBarrier
	claimCheck func(context.Context) error
}

func NewService(repository RepositoryExecution, store Store, externals Externals, blobs blob.Store, barrier spine.FaultBarrier, claimCheck func(context.Context) error) Service {
	return Service{RepositoryExecution: repository, store: store, externals: externals, blobs: blobs, barrier: barrier, claimCheck: claimCheck}
}

func (s Service) BlobStore() blob.Store { return s.blobs }

func (s Service) ExecuteSetup(ctx context.Context, job spine.CodingJob, action spine.Action) error {
	observation, declared, err := s.externals.RepositorySetup(ctx, job, action)
	if err != nil {
		if attentionNeeded(err) {
			return s.setWorkflowAttention(ctx, job.ID, action.ID, err)
		}
		return err
	}
	observation = canonicalCommandObservation(observation)
	artifact, err := commandArtifact(action.ID, job.StartingRevision, observation)
	if err != nil {
		return err
	}
	evidenceRecord, err := s.retainEvidence(action.ID, "repository-setup", action.ID, "", "", job.StartingRevision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.reachWorkflow(ctx, spine.BarrierSetupComplete, job.ID, action.ID); err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return s.store.RecordSetup(ctx, action.ID, evidenceRecord, observation, declared)
}

func (s Service) ObserveRevision(ctx context.Context, job spine.CodingJob, run spine.AgentRun) error {
	observation, err := s.RepositoryExecution.ObserveRevision(ctx, job.Job, job.Branch, job.Revision)
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
	evidenceRecord, err := s.retainEvidence(run.ID, "git-revision", "", run.ID, "", observation.Revision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return s.store.RecordRevisionObservation(ctx, job.ID, run.ID, observation, evidenceRecord)
}

func (s Service) ExecuteCheck(ctx context.Context, job spine.CodingJob, check spine.Check) error {
	if check.State == "passed" {
		return nil
	}
	if check.State == "failed" {
		return s.handleFailedCheck(ctx, check)
	}
	observation, err := s.externals.RepositoryCheck(ctx, job, check)
	if err != nil {
		if attentionNeeded(err) {
			return s.setWorkflowAttention(ctx, job.ID, check.ID, err)
		}
		return err
	}
	observation = canonicalCommandObservation(observation)
	artifact, err := commandArtifact(check.ID, job.Revision, observation)
	if err != nil {
		return err
	}
	evidenceRecord, err := s.retainEvidence(check.ID, "check-output", "", "", check.ID, job.Revision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.reachWorkflow(ctx, spine.BarrierCheckExited, job.ID, check.ID); err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	if err := s.store.RecordCheck(ctx, check, evidenceRecord, observation); err != nil {
		return err
	}
	check.State, check.ExitCode, check.EvidenceID = "passed", observation.ExitCode, evidenceRecord.ID
	if observation.ExitCode != 0 {
		check.State = "failed"
		return s.handleFailedCheck(ctx, check)
	}
	return nil
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

func (s Service) handleFailedCheck(ctx context.Context, check spine.Check) error {
	_, _, err := s.store.AdmitCheckMessage(ctx, check)
	return err
}

func canonicalCommandObservation(observation spine.CommandObservation) spine.CommandObservation {
	observation.StartedAt = observation.StartedAt.UTC().Truncate(time.Microsecond)
	observation.FinishedAt = observation.FinishedAt.UTC().Truncate(time.Microsecond)
	return observation
}

func commandArtifact(identity, revision string, observation spine.CommandObservation) ([]byte, error) {
	return json.Marshal(struct {
		Identity        string    `json:"identity"`
		Revision        string    `json:"revision"`
		Producer        string    `json:"producer"`
		Command         string    `json:"command"`
		ExitCode        int       `json:"exit_code"`
		StartedAt       time.Time `json:"started_at"`
		FinishedAt      time.Time `json:"finished_at"`
		Stdout          string    `json:"stdout"`
		Stderr          string    `json:"stderr"`
		StdoutTruncated bool      `json:"stdout_truncated"`
		StderrTruncated bool      `json:"stderr_truncated"`
		Redactions      []string  `json:"redactions"`
	}{identity, revision, commandEvidenceProducer, observation.Command, observation.ExitCode, observation.StartedAt, observation.FinishedAt, string(observation.Stdout), string(observation.Stderr), observation.StdoutCut, observation.StderrCut, observation.Redactions})
}

func (s Service) retainEvidence(ownerID, kind, actionID, agentRunID, checkID, revision string, startedAt, finishedAt time.Time, contents []byte) (spine.Evidence, error) {
	stored, err := s.blobs.Put(contents)
	if err != nil {
		return spine.Evidence{}, err
	}
	return spine.Evidence{ID: spine.EvidenceID(ownerID, kind), Digest: stored.Digest, ByteSize: stored.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: kind, ActionID: actionID, AgentRunID: agentRunID, CheckID: checkID, Revision: revision, StartedAt: startedAt.UTC().Truncate(time.Microsecond), FinishedAt: finishedAt.UTC().Truncate(time.Microsecond)}, nil
}

func (s Service) reach(ctx context.Context, point string, delivery spine.Delivery) error {
	if s.barrier == nil {
		return nil
	}
	return s.barrier.Reach(ctx, point, delivery)
}

func (s Service) reachWorkflow(ctx context.Context, point, jobID, identity string) error {
	if s.barrier == nil {
		return nil
	}
	return s.barrier.ReachWorkflow(ctx, point, jobID, identity)
}
