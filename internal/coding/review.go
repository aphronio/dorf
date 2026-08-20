package coding

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

const reviewEvidenceProducer = "dorf-agent-review"

type reviewBoundaryError string

func (e reviewBoundaryError) Error() string         { return string(e) }
func (e reviewBoundaryError) AttentionNeeded() bool { return true }

type reviewObservationArtifact struct {
	AgentRunID  string                          `json:"agent_run_id"`
	Revision    string                          `json:"revision"`
	Role        string                          `json:"role"`
	Capability  string                          `json:"capability"`
	Harness     string                          `json:"harness"`
	ThreadID    string                          `json:"thread_id"`
	TurnID      string                          `json:"turn_id"`
	TurnOutcome string                          `json:"turn_outcome"`
	Checkout    spine.ReviewCheckoutObservation `json:"checkout"`
}

func (s Service) PlanReview(ctx context.Context, job spine.CodingJob) error {
	declared, err := s.store.DeclaredChecks(ctx, job.ID)
	if err != nil {
		return err
	}
	checks, err := s.store.Checks(ctx, job.ID)
	if err != nil {
		return err
	}
	records, err := s.store.Evidence(ctx, job.ID)
	if err != nil {
		return err
	}
	if err := spine.VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, s.blobs); err != nil {
		return s.setWorkflowAttention(ctx, job.ID, spine.ReviewPolicyAttentionSource(job.Revision), fmt.Errorf("Revision %s Evidence verification failed: %w", job.Revision, err))
	}
	facts, err := s.externals.RepositoryChangeFacts(ctx, job)
	if err != nil {
		return s.setWorkflowAttention(ctx, job.ID, spine.ReviewPolicyAttentionSource(job.Revision), fmt.Errorf("deterministic ChangeFacts failed: %w", err))
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		return s.setWorkflowAttention(ctx, job.ID, spine.ReviewPolicyAttentionSource(job.Revision), fmt.Errorf("mandatory ReviewPolicy rejected input: %w", err))
	}
	return s.recordReviewPolicy(ctx, spine.ReviewPlanRecord{JobID: job.ID, Revision: job.Revision, Facts: facts, Plan: plan})
}

func (s Service) recordReviewPolicy(ctx context.Context, record spine.ReviewPlanRecord) error {
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return s.store.RecordReviewPolicy(ctx, record)
}

func (s Service) RunReview(ctx context.Context, job spine.CodingJob, runID string) error {
	run, err := s.store.ReviewRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.JobID != job.ID || run.InputRevision != job.Revision {
		return fmt.Errorf("review AgentRun %s does not belong to the current Revision", runID)
	}
	if run.State == spine.AgentRunFailed || run.State == spine.AgentRunInterrupted {
		return s.setWorkflowAttention(ctx, job.ID, run.ID, fmt.Errorf("selected review Role %s is %s: %s", run.Role, run.State, run.Attention))
	}
	err = s.executeAndRecordReview(ctx, job, run)
	if err != nil && attentionNeeded(err) {
		return s.setWorkflowAttention(ctx, job.ID, run.ID, err)
	}
	return err
}

func (s Service) ExecuteReviewCheckout(ctx context.Context, job spine.CodingJob, runID string, action spine.Action) error {
	run, err := s.store.ReviewRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.JobID != job.ID || run.InputRevision != job.Revision || run.SandboxID != spine.ReviewSandboxName(run.ID) || run.Sandbox.ID != run.SandboxID ||
		action.ID != spine.ScopedActionID(job.ID, spine.ActionReviewCheckout, run.Sandbox.ID) || action.JobID != job.ID || action.Kind != spine.ActionReviewCheckout || action.Scope != run.Sandbox.ID {
		return reviewBoundaryError("review checkout Action does not belong to the exact selected AgentRun, Revision, and Sandbox")
	}
	if action.State == spine.ActionSucceeded {
		return nil
	}
	if err := s.externals.PrepareReviewCheckout(ctx, job, run); err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return s.store.RecordSandboxActionSuccess(ctx, action.ID)
}

func (s Service) executeAndRecordReview(ctx context.Context, job spine.CodingJob, run spine.ReviewRunView) error {
	outcome, err := s.executeReviewRun(ctx, job, run)
	if err != nil {
		return err
	}
	if outcome.Status != "completed" {
		return fmt.Errorf("review Role %s settled as %s", run.Role, outcome.Status)
	}
	checkout, err := s.verifyReviewCheckout(ctx, job, run)
	if err != nil {
		return err
	}
	run, err = s.store.ReviewRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(outcome.Output) == "" {
		return reviewBoundaryError(fmt.Sprintf("review Role %s returned no feedback text", run.Role))
	}
	observed, err := s.reviewEvidence(run, checkout)
	if err != nil {
		return err
	}
	_, _, err = s.recordReviewFeedback(ctx, run.ID, outcome, observed)
	return err
}

func (s Service) recordReviewFeedback(ctx context.Context, runID string, outcome spine.HarnessTurn, observed spine.Evidence) (spine.Message, bool, error) {
	if err := s.requireClaim(ctx); err != nil {
		return spine.Message{}, false, err
	}
	return s.store.RecordReviewFeedback(ctx, runID, outcome, observed)
}

func (s Service) verifyReviewCheckout(ctx context.Context, job spine.CodingJob, run spine.ReviewRunView) (spine.ReviewCheckoutObservation, error) {
	checkout, err := s.externals.VerifyReviewCheckout(ctx, job, run)
	if err != nil {
		reason := "review checkout verification failed: " + err.Error()
		_ = s.uncertainAgentRun(ctx, run.ID, reason)
		return spine.ReviewCheckoutObservation{}, reviewBoundaryError(reason)
	}
	if checkout.Revision != run.InputRevision || !fullGitObjectID(checkout.Tree) {
		reason := "review checkout is not the exact Revision with a full tree identity"
		_ = s.uncertainAgentRun(ctx, run.ID, reason)
		return spine.ReviewCheckoutObservation{}, reviewBoundaryError(reason)
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

func (s Service) executeReviewRun(ctx context.Context, job spine.CodingJob, original spine.ReviewRunView) (spine.HarnessTurn, error) {
	expectedFromID := spine.ReviewRequestFromID(original.InputRevision, original.Role)
	expectedMessageID := spine.ReviewRequestMessageID(job.ID, original.InputRevision, original.Role)
	if original.MessageID != expectedMessageID || original.Request.ID != expectedMessageID || original.Request.JobID != job.ID || original.Request.FromKind != spine.MessageFromWorkflow || original.Request.FromID != expectedFromID || original.Request.Intent != spine.MessageFollow || original.Request.TargetTurnID != "" || strings.TrimSpace(original.Request.Input) == "" || original.SandboxID != spine.ReviewSandboxName(original.ID) || original.Sandbox.ID != original.SandboxID || original.Sandbox.JobID != job.ID || len(original.Sandbox.OwnershipNonce) != 64 || len(original.SubmissionNonce) != 64 {
		reason := "review AgentRun request Message, Sandbox ownership, or exact submission contract is invalid"
		_ = s.uncertainAgentRun(ctx, original.ID, reason)
		return spine.HarnessTurn{}, reviewBoundaryError(reason)
	}
	if err := s.requireReviewActions(ctx, job, original); err != nil {
		return spine.HarnessTurn{}, err
	}
	run, err := s.store.ReviewRun(ctx, original.ID)
	if err != nil {
		return spine.HarnessTurn{}, err
	}
	if run.State == spine.AgentRunUncertain && (run.ThreadID == "") != (run.TurnID == "") {
		reason := "uncertain review AgentRun has a partial harness thread/turn binding"
		_ = s.uncertainAgentRun(ctx, run.ID, reason)
		return spine.HarnessTurn{}, reviewBoundaryError(reason)
	}
	expectedController := spine.ReviewControllerID(run.ID, run.Sandbox.ID, run.Sandbox.OwnershipNonce)
	request, sandbox := run.Request, run.Sandbox
	return spine.ExecuteAgentRun(ctx, spine.AgentRunExecution{
		Store: s.store, ReachBarrier: s.reach,
		Delivery: spine.Delivery{AgentRun: run.AgentRun}, Run: run.AgentRun,
		Harness: s.externals.Harness(), Label: "review",
		SubmitNew: func(ctx context.Context, agentRun spine.AgentRun) (spine.HarnessBinding, error) {
			return s.externals.ReviewInitialTurn(ctx, job, reviewRunAttempt(agentRun, request, sandbox))
		},
		Recover: func(ctx context.Context, agentRun spine.AgentRun) (spine.HarnessBinding, error) {
			return s.externals.ReviewRecover(ctx, job, reviewRunAttempt(agentRun, request, sandbox))
		},
		History: func(ctx context.Context, agentRun spine.AgentRun) (spine.HarnessHistory, error) {
			return s.externals.ReviewTurns(ctx, job, reviewRunAttempt(agentRun, request, sandbox))
		},
		Wait: func(ctx context.Context, agentRun spine.AgentRun, turnID string) (spine.HarnessBinding, error) {
			return s.externals.ReviewWait(ctx, job, reviewRunAttempt(agentRun, request, sandbox), turnID)
		},
		ValidateOwner: func(binding spine.HarnessBinding) error { return validateReviewController(expectedController, binding) },
		BeforeRecord:  s.requireClaim,
		OnReadError:   s.recordReviewReadError,
		OnRecoverError: func(ctx context.Context, agentRun spine.AgentRun, err error) error {
			var missing interface{ RetryableReviewVisibility() bool }
			if errors.As(err, &missing) && missing.RetryableReviewVisibility() {
				_ = s.agentRunAttention(ctx, agentRun.ID, err.Error())
			} else if attentionNeeded(err) {
				_ = s.failAgentRun(ctx, agentRun.ID, "strict review no-submit reconciliation conflict: "+err.Error())
			} else {
				_ = s.agentRunAttention(ctx, agentRun.ID, err.Error())
			}
			return err
		},
		OnSubmitError: func(ctx context.Context, agentRun spine.AgentRun, _ spine.HarnessBinding, err error) (spine.HarnessTurn, error) {
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				if failErr := s.failAgentRun(ctx, agentRun.ID, err.Error()); failErr != nil {
					return spine.HarnessTurn{}, failErr
				}
				return spine.HarnessTurn{Status: "failed"}, nil
			}
			if uncertainErr := s.uncertainAgentRun(ctx, agentRun.ID, err.Error()); uncertainErr != nil {
				return spine.HarnessTurn{}, uncertainErr
			}
			return spine.HarnessTurn{}, err
		},
	})
}

func (s Service) requireReviewActions(ctx context.Context, job spine.CodingJob, run spine.ReviewRunView) error {
	if run.SandboxID != spine.ReviewSandboxName(run.ID) || run.Sandbox.ID != run.SandboxID || run.Sandbox.JobID != job.ID {
		return reviewBoundaryError("review AgentRun has no exact dedicated reviewer Sandbox")
	}
	actions, err := s.store.Actions(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, kind := range []spine.ActionKind{spine.ActionSandboxCreate, spine.ActionReviewCheckout, spine.ActionRouteCreate} {
		expectedID := spine.ScopedActionID(job.ID, kind, run.Sandbox.ID)
		ready := false
		for _, action := range actions {
			if action.ID == expectedID && action.JobID == job.ID && action.Kind == kind && action.Scope == run.Sandbox.ID && action.State == spine.ActionSucceeded {
				ready = true
				break
			}
		}
		if !ready {
			return fmt.Errorf("review AgentRun %s requires succeeded %s Action %s", run.ID, kind, expectedID)
		}
	}
	return nil
}

func reviewRunAttempt(run spine.AgentRun, request spine.Message, sandbox spine.Sandbox) spine.ReviewRunView {
	return spine.ReviewRunView{AgentRun: run, Request: request, Sandbox: sandbox}
}

func validateReviewController(expected string, binding spine.HarnessBinding) error {
	if expected == "" || binding.ControllerID != expected {
		return reviewBoundaryError("review harness recovery returned a foreign controller")
	}
	return nil
}

func (s Service) recordReviewReadError(ctx context.Context, runID string, err error) {
	var missing interface{ RetryableReviewVisibility() bool }
	if errors.As(err, &missing) && missing.RetryableReviewVisibility() {
		_ = s.agentRunAttention(ctx, runID, err.Error())
	} else if attentionNeeded(err) {
		_ = s.uncertainAgentRun(ctx, runID, err.Error())
	}
}

func (s Service) reviewEvidence(run spine.ReviewRunView, checkout spine.ReviewCheckoutObservation) (spine.Evidence, error) {
	finished := run.FinishedAt.UTC().Truncate(time.Microsecond)
	started := run.StartedAt.UTC().Truncate(time.Microsecond)
	if started.IsZero() || finished.IsZero() || started.After(finished) {
		return spine.Evidence{}, fmt.Errorf("review AgentRun %s has no stable bounded timing", run.ID)
	}
	artifact, err := json.Marshal(reviewObservationArtifact{
		AgentRunID: run.ID, Revision: run.InputRevision, Role: run.Role, Capability: run.Capability,
		Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.TurnOutcome, Checkout: checkout,
	})
	if err != nil {
		return spine.Evidence{}, err
	}
	stored, err := s.blobs.Put(artifact)
	if err != nil {
		return spine.Evidence{}, err
	}
	return spine.Evidence{ID: spine.EvidenceID(run.ID, "review-observation"), Digest: stored.Digest, ByteSize: stored.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: reviewEvidenceProducer, Kind: "review-observation", AgentRunID: run.ID, Revision: run.InputRevision, StartedAt: started, FinishedAt: finished}, nil
}
