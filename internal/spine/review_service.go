package spine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
)

const (
	reviewEvidenceProducer = "dorf-agent-review"
)

type reviewBoundaryError string

func (e reviewBoundaryError) Error() string         { return string(e) }
func (e reviewBoundaryError) AttentionNeeded() bool { return true }

func (s Service) PlanReview(ctx context.Context, job Job) error {
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
	if err := VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, s.evidence); err != nil {
		return s.setWorkflowAttention(ctx, job.ID, ReviewPolicyAttentionSource(job.Revision), fmt.Errorf("Revision %s Evidence verification failed: %w", job.Revision, err))
	}
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

func (s Service) RunReview(ctx context.Context, job Job, runID string) error {
	run, err := s.store.ReviewRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.JobID != job.ID || run.InputRevision != job.Revision {
		return fmt.Errorf("review AgentRun %s does not belong to the current Revision", runID)
	}
	if run.State == AgentRunFailed || run.State == AgentRunInterrupted {
		return s.setWorkflowAttention(ctx, job.ID, run.ID, fmt.Errorf("selected review Role %s is %s: %s", run.Role, run.State, run.Attention))
	}
	err = s.executeAndRecordReview(ctx, job, run)
	if err != nil && attentionNeeded(err) {
		return s.setWorkflowAttention(ctx, job.ID, run.ID, err)
	}
	return err
}

// ExecuteReviewCheckout prepares one selected reviewer's exact immutable
// checkout. The workflow owns the surrounding Action Step; this operation
// only reconciles and records that Action's external effect.
func (s Service) ExecuteReviewCheckout(ctx context.Context, job Job, runID string, action Action) error {
	run, err := s.store.ReviewRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.JobID != job.ID || run.InputRevision != job.Revision || run.SandboxID != ReviewSandboxName(run.ID) || run.Sandbox.ID != run.SandboxID ||
		action.ID != ScopedActionID(job.ID, ActionReviewCheckout, run.Sandbox.ID) || action.JobID != job.ID || action.Kind != ActionReviewCheckout || action.Scope != run.Sandbox.ID {
		return reviewBoundaryError("review checkout Action does not belong to the exact selected AgentRun, Revision, and Sandbox")
	}
	if action.State == ActionSucceeded {
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

func (s Service) executeAndRecordReview(ctx context.Context, job Job, run ReviewRunView) error {
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
	if _, _, err := s.recordReviewFeedback(ctx, run.ID, outcome, observed); err != nil {
		return err
	}
	return nil
}

func (s Service) recordReviewFeedback(ctx context.Context, runID string, outcome HarnessTurn, observed Evidence) (Message, bool, error) {
	if err := s.requireClaim(ctx); err != nil {
		return Message{}, false, err
	}
	return s.store.RecordReviewFeedback(ctx, runID, outcome, observed)
}

func (s Service) verifyReviewCheckout(ctx context.Context, job Job, run ReviewRunView) (ReviewCheckoutObservation, error) {
	checkout, err := s.externals.VerifyReviewCheckout(ctx, job, run)
	if err != nil {
		reason := "review checkout verification failed: " + err.Error()
		_ = s.uncertainAgentRun(ctx, run.ID, reason)
		return ReviewCheckoutObservation{}, reviewBoundaryError(reason)
	}
	if checkout.Revision != run.InputRevision || !fullGitObjectID(checkout.Tree) {
		reason := "review checkout is not the exact Revision with a full tree identity"
		_ = s.uncertainAgentRun(ctx, run.ID, reason)
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

func (s Service) executeReviewRun(ctx context.Context, job Job, original ReviewRunView) (HarnessTurn, error) {
	expectedFromID := ReviewRequestFromID(original.InputRevision, original.Role)
	expectedMessageID := ReviewRequestMessageID(job.ID, original.InputRevision, original.Role)
	if original.MessageID != expectedMessageID || original.Request.ID != expectedMessageID || original.Request.JobID != job.ID || original.Request.FromKind != MessageFromWorkflow || original.Request.FromID != expectedFromID || original.Request.Intent != MessageFollow || original.Request.TargetTurnID != "" || strings.TrimSpace(original.Request.Input) == "" || original.SandboxID != ReviewSandboxName(original.ID) || original.Sandbox.ID != original.SandboxID || original.Sandbox.JobID != job.ID || len(original.Sandbox.OwnershipNonce) != 64 || len(original.SubmissionNonce) != 64 {
		reason := "review AgentRun request Message, Sandbox ownership, or exact submission contract is invalid"
		_ = s.uncertainAgentRun(ctx, original.ID, reason)
		return HarnessTurn{}, reviewBoundaryError(reason)
	}
	if err := s.requireReviewActions(ctx, job, original); err != nil {
		return HarnessTurn{}, err
	}
	run, err := s.store.ReviewRun(ctx, original.ID)
	if err != nil {
		return HarnessTurn{}, err
	}
	if run.State == AgentRunUncertain && (run.ThreadID == "") != (run.TurnID == "") {
		reason := "uncertain review AgentRun has a partial harness thread/turn binding"
		_ = s.uncertainAgentRun(ctx, run.ID, reason)
		return HarnessTurn{}, reviewBoundaryError(reason)
	}
	expectedController := ReviewControllerID(run.ID, run.Sandbox.ID, run.Sandbox.OwnershipNonce)
	request := run.Request
	sandbox := run.Sandbox
	contract := agentRunContract{
		store:        s.store,
		reachBarrier: s.reach,
		delivery:     Delivery{AgentRun: run.AgentRun},
		run:          run.AgentRun,
		harness:      s.externals.Harness(),
		label:        "review",
		submitNew: func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
			return s.externals.ReviewInitialTurn(ctx, job, reviewRunAttempt(run, request, sandbox))
		},
		recover: func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
			return s.externals.ReviewRecover(ctx, job, reviewRunAttempt(run, request, sandbox))
		},
		history: func(ctx context.Context, run AgentRun) (HarnessHistory, error) {
			return s.externals.ReviewTurns(ctx, job, reviewRunAttempt(run, request, sandbox))
		},
		wait: func(ctx context.Context, run AgentRun, turnID string) (HarnessBinding, error) {
			return s.externals.ReviewWait(ctx, job, reviewRunAttempt(run, request, sandbox), turnID)
		},
		validateOwner: func(binding HarnessBinding) error {
			return validateReviewController(expectedController, binding)
		},
		beforeRecord: s.requireClaim,
		onReadError:  s.recordReviewReadError,
		onRecoverError: func(ctx context.Context, run AgentRun, err error) error {
			var missing interface{ RetryableReviewVisibility() bool }
			if errors.As(err, &missing) && missing.RetryableReviewVisibility() {
				_ = s.agentRunAttention(ctx, run.ID, err.Error())
			} else if attentionNeeded(err) {
				_ = s.failAgentRun(ctx, run.ID, "strict review no-submit reconciliation conflict: "+err.Error())
			} else {
				_ = s.agentRunAttention(ctx, run.ID, err.Error())
			}
			return err
		},
		onSubmitError: func(ctx context.Context, run AgentRun, _ HarnessBinding, err error) (HarnessTurn, error) {
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				if failErr := s.failAgentRun(ctx, run.ID, err.Error()); failErr != nil {
					return HarnessTurn{}, failErr
				}
				return HarnessTurn{Status: "failed"}, nil
			}
			if uncertainErr := s.uncertainAgentRun(ctx, run.ID, err.Error()); uncertainErr != nil {
				return HarnessTurn{}, uncertainErr
			}
			return HarnessTurn{}, err
		},
	}
	return contract.execute(ctx)
}

func (s Service) requireReviewActions(ctx context.Context, job Job, run ReviewRunView) error {
	if run.SandboxID != ReviewSandboxName(run.ID) || run.Sandbox.ID != run.SandboxID || run.Sandbox.JobID != job.ID {
		return reviewBoundaryError("review AgentRun has no exact dedicated reviewer Sandbox")
	}
	actions, err := s.store.Actions(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, kind := range []ActionKind{ActionSandboxCreate, ActionReviewCheckout, ActionRouteCreate} {
		expectedID := ScopedActionID(job.ID, kind, run.Sandbox.ID)
		ready := false
		for _, action := range actions {
			if action.ID == expectedID && action.JobID == job.ID && action.Kind == kind && action.Scope == run.Sandbox.ID && action.State == ActionSucceeded {
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

func reviewRunAttempt(run AgentRun, request Message, sandbox Sandbox) ReviewRunView {
	return ReviewRunView{AgentRun: run, Request: request, Sandbox: sandbox}
}

func validateReviewController(expected string, binding HarnessBinding) error {
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

func (s Service) reviewEvidence(run ReviewRunView, checkout ReviewCheckoutObservation) (Evidence, error) {
	finished := run.FinishedAt.UTC().Truncate(time.Microsecond)
	started := run.StartedAt.UTC().Truncate(time.Microsecond)
	if started.IsZero() || finished.IsZero() || started.After(finished) {
		return Evidence{}, fmt.Errorf("review AgentRun %s has no stable bounded timing", run.ID)
	}
	artifact, err := json.Marshal(reviewObservationArtifact{
		AgentRunID: run.ID, Revision: run.InputRevision, Role: run.Role, Capability: run.Capability,
		Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.TurnOutcome, Checkout: checkout,
	})
	if err != nil {
		return Evidence{}, err
	}
	blob, err := s.evidence.Put(artifact)
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{ID: EvidenceID(run.ID, "review-observation"), Digest: blob.Digest, ByteSize: blob.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: reviewEvidenceProducer, Kind: "review-observation", AgentRunID: run.ID, Revision: run.InputRevision, StartedAt: started, FinishedAt: finished}, nil
}
