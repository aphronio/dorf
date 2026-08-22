package coding

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	policy "github.com/aphronio/dorf/internal/review"
)

const reviewEvidenceProducer = "dorf-agent-review"

type reviewBoundaryError string

func (e reviewBoundaryError) Error() string         { return string(e) }
func (e reviewBoundaryError) AttentionNeeded() bool { return true }

func (s Service) PlanReview(ctx context.Context, job Job) error {
	facts, err := s.externals.RepositoryChangeFacts(ctx, job)
	if err != nil {
		return s.setWorkflowAttention(ctx, job.ID, ReviewPolicyAttentionSource(job.Revision), fmt.Errorf("deterministic ChangeFacts failed: %w", err))
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		return s.setWorkflowAttention(ctx, job.ID, ReviewPolicyAttentionSource(job.Revision), fmt.Errorf("mandatory ReviewPolicy rejected input: %w", err))
	}
	return s.recordReviewPolicy(ctx, ReviewPlanRecord{JobID: job.ID, Revision: job.Revision, Facts: facts, Plan: plan})
}

func (s Service) recordReviewPolicy(ctx context.Context, record ReviewPlanRecord) error {
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return s.store.RecordReviewPolicy(ctx, record)
}

func (s Service) RecordReviewResult(ctx context.Context, job Job, messageID string) error {
	result, err := s.ObserveSettledAgentMessage(ctx, job.ID, messageID)
	if err != nil {
		return err
	}
	run, err := s.store.ReviewRun(ctx, core.AgentRunID(messageID))
	if err != nil {
		return err
	}
	if result.MessageID != messageID || result.Outcome != "completed" || run.MessageID != messageID ||
		run.JobID != job.ID || run.InputRevision != job.Revision || run.Outcome != "completed" {
		return fmt.Errorf("review Message %s does not belong to a completed current-Revision review", messageID)
	}
	if strings.TrimSpace(result.Output) == "" {
		return s.setWorkflowAttention(ctx, job.ID, messageID, reviewBoundaryError(fmt.Sprintf("review Role %s returned no feedback text", run.Role)))
	}
	checkout, err := s.verifyReviewCheckout(ctx, job, run)
	if err != nil {
		if attentionNeeded(err) {
			return s.setWorkflowAttention(ctx, job.ID, messageID, err)
		}
		return err
	}
	observed, err := s.reviewEvidence(run, checkout)
	if err != nil {
		return err
	}
	outcome := core.HarnessTurn{ID: run.TurnID, Status: result.Outcome, Output: result.Output}
	_, _, err = s.recordReviewFeedback(ctx, run.ID, outcome, observed)
	return err
}

func (s Service) ExecuteReviewCheckout(ctx context.Context, job Job, runID string, action core.Action) error {
	run, err := s.store.ReviewRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.JobID != job.ID || run.InputRevision != job.Revision || run.SandboxID != ReviewSandboxName(job.ID, run.ID) || run.Sandbox.ID != run.SandboxID ||
		action.ID != core.ScopedActionID(job.ID, ActionReviewCheckout, run.Sandbox.ID) || action.JobID != job.ID || action.Kind != ActionReviewCheckout || action.Scope != run.Sandbox.ID {
		return reviewBoundaryError("review checkout Action does not belong to the exact selected AgentRun, Revision, and Sandbox")
	}
	if action.State == core.ActionSucceeded {
		return nil
	}
	return s.ExecuteSandboxActionEffect(ctx, job.ID, action.ID, ActionReviewCheckout, func(effectCtx context.Context, authoritativeJob core.Job, authoritativeSandbox core.Sandbox) error {
		if authoritativeJob.ID != job.ID || authoritativeSandbox.ID != run.Sandbox.ID {
			return reviewBoundaryError("review checkout authority changed before execution")
		}
		return s.externals.PrepareReviewCheckout(effectCtx, job, run)
	})
}

func (s Service) recordReviewFeedback(ctx context.Context, runID string, outcome core.HarnessTurn, observed core.Evidence) (core.Message, bool, error) {
	if err := s.requireClaim(ctx); err != nil {
		return core.Message{}, false, err
	}
	return s.store.RecordReviewFeedback(ctx, runID, outcome, observed)
}

func (s Service) verifyReviewCheckout(ctx context.Context, job Job, run ReviewRunView) (ReviewCheckoutObservation, error) {
	checkout, err := s.externals.VerifyReviewCheckout(ctx, job, run)
	if err != nil {
		reason := "review checkout verification failed: " + err.Error()
		return ReviewCheckoutObservation{}, reviewBoundaryError(reason)
	}
	if checkout.Revision != run.InputRevision || !fullGitObjectID(checkout.Tree) {
		reason := "review checkout is not the exact Revision with a full tree identity"
		return ReviewCheckoutObservation{}, reviewBoundaryError(reason)
	}
	return checkout, nil
}

func fullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func reviewRunAttempt(run core.AgentRun, request core.Message, sandbox core.Sandbox) ReviewRunView {
	return ReviewRunView{
		ID: run.ID, JobID: run.JobID, MessageID: run.MessageID, Harness: run.Harness,
		ThreadID: run.ThreadID, TurnID: run.TurnID, Outcome: run.TurnOutcome, Attention: run.Attention,
		Role: run.Role, InputRevision: run.InputRevision, Capability: run.Capability,
		SandboxID: run.SandboxID, SubmissionNonce: run.SubmissionNonce,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, Request: request, Sandbox: sandbox,
	}
}

func (s Service) reviewEvidence(run ReviewRunView, checkout ReviewCheckoutObservation) (core.Evidence, error) {
	finished := run.FinishedAt.UTC().Truncate(time.Microsecond)
	started := run.StartedAt.UTC().Truncate(time.Microsecond)
	if started.IsZero() || finished.IsZero() || started.After(finished) {
		return core.Evidence{}, fmt.Errorf("review AgentRun %s has no stable bounded timing", run.ID)
	}
	artifact, err := json.Marshal(reviewObservationArtifact{
		AgentRunID: run.ID, Revision: run.InputRevision, Role: run.Role, Capability: run.Capability,
		Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.Outcome, Checkout: checkout,
	})
	if err != nil {
		return core.Evidence{}, err
	}
	stored, err := s.blobs.Put(artifact)
	if err != nil {
		return core.Evidence{}, err
	}
	return core.Evidence{ID: core.EvidenceID(run.ID, "review-observation"), Digest: stored.Digest, ByteSize: stored.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: reviewEvidenceProducer, Kind: "review-observation", AgentRunID: run.ID, Revision: run.InputRevision, StartedAt: started, FinishedAt: finished}, nil
}
