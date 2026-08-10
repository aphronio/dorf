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
	BarrierReviewCheckoutReady = "review-checkout-ready-before-record"
	BarrierReviewFeedbackReady = "review-feedback-ready-before-record"
	reviewEvidenceProducer     = "dorf-agent-review"
)

type reviewBoundaryError string

func (e reviewBoundaryError) Error() string         { return string(e) }
func (e reviewBoundaryError) AttentionNeeded() bool { return true }

func (s Service) PlanReview(ctx context.Context, job Job) error {
	store, ok := s.Store.(ReviewStore)
	if !ok {
		return fmt.Errorf("review phase requires durable ReviewStore")
	}
	externals, ok := s.Externals.(ReviewExternals)
	if !ok {
		return fmt.Errorf("review phase requires Revision-isolated review externals")
	}
	if job.WorkflowPhase != "review-planning" {
		return fmt.Errorf("cannot plan review from phase %q", job.WorkflowPhase)
	}
	facts, err := externals.RepositoryChangeFacts(ctx, job)
	if err != nil {
		return s.Store.(CodingStore).BlockWorkflow(ctx, job.ID, "deterministic ChangeFacts failed: "+err.Error())
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		return s.Store.(CodingStore).BlockWorkflow(ctx, job.ID, "mandatory ReviewPolicy rejected input: "+err.Error())
	}
	return store.RecordReviewPolicy(ctx, ReviewPlanRecord{JobID: job.ID, Revision: job.Revision, Facts: facts, Plan: plan})
}

func (s Service) RunReview(ctx context.Context, job Job, runID string) error {
	store, ok := s.Store.(ReviewStore)
	if !ok {
		return fmt.Errorf("review phase requires durable ReviewStore")
	}
	externals, ok := s.Externals.(ReviewExternals)
	if !ok {
		return fmt.Errorf("review phase requires Revision-isolated review externals")
	}
	run, err := store.ReviewRun(ctx, runID)
	if err != nil {
		return err
	}
	if job.WorkflowPhase != "reviewing" || run.JobID != job.ID || run.Revision != job.Revision {
		return fmt.Errorf("review AgentRun %s does not belong to the current reviewing Revision", runID)
	}
	if run.State == AgentRunFailed || run.State == AgentRunInterrupted {
		return s.Store.(CodingStore).BlockWorkflow(ctx, job.ID, fmt.Sprintf("selected review Role %s is %s: %s", run.Role, run.State, run.Attention))
	}
	_, err = s.executeAndRecordReview(ctx, job, run, store, externals)
	if err != nil && attentionNeeded(err) {
		return s.Store.(CodingStore).BlockWorkflow(ctx, job.ID, err.Error())
	}
	return err
}

func (s Service) executeAndRecordReview(ctx context.Context, job Job, run ReviewRunView, store ReviewStore, externals ReviewExternals) (HarnessTurn, error) {
	outcome, err := s.executeReviewRun(ctx, job, run, externals, store)
	if err != nil {
		return HarnessTurn{}, err
	}
	if outcome.Status != "completed" {
		return outcome, fmt.Errorf("review Role %s settled as %s", run.Role, outcome.Status)
	}
	checkout, err := s.verifyReviewCheckout(ctx, job, run, externals)
	if err != nil {
		return outcome, err
	}
	run, err = store.ReviewRun(ctx, run.ID)
	if err != nil {
		return outcome, err
	}
	if strings.TrimSpace(outcome.Output) == "" {
		return outcome, reviewBoundaryError(fmt.Sprintf("review Role %s returned no feedback text", run.Role))
	}
	observed, err := s.reviewEvidence(run, checkout)
	if err != nil {
		return outcome, err
	}
	if err := s.reachWorkflow(ctx, BarrierReviewFeedbackReady, job.ID, run.ID); err != nil {
		return outcome, err
	}
	if _, _, err := store.RecordReviewFeedback(ctx, run.ID, outcome, observed); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func (s Service) verifyReviewCheckout(ctx context.Context, job Job, run ReviewRunView, externals ReviewExternals) (ReviewCheckoutObservation, error) {
	checkout, err := externals.VerifyReviewCheckout(ctx, job, run)
	if err != nil {
		reason := "review checkout verification failed: " + err.Error()
		_ = s.Store.UncertainAgentRun(ctx, run.ID, reason)
		return ReviewCheckoutObservation{}, reviewBoundaryError(reason)
	}
	if checkout.Revision != run.Revision || !fullGitObjectID(checkout.Tree) {
		reason := "review checkout is not the exact Revision with a full tree identity"
		_ = s.Store.UncertainAgentRun(ctx, run.ID, reason)
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

func (s Service) executeReviewRun(ctx context.Context, job Job, original ReviewRunView, externals ReviewExternals, store ReviewStore) (HarnessTurn, error) {
	expectedFromID := ReviewRequestFromID(original.Revision, original.Role)
	expectedMessageID := ReviewRequestMessageID(job.ID, original.Revision, original.Role)
	if original.MessageID != expectedMessageID || original.Request.ID != expectedMessageID || original.Request.JobID != job.ID || original.Request.FromKind != MessageFromWorkflow || original.Request.FromID != expectedFromID || original.Request.Intent != MessageFollow || original.Request.TargetTurnID != "" || strings.TrimSpace(original.Request.Input) == "" || original.SandboxID != ReviewSandboxName(original.ID) || original.Sandbox.ID != original.SandboxID || original.Sandbox.JobID != job.ID || len(original.Sandbox.OwnershipNonce) != 64 || len(original.SubmissionNonce) != 64 {
		reason := "review AgentRun request Message, Sandbox ownership, or exact submission contract is invalid"
		_ = s.Store.UncertainAgentRun(ctx, original.ID, reason)
		return HarnessTurn{}, reviewBoundaryError(reason)
	}
	if err := s.ensureReviewCheckout(ctx, job, original, store, externals); err != nil {
		if attentionNeeded(err) {
			_ = s.Store.UncertainAgentRun(ctx, original.ID, err.Error())
		}
		return HarnessTurn{}, err
	}
	run, err := store.ReviewRun(ctx, original.ID)
	if err != nil {
		return HarnessTurn{}, err
	}
	if run.State == AgentRunUncertain && (run.ThreadID == "") != (run.TurnID == "") {
		reason := "uncertain review AgentRun has a partial harness thread/turn binding"
		_ = s.Store.UncertainAgentRun(ctx, run.ID, reason)
		return HarnessTurn{}, reviewBoundaryError(reason)
	}
	expectedController := ReviewControllerID(run.ID, run.Sandbox.ID, run.Sandbox.OwnershipNonce)
	request := run.Request
	sandbox, route := run.Sandbox, run.Route
	contract := agentRunContract{
		service:             s,
		delivery:            Delivery{AgentRun: run.AgentRun},
		run:                 run.AgentRun,
		harness:             s.Externals.Harness(),
		label:               "review",
		bindUnsupportedTurn: false,
		submitNew: func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
			return externals.ReviewInitialTurn(ctx, job, reviewRunAttempt(run, request, sandbox, route))
		},
		recover: func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
			return externals.ReviewRecover(ctx, job, reviewRunAttempt(run, request, sandbox, route))
		},
		history: func(ctx context.Context, run AgentRun) (HarnessHistory, error) {
			return externals.ReviewTurns(ctx, job, reviewRunAttempt(run, request, sandbox, route))
		},
		wait: func(ctx context.Context, run AgentRun, turnID string) (HarnessBinding, error) {
			return externals.ReviewWait(ctx, job, reviewRunAttempt(run, request, sandbox, route), turnID)
		},
		validateOwner: func(binding HarnessBinding) error {
			return validateReviewController(expectedController, binding)
		},
		beforeBind:  s.requireClaim,
		onReadError: s.recordReviewReadError,
		onRecoverError: func(ctx context.Context, run AgentRun, err error) error {
			var missing interface{ RetryableReviewVisibility() bool }
			if errors.As(err, &missing) && missing.RetryableReviewVisibility() {
				_ = s.Store.AgentRunAttention(ctx, run.ID, err.Error())
			} else if attentionNeeded(err) {
				_ = s.Store.FailAgentRun(ctx, run.ID, "strict review no-submit reconciliation conflict: "+err.Error())
			} else {
				_ = s.Store.AgentRunAttention(ctx, run.ID, err.Error())
			}
			return err
		},
		onSubmitError: func(ctx context.Context, run AgentRun, _ HarnessBinding, err error) (HarnessTurn, error) {
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				if failErr := s.Store.FailAgentRun(ctx, run.ID, err.Error()); failErr != nil {
					return HarnessTurn{}, failErr
				}
				return HarnessTurn{Status: "failed"}, nil
			}
			if uncertainErr := s.Store.UncertainAgentRun(ctx, run.ID, err.Error()); uncertainErr != nil {
				return HarnessTurn{}, uncertainErr
			}
			return HarnessTurn{}, err
		},
	}
	return contract.execute(ctx)
}

func reviewRunAttempt(run AgentRun, request Message, sandbox Sandbox, route Route) ReviewRunView {
	return ReviewRunView{AgentRun: run, Request: request, Sandbox: sandbox, Route: route}
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
		_ = s.Store.AgentRunAttention(ctx, runID, err.Error())
	} else if attentionNeeded(err) {
		_ = s.Store.UncertainAgentRun(ctx, runID, err.Error())
	}
}

func (s Service) ensureReviewCheckout(ctx context.Context, job Job, original ReviewRunView, store ReviewStore, externals ReviewExternals) error {
	if original.SandboxID != ReviewSandboxName(original.ID) || original.Sandbox.ID != original.SandboxID {
		return reviewBoundaryError("review AgentRun has no exact dedicated reviewer Sandbox")
	}
	sandbox, err := s.Store.GetOrCreateResourceAction(ctx, original.Sandbox.ID, ActionSandboxCreate)
	if err != nil {
		return err
	}
	if sandbox.State != ActionSucceeded {
		receipt, err := s.Externals.SandboxCreate(ctx, job, original.Sandbox, sandbox)
		if err != nil {
			_ = s.Store.UncertainAction(ctx, sandbox.ID)
			return err
		}
		if err := s.Store.CompleteAction(ctx, sandbox.ID, receipt); err != nil {
			return err
		}
	}
	checkout, err := s.Store.GetOrCreateResourceAction(ctx, original.Sandbox.ID, ActionReviewCheckout)
	if err != nil {
		return err
	}
	if checkout.State != ActionSucceeded {
		receipt, err := externals.PrepareReviewCheckout(ctx, job, original, checkout)
		if err != nil {
			_ = s.Store.UncertainAction(ctx, checkout.ID)
			return err
		}
		if err := s.reachWorkflow(ctx, BarrierReviewCheckoutReady, job.ID, original.ID); err != nil {
			return err
		}
		if err := s.Store.CompleteAction(ctx, checkout.ID, receipt); err != nil {
			return err
		}
	}
	route, err := s.Store.GetOrCreateResourceAction(ctx, original.Sandbox.ID, ActionRouteCreate)
	if err != nil {
		return err
	}
	if route.State != ActionSucceeded {
		persistedRoute, loadErr := s.Store.Route(ctx, original.Sandbox.ID)
		if loadErr != nil {
			return loadErr
		}
		receipt, err := s.Externals.RouteCreate(ctx, job, original.Sandbox, persistedRoute, route)
		if err != nil {
			_ = s.Store.UncertainAction(ctx, route.ID)
			return err
		}
		if err := s.Store.CompleteAction(ctx, route.ID, receipt); err != nil {
			return err
		}
	}
	return nil
}

func (s Service) reviewEvidence(run ReviewRunView, checkout ReviewCheckoutObservation) (Evidence, error) {
	finished := run.FinishedAt.UTC().Truncate(time.Microsecond)
	started := run.StartedAt.UTC().Truncate(time.Microsecond)
	if started.IsZero() || finished.IsZero() || started.After(finished) {
		return Evidence{}, fmt.Errorf("review AgentRun %s has no stable bounded timing", run.ID)
	}
	artifact, err := json.Marshal(reviewObservationArtifact{
		AgentRunID: run.ID, Revision: run.Revision, Role: run.Role, Capability: run.Capability,
		Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.TurnOutcome, Checkout: checkout,
	})
	if err != nil {
		return Evidence{}, err
	}
	blob, err := s.Evidence.Put(artifact)
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{ID: EvidenceID(run.ID, "review-observation"), Digest: blob.Digest, ByteSize: blob.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: reviewEvidenceProducer, Kind: "review-observation", AgentRunID: run.ID, Revision: run.Revision, StartedAt: started, FinishedAt: finished}, nil
}
