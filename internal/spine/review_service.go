package spine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
)

const (
	BarrierReviewWorkspaceReady = "review-workspace-ready-before-record"
	BarrierReviewFeedbackReady  = "review-feedback-ready-before-record"
	reviewEvidenceProducer      = "dorf-codex-review"
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
	record, err := store.ReviewPlan(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	facts, err := externals.RepositoryChangeFacts(ctx, job)
	if err != nil {
		return s.Store.(CodingStore).BlockWorkflow(ctx, job.ID, "deterministic ChangeFacts failed: "+err.Error())
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		return s.Store.(CodingStore).BlockWorkflow(ctx, job.ID, "mandatory ReviewPolicy rejected input: "+err.Error())
	}
	record.Facts, record.Plan = facts, plan
	return store.RecordReviewPolicy(ctx, record)
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

func (s Service) executeAndRecordReview(ctx context.Context, job Job, run ReviewRunView, store ReviewStore, externals ReviewExternals) (NativeTurn, error) {
	outcome, err := s.executeReviewRun(ctx, job, run, externals, store)
	if err != nil {
		return NativeTurn{}, err
	}
	if outcome.Status != "completed" {
		return outcome, fmt.Errorf("review Role %s settled as %s", run.Role, outcome.Status)
	}
	if err := s.verifyReviewPostState(ctx, job, run, store, externals); err != nil {
		return outcome, err
	}
	run, err = store.ReviewRun(ctx, run.ID)
	if err != nil {
		return outcome, err
	}
	if strings.TrimSpace(outcome.Output) == "" {
		return outcome, reviewBoundaryError(fmt.Sprintf("review Role %s returned no feedback text", run.Role))
	}
	claim, observed, err := s.reviewEvidence(run, outcome, "review-feedback")
	if err != nil {
		return outcome, err
	}
	if err := s.reachWorkflow(ctx, BarrierReviewFeedbackReady, job.ID, run.ID); err != nil {
		return outcome, err
	}
	if _, _, err := store.RecordReviewFeedback(ctx, run.ID, outcome, claim, observed); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func (s Service) verifyReviewPostState(ctx context.Context, job Job, run ReviewRunView, store ReviewStore, externals ReviewExternals) error {
	post, err := externals.ReviewWorkspaceVerify(ctx, job, run)
	if err != nil {
		reason := "reviewer checkout post-turn attestation failed: " + err.Error()
		_ = s.Store.UncertainAgentRun(ctx, run.ID, reason)
		return reviewBoundaryError(reason)
	}
	if err := store.RecordReviewPostState(ctx, run.ID, post); err != nil {
		reason := "reviewer checkout post-turn state conflicts with durable input: " + err.Error()
		_ = s.Store.UncertainAgentRun(ctx, run.ID, reason)
		return reviewBoundaryError(reason)
	}
	return nil
}

func (s Service) executeReviewRun(ctx context.Context, job Job, original ReviewRunView, externals ReviewExternals, store ReviewStore) (NativeTurn, error) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(original.InputContract)))
	if original.ReviewerSandboxID != ReviewSandboxName(original.ID) || len(original.ReviewerOwnerNonce) != 64 || len(original.SubmissionNonce) != 64 || original.InputDigest != digest {
		reason := "review AgentRun durable Sandbox ownership or exact submission contract is invalid"
		_ = s.Store.UncertainAgentRun(ctx, original.ID, reason)
		return NativeTurn{}, reviewBoundaryError(reason)
	}
	if err := s.ensureReviewWorkspace(ctx, job, original, store, externals); err != nil {
		if attentionNeeded(err) {
			_ = s.Store.UncertainAgentRun(ctx, original.ID, err.Error())
		}
		return NativeTurn{}, err
	}
	run, err := store.ReviewRun(ctx, original.ID)
	if err != nil {
		return NativeTurn{}, err
	}
	var session Action
	if run.State != AgentRunCompleted && run.State != AgentRunFailed && run.State != AgentRunInterrupted {
		session, err = store.BeginReviewSession(ctx, run.ID)
		if err != nil {
			return NativeTurn{}, err
		}
	}
	if run.State == AgentRunUncertain {
		switch {
		case run.SessionID == "" && run.NativeTurnID == "":
			if session.State != ActionUncertain || !strings.HasPrefix(session.Outcome, ReviewSubmissionUncertainOutcome+": ") {
				return NativeTurn{}, reviewBoundaryError("uncertain review AgentRun is not eligible for no-submit reconciliation: " + run.Attention)
			}
		case run.SessionID == "" || run.NativeTurnID == "":
			reason := "uncertain review AgentRun has a partial native Session/turn binding"
			_ = s.Store.UncertainAgentRun(ctx, run.ID, reason)
			return NativeTurn{}, reviewBoundaryError(reason)
		case session.State != ActionSucceeded:
			reason := "uncertain bound review AgentRun does not have a succeeded Session Action"
			_ = s.Store.UncertainAgentRun(ctx, run.ID, reason)
			return NativeTurn{}, reviewBoundaryError(reason)
		}
	}
	expectedController := ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce)
	contract := nativeAgentRunContract{
		service:             s,
		delivery:            Delivery{AgentRun: run.AgentRun},
		run:                 run.AgentRun,
		label:               "review",
		bindUnsupportedTurn: false,
		submitNew: func(ctx context.Context, run AgentRun) (nativeAgentBinding, error) {
			binding, err := externals.ReviewInitialTurn(ctx, job, ReviewRunView{AgentRun: run, ReviewRunProjection: original.ReviewRunProjection})
			return nativeAgentBinding{OwnerID: binding.AppServerID, SessionID: binding.SessionID, Turn: binding.Turn}, err
		},
		recover: func(ctx context.Context, run AgentRun) (nativeAgentBinding, error) {
			binding, err := externals.ReviewRecover(ctx, job, ReviewRunView{AgentRun: run, ReviewRunProjection: original.ReviewRunProjection})
			return nativeAgentBinding{OwnerID: binding.AppServerID, SessionID: binding.SessionID, Turn: binding.Turn}, err
		},
		history: func(ctx context.Context, run AgentRun) (nativeAgentHistory, error) {
			history, err := externals.ReviewTurns(ctx, job, ReviewRunView{AgentRun: run, ReviewRunProjection: original.ReviewRunProjection})
			return nativeAgentHistory{OwnerID: history.AppServerID, SessionID: history.SessionID, Turns: history.Turns}, err
		},
		wait: func(ctx context.Context, run AgentRun, turnID string) (nativeAgentBinding, error) {
			binding, err := externals.ReviewWait(ctx, job, ReviewRunView{AgentRun: run, ReviewRunProjection: original.ReviewRunProjection}, turnID)
			return nativeAgentBinding{OwnerID: binding.AppServerID, SessionID: binding.SessionID, Turn: binding.Turn}, err
		},
		validateOwner: func(run AgentRun, appServerID, sessionID string) error {
			if run.SessionID == "" {
				if appServerID == "" || appServerID != expectedController || sessionID == "" {
					return reviewBoundaryError("review native recovery returned a foreign or incomplete controller, Session, or turn binding")
				}
				return nil
			}
			return validateReviewNativeOwner(ReviewRunView{AgentRun: run, ReviewRunProjection: original.ReviewRunProjection}, appServerID, sessionID)
		},
		bindSession: func(ctx context.Context, binding nativeAgentBinding) error {
			return s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: binding.SessionID, Outcome: binding.OwnerID})
		},
		adoptBinding: func(_ *AgentRun, _ nativeAgentBinding) {},
		onReadError:  s.recordReviewReadError,
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
		onSubmitError: func(ctx context.Context, run AgentRun, binding nativeAgentBinding, err error) (NativeTurn, error) {
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				_ = s.Store.UncertainAction(ctx, session.ID)
				if binding.SessionID != "" {
					if bindErr := s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: binding.SessionID, Outcome: binding.OwnerID}); bindErr != nil {
						return NativeTurn{}, bindErr
					}
				}
				if failErr := s.Store.FailAgentRun(ctx, run.ID, err.Error()); failErr != nil {
					return NativeTurn{}, failErr
				}
				return NativeTurn{Status: "failed"}, nil
			}
			if attentionNeeded(err) {
				_ = s.Store.UncertainAction(ctx, session.ID)
				if attentionErr := s.Store.UncertainAgentRun(ctx, run.ID, err.Error()); attentionErr != nil {
					return NativeTurn{}, attentionErr
				}
			} else if uncertainErr := store.UncertainReviewSubmission(ctx, run.ID, session.ID, err.Error()); uncertainErr != nil {
				return NativeTurn{}, uncertainErr
			}
			return NativeTurn{}, err
		},
	}
	return contract.execute(ctx)
}

func (s Service) recordReviewReadError(ctx context.Context, runID string, err error) {
	var missing interface{ RetryableReviewVisibility() bool }
	if errors.As(err, &missing) && missing.RetryableReviewVisibility() {
		_ = s.Store.AgentRunAttention(ctx, runID, err.Error())
	} else if attentionNeeded(err) {
		_ = s.Store.UncertainAgentRun(ctx, runID, err.Error())
	}
}

func (s Service) ensureReviewWorkspace(ctx context.Context, job Job, original ReviewRunView, store ReviewStore, externals ReviewExternals) error {
	if original.ReviewerSandboxID != ReviewSandboxName(original.ID) {
		return reviewBoundaryError("review AgentRun has no exact dedicated reviewer Sandbox")
	}
	sandbox, err := store.BeginReviewSandbox(ctx, original.ID)
	if err != nil {
		return err
	}
	if sandbox.State != ActionSucceeded {
		receipt, err := externals.ReviewSandboxCreate(ctx, job, original, sandbox)
		if err != nil {
			_ = s.Store.UncertainAction(ctx, sandbox.ID)
			return err
		}
		if err := s.Store.CompleteAction(ctx, sandbox.ID, receipt); err != nil {
			return err
		}
	}
	workspace, err := store.BeginReviewWorkspace(ctx, original.ID)
	if err != nil {
		return err
	}
	if workspace.State != ActionSucceeded {
		receipt, err := externals.ReviewWorkspaceCreate(ctx, job, original, workspace)
		if err != nil {
			_ = s.Store.UncertainAction(ctx, workspace.ID)
			return err
		}
		if err := s.reachWorkflow(ctx, BarrierReviewWorkspaceReady, job.ID, original.ID); err != nil {
			return err
		}
		if err := s.Store.CompleteAction(ctx, workspace.ID, receipt); err != nil {
			return err
		}
	}
	route, err := store.BeginReviewRoute(ctx, original.ID)
	if err != nil {
		return err
	}
	if route.State != ActionSucceeded {
		receipt, err := externals.ReviewRouteCreate(ctx, job, original, route)
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

func validateReviewNativeOwner(run ReviewRunView, appServerID, sessionID string) error {
	expectedController := ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce)
	if appServerID == "" || appServerID != expectedController || sessionID == "" || run.ReviewerAppServer != appServerID || run.SessionID != sessionID {
		return reviewBoundaryError("review native recovery conflicts with the exact reviewer app-server or Session")
	}
	return nil
}

func (s Service) reviewEvidence(run ReviewRunView, outcome NativeTurn, claimKind string) (Evidence, Evidence, error) {
	if outcome.InputTokens < 0 || outcome.CachedInputTokens < 0 || outcome.OutputTokens < 0 || outcome.CostMicrousd < 0 {
		return Evidence{}, Evidence{}, fmt.Errorf("review AgentRun %s returned invalid negative usage facts", run.ID)
	}
	finished := run.FinishedAt.UTC().Truncate(time.Microsecond)
	started := run.StartedAt.UTC().Truncate(time.Microsecond)
	if started.IsZero() || finished.IsZero() || started.After(finished) {
		return Evidence{}, Evidence{}, fmt.Errorf("review AgentRun %s has no stable bounded timing", run.ID)
	}
	claimBlob, err := s.Evidence.Put([]byte(outcome.Output))
	if err != nil {
		return Evidence{}, Evidence{}, err
	}
	claim := Evidence{ID: EvidenceID(run.ID, claimKind), Digest: claimBlob.Digest, ByteSize: claimBlob.ByteSize, MediaType: "text/plain; charset=utf-8", Producer: reviewEvidenceProducer, Provenance: "claim", Kind: claimKind, ActionID: run.ActionID, Revision: run.Revision, StartedAt: started, FinishedAt: finished}
	artifact, err := json.Marshal(reviewObservationArtifact{
		AgentRunID: run.ID, Revision: run.Revision, Role: run.Role, Capability: run.Capability, Workspace: run.Workspace,
		SessionID: run.SessionID, NativeTurnID: outcome.ID, NativeOutcome: outcome.Status, InputTokens: outcome.InputTokens,
		CachedInputTokens: outcome.CachedInputTokens, OutputTokens: outcome.OutputTokens, CostMicrousd: outcome.CostMicrousd,
		UsageAvailable: outcome.UsageAvailable, ReviewerSandboxID: run.ReviewerSandboxID, ReviewerRouteID: run.ReviewerRouteID,
		ReviewerAppServer: run.ReviewerAppServer, InputDigest: run.InputDigest, RevisionTree: run.RevisionTree,
	})
	if err != nil {
		return Evidence{}, Evidence{}, err
	}
	observedBlob, err := s.Evidence.Put(artifact)
	if err != nil {
		return Evidence{}, Evidence{}, err
	}
	observed := Evidence{ID: EvidenceID(run.ID, "review-native-observation"), Digest: observedBlob.Digest, ByteSize: observedBlob.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: reviewEvidenceProducer, Provenance: "observed", Kind: "review-native-observation", ActionID: run.ActionID, Revision: run.Revision, StartedAt: started, FinishedAt: finished}
	return claim, observed, nil
}
