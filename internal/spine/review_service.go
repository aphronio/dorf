package spine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
)

const (
	BarrierReviewWorkspaceReady = "review-workspace-ready-before-record"
	BarrierReviewResultReady    = "review-result-ready-before-record"
	reviewEvidenceProducer      = "dorf-codex-review"
)

type reviewBoundaryError string

func (e reviewBoundaryError) Error() string         { return string(e) }
func (e reviewBoundaryError) AttentionNeeded() bool { return true }

func (s Service) advanceReview(ctx context.Context, job Job) (RunDisposition, bool, error) {
	store, ok := s.Store.(ReviewStore)
	if !ok {
		return RunIdle, false, fmt.Errorf("review phase requires durable ReviewStore")
	}
	externals, ok := s.Externals.(ReviewExternals)
	if !ok {
		return RunIdle, false, fmt.Errorf("review phase requires Revision-isolated review externals")
	}
	switch job.WorkflowPhase {
	case "review-planning":
		activation, err := store.ReviewPlan(ctx, job.ID, job.Revision)
		if err != nil {
			return RunIdle, false, err
		}
		facts, err := externals.RepositoryChangeFacts(ctx, job)
		if err != nil {
			return s.blockReview(ctx, job.ID, "deterministic ChangeFacts failed: "+err.Error())
		}
		requested := activation.RequestedRoles
		if job.ReviewRepairCount == 1 {
			requested = nil
		}
		plan, err := policy.ReviewPolicy(facts, requested)
		if err != nil {
			return s.blockReview(ctx, job.ID, "mandatory ReviewPolicy rejected input: "+err.Error())
		}
		if job.ReviewRepairCount == 1 {
			targets, targetErr := store.ReviewRepairTargets(ctx, job.ID)
			if targetErr != nil {
				return s.blockReview(ctx, job.ID, "accepted finding has no exact re-verification target: "+targetErr.Error())
			}
			plan, err = policy.TargetedReverification(plan, targets)
			if err != nil {
				return s.blockReview(ctx, job.ID, err.Error())
			}
		}
		activation.Facts, activation.Initial, activation.Final = facts, plan, plan
		if err := store.RecordReviewPolicy(ctx, activation); err != nil {
			return RunIdle, false, err
		}
		return RunIdle, true, nil
	case "review-triage":
		plan, err := store.ReviewPlan(ctx, job.ID, job.Revision)
		if err != nil {
			return RunIdle, false, err
		}
		run, err := store.ReviewRun(ctx, plan.TriageRunID)
		if err != nil {
			return RunIdle, false, err
		}
		outcome, err := s.executeReviewRun(ctx, job, run, externals, store)
		if err != nil {
			if attentionNeeded(err) {
				return s.blockReview(ctx, job.ID, err.Error())
			}
			return RunIdle, false, err
		}
		if outcome.Status != "completed" {
			return s.blockReview(ctx, job.ID, fmt.Sprintf("review triage AgentRun settled as %s", outcome.Status))
		}
		if err := s.verifyReviewPostState(ctx, job, run, store, externals); err != nil {
			return s.blockReview(ctx, job.ID, err.Error())
		}
		run, err = store.ReviewRun(ctx, run.ID)
		if err != nil {
			return RunIdle, false, err
		}
		output, err := policy.ParseTriageOutput(outcome.Output)
		if err != nil {
			return s.blockReview(ctx, job.ID, err.Error())
		}
		final, err := policy.AddTriage(plan.Initial, output.Roles, output.Rationale)
		if err != nil {
			return s.blockReview(ctx, job.ID, err.Error())
		}
		claim, observed, err := s.reviewEvidence(run, outcome, "review-triage-rationale")
		if err != nil {
			return RunIdle, false, err
		}
		if err := s.reachWorkflow(ctx, BarrierReviewResultReady, job.ID, run.ID); err != nil {
			return RunIdle, false, err
		}
		if err := store.RecordTriageResult(ctx, run.ID, outcome, claim, observed, final, output.Rationale); err != nil {
			return RunIdle, false, err
		}
		return RunIdle, true, nil
	case "reviewing":
		return s.executeSelectedReviews(ctx, job, store, externals)
	default:
		return RunIdle, false, fmt.Errorf("unsupported review phase %q", job.WorkflowPhase)
	}
}

func (s Service) blockReview(ctx context.Context, jobID, reason string) (RunDisposition, bool, error) {
	if err := s.Store.(CodingStore).BlockWorkflow(ctx, jobID, reason); err != nil {
		return RunIdle, false, err
	}
	return RunBlocked, false, nil
}

func (s Service) executeSelectedReviews(ctx context.Context, job Job, store ReviewStore, externals ReviewExternals) (RunDisposition, bool, error) {
	plan, err := store.ReviewPlan(ctx, job.ID, job.Revision)
	if err != nil {
		return RunIdle, false, err
	}
	runs, err := store.ReviewRuns(ctx, job.ID, job.Revision)
	if err != nil {
		return RunIdle, false, err
	}
	selected := map[string]bool{}
	for _, role := range plan.Final.Roles {
		selected[string(role)] = true
	}
	var pending []AgentRun
	for _, view := range runs {
		if view.Role == ReviewTriageRole || !selected[view.Role] || view.Finding != nil {
			continue
		}
		if view.State == AgentRunFailed || view.State == AgentRunInterrupted {
			return s.blockReview(ctx, job.ID, fmt.Sprintf("selected review Role %s is %s: %s", view.Role, view.State, view.Attention))
		}
		pending = append(pending, view.AgentRun)
	}
	if len(pending) > 0 {
		// Reviewer Sandbox, immutable checkout, and scoped route reconciliation
		// are serialized before independently read-only native turns overlap.
		for i, run := range pending {
			if err := s.ensureReviewWorkspace(ctx, job, run, store, externals); err != nil {
				if attentionNeeded(err) {
					_ = s.Store.UncertainAgentRun(ctx, run.ID, err.Error())
					return s.blockReview(ctx, job.ID, err.Error())
				}
				return RunIdle, false, err
			}
			refreshed, err := store.ReviewRun(ctx, run.ID)
			if err != nil {
				return RunIdle, false, err
			}
			pending[i] = refreshed
		}
		if err := validateIndependentReviewBatch(pending); err != nil {
			// A future non-read-only capability is deliberately serialized.
			for _, run := range pending {
				if _, oneErr := s.executeAndRecordReview(ctx, job, run, store, externals); oneErr != nil {
					if attentionNeeded(oneErr) {
						return s.blockReview(ctx, job.ID, oneErr.Error())
					}
					return RunIdle, false, oneErr
				}
			}
		} else {
			var wg sync.WaitGroup
			errs := make(chan error, len(pending))
			for _, run := range pending {
				run := run
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := s.executeAndRecordReview(ctx, job, run, store, externals)
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					if attentionNeeded(err) {
						return s.blockReview(ctx, job.ID, err.Error())
					}
					return RunIdle, false, err
				}
			}
		}
		return RunIdle, true, nil
	}
	runs, err = store.ReviewRuns(ctx, job.ID, job.Revision)
	if err != nil {
		return RunIdle, false, err
	}
	var material []spineFindingRun
	for _, view := range runs {
		if selected[view.Role] && view.Finding != nil && view.Finding.Material && view.Finding.Adjudication != "rejected" {
			material = append(material, spineFindingRun{runID: view.ID, role: view.Role})
		}
	}
	sort.Slice(material, func(i, j int) bool { return material[i].role < material[j].role })
	if len(material) > 1 {
		return s.blockReview(ctx, job.ID, "multiple material review claims require human attention; the bounded automatic repair admits exactly one")
	}
	if len(material) == 1 {
		if job.ReviewRepairCount != 0 {
			return s.blockReview(ctx, job.ID, "targeted review still reports a material finding after the one bounded repair")
		}
		if _, _, err := store.AdmitReviewRepair(ctx, job.ID, material[0].runID); err != nil {
			return RunIdle, false, err
		}
		return RunIdle, true, nil
	}
	if err := store.MarkReviewReady(ctx, job.ID, job.Revision); err != nil {
		return RunIdle, false, err
	}
	return RunIdle, true, nil
}

type spineFindingRun struct{ runID, role string }

func validateIndependentReviewBatch(runs []AgentRun) error {
	sandboxes := map[string]bool{}
	routes := map[string]bool{}
	for _, run := range runs {
		if run.Capability != ReviewReadOnlyCapability || run.Revision == "" || run.Workspace == "" || run.ReviewerSandboxID == "" || run.ReviewerRouteID == "" || sandboxes[run.ReviewerSandboxID] || routes[run.ReviewerRouteID] || run.ReviewerSandboxState != "created" || run.ReviewerRouteState != "active" || run.CheckoutState != "verified" || run.SessionID != "" && run.State == AgentRunPending {
			return fmt.Errorf("review AgentRuns are not proven independent immutable read-only inputs")
		}
		sandboxes[run.ReviewerSandboxID] = true
		routes[run.ReviewerRouteID] = true
	}
	return nil
}

func (s Service) executeAndRecordReview(ctx context.Context, job Job, run AgentRun, store ReviewStore, externals ReviewExternals) (NativeTurn, error) {
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
	declared, err := s.Store.(CodingStore).DeclaredChecks(ctx, job.ID)
	if err != nil {
		return outcome, err
	}
	names := make([]string, 0, len(declared))
	for _, check := range declared {
		names = append(names, check.Name)
	}
	parsed, err := policy.ParseFindingOutput(outcome.Output, policy.Role(run.Role), names)
	if err != nil {
		_ = s.Store.(CodingStore).BlockWorkflow(ctx, job.ID, err.Error())
		return outcome, err
	}
	finding := ReviewFinding{RunID: run.ID, Revision: run.Revision, Role: policy.Role(run.Role), Material: parsed.Material, Summary: parsed.Summary, Rationale: parsed.Rationale, AffectedRoles: parsed.AffectedRoles, AffectedChecks: parsed.AffectedChecks}
	claim, observed, err := s.reviewEvidence(run, outcome, "review-finding")
	if err != nil {
		return outcome, err
	}
	if err := s.reachWorkflow(ctx, BarrierReviewResultReady, job.ID, run.ID); err != nil {
		return outcome, err
	}
	if err := store.RecordReviewResult(ctx, run.ID, outcome, claim, observed, finding); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func (s Service) verifyReviewPostState(ctx context.Context, job Job, run AgentRun, store ReviewStore, externals ReviewExternals) error {
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

func (s Service) executeReviewRun(ctx context.Context, job Job, original AgentRun, externals ReviewExternals, store ReviewStore) (NativeTurn, error) {
	if original.ReviewerSandboxID != "" {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(original.InputContract)))
		if original.ReviewerSandboxID != ReviewSandboxName(original.ID) || len(original.ReviewerOwnerNonce) != 64 || len(original.SubmissionNonce) != 64 || original.InputDigest != digest {
			reason := "review AgentRun durable Sandbox ownership or exact submission contract is invalid"
			_ = s.Store.UncertainAgentRun(ctx, original.ID, reason)
			return NativeTurn{}, reviewBoundaryError(reason)
		}
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
	if run.State == AgentRunCompleted {
		history, err := externals.ReviewTurns(ctx, job, run)
		if err != nil {
			s.recordReviewReadError(ctx, run.ID, err)
			return NativeTurn{}, err
		}
		if err := validateReviewNativeOwner(run, history.AppServerID, history.SessionID); err != nil {
			_ = s.Store.UncertainAgentRun(ctx, run.ID, err.Error())
			return NativeTurn{}, err
		}
		for _, turn := range history.Turns {
			if turn.ID == run.NativeTurnID {
				return turn, nil
			}
		}
		return NativeTurn{}, fmt.Errorf("terminal review AgentRun %s native output is unavailable", run.ID)
	}
	if run.State == AgentRunFailed || run.State == AgentRunInterrupted {
		return NativeTurn{ID: run.NativeTurnID, Status: string(run.State)}, nil
	}
	session, err := store.BeginReviewSession(ctx, run.ID)
	if err != nil {
		return NativeTurn{}, err
	}
	if run.State == AgentRunUncertain {
		switch {
		case run.SessionID == "" && run.NativeTurnID == "":
			if session.State != ActionUncertain || !strings.HasPrefix(session.Outcome, ReviewSubmissionUncertainOutcome+": ") {
				return NativeTurn{}, reviewBoundaryError("uncertain review AgentRun is not eligible for no-submit reconciliation: " + run.Attention)
			}
			return s.reconcileUnboundReview(ctx, job, run, session, externals)
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
	if run.SessionID == "" {
		if !run.BaselineRecorded {
			if err := s.Store.PrepareAgentRun(ctx, run.ID, ""); err != nil {
				return NativeTurn{}, err
			}
			run.BaselineRecorded, run.State = true, AgentRunSubmitting
		}
		delivery := Delivery{AgentRun: run}
		if err := s.reach(ctx, BarrierBeforeSubmit, delivery); err != nil {
			return NativeTurn{}, err
		}
		if err := s.Store.BeginTurnSubmission(ctx, run.ID); err != nil {
			return NativeTurn{}, err
		}
		binding, err := externals.ReviewInitialTurn(ctx, job, run)
		if err != nil {
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				_ = s.Store.UncertainAction(ctx, session.ID)
				if binding.SessionID != "" {
					if bindErr := s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: binding.SessionID, Outcome: binding.AppServerID}); bindErr != nil {
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
		}
		if binding.AppServerID == "" || binding.SessionID == "" || binding.Turn.ID == "" {
			return NativeTurn{}, fmt.Errorf("review native submission returned incomplete Session or turn binding")
		}
		if binding.AppServerID != ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce) {
			reason := "review native submission returned a foreign logical controller identity"
			_ = s.Store.UncertainAgentRun(ctx, run.ID, reason)
			return NativeTurn{}, reviewBoundaryError(reason)
		}
		if err := s.reach(ctx, BarrierAfterSubmitBeforeBind, delivery); err != nil {
			return NativeTurn{}, err
		}
		if err := s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: binding.SessionID, Outcome: binding.AppServerID}); err != nil {
			return NativeTurn{}, err
		}
		if err := s.Store.BindNativeTurn(ctx, run.ID, binding.Turn.ID, binding.Turn.Status); err != nil {
			return NativeTurn{}, err
		}
		run.SessionID, run.NativeTurnID, run.ReviewerAppServer = binding.SessionID, binding.Turn.ID, binding.AppServerID
		if terminalNative(binding.Turn.Status) {
			return binding.Turn, nil
		}
		waited, err := externals.ReviewWait(ctx, job, run, binding.Turn.ID)
		if err != nil {
			s.recordReviewReadError(ctx, run.ID, err)
			return NativeTurn{}, err
		}
		if err := validateReviewNativeOwner(run, waited.AppServerID, waited.SessionID); err != nil {
			_ = s.Store.UncertainAgentRun(ctx, run.ID, err.Error())
			return NativeTurn{}, err
		}
		if err := s.Store.BindNativeTurn(ctx, run.ID, waited.Turn.ID, waited.Turn.Status); err != nil {
			return NativeTurn{}, err
		}
		return waited.Turn, nil
	}
	history, err := externals.ReviewTurns(ctx, job, run)
	if err != nil {
		s.recordReviewReadError(ctx, run.ID, err)
		return NativeTurn{}, err
	}
	if err := validateReviewNativeOwner(run, history.AppServerID, history.SessionID); err != nil {
		_ = s.Store.UncertainAgentRun(ctx, run.ID, err.Error())
		return NativeTurn{}, err
	}
	reconciliation := ReconcileTurns(run.BaselineRecorded, run.BaselineTurnID, run.NativeTurnID, history.Turns)
	if reconciliation.Classification == "uncertain" {
		_ = s.Store.UncertainAgentRun(ctx, run.ID, reconciliation.Reason)
		return NativeTurn{}, fmt.Errorf("review native reconciliation is uncertain: %s", reconciliation.Reason)
	}
	if reconciliation.Classification == "no-submit" {
		return NativeTurn{}, fmt.Errorf("review Session exists but its durably prepared turn was not submitted")
	}
	if err := s.Store.BindNativeTurn(ctx, run.ID, reconciliation.Turn.ID, reconciliation.Turn.Status); err != nil {
		return NativeTurn{}, err
	}
	if reconciliation.Classification == "active" {
		waited, err := externals.ReviewWait(ctx, job, run, reconciliation.Turn.ID)
		if err != nil {
			s.recordReviewReadError(ctx, run.ID, err)
			return NativeTurn{}, err
		}
		if err := validateReviewNativeOwner(run, waited.AppServerID, waited.SessionID); err != nil {
			_ = s.Store.UncertainAgentRun(ctx, run.ID, err.Error())
			return NativeTurn{}, err
		}
		if err := s.Store.BindNativeTurn(ctx, run.ID, waited.Turn.ID, waited.Turn.Status); err != nil {
			return NativeTurn{}, err
		}
		return waited.Turn, nil
	}
	return reconciliation.Turn, nil
}

func (s Service) reconcileUnboundReview(ctx context.Context, job Job, run AgentRun, session Action, externals ReviewExternals) (NativeTurn, error) {
	binding, err := externals.ReviewRecover(ctx, job, run)
	if err != nil {
		var missing interface{ RetryableReviewVisibility() bool }
		if errors.As(err, &missing) && missing.RetryableReviewVisibility() {
			_ = s.Store.AgentRunAttention(ctx, run.ID, err.Error())
		} else if attentionNeeded(err) {
			_ = s.Store.FailAgentRun(ctx, run.ID, "strict review no-submit reconciliation conflict: "+err.Error())
		} else {
			_ = s.Store.AgentRunAttention(ctx, run.ID, err.Error())
		}
		return NativeTurn{}, err
	}
	if binding.AppServerID != ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce) || binding.SessionID == "" || binding.Turn.ID == "" {
		reason := "review reconciliation returned a foreign or incomplete controller, Session, or turn binding"
		_ = s.Store.UncertainAgentRun(ctx, run.ID, reason)
		return NativeTurn{}, reviewBoundaryError(reason)
	}
	if err := s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: binding.SessionID, Outcome: binding.AppServerID}); err != nil {
		return NativeTurn{}, err
	}
	if err := s.Store.BindNativeTurn(ctx, run.ID, binding.Turn.ID, binding.Turn.Status); err != nil {
		return NativeTurn{}, err
	}
	run.SessionID, run.NativeTurnID, run.ReviewerAppServer = binding.SessionID, binding.Turn.ID, binding.AppServerID
	if terminalNative(binding.Turn.Status) {
		return binding.Turn, nil
	}
	waited, err := externals.ReviewWait(ctx, job, run, binding.Turn.ID)
	if err != nil {
		s.recordReviewReadError(ctx, run.ID, err)
		return NativeTurn{}, err
	}
	if err := validateReviewNativeOwner(run, waited.AppServerID, waited.SessionID); err != nil {
		_ = s.Store.UncertainAgentRun(ctx, run.ID, err.Error())
		return NativeTurn{}, err
	}
	if err := s.Store.BindNativeTurn(ctx, run.ID, waited.Turn.ID, waited.Turn.Status); err != nil {
		return NativeTurn{}, err
	}
	return waited.Turn, nil
}

func (s Service) recordReviewReadError(ctx context.Context, runID string, err error) {
	var missing interface{ RetryableReviewVisibility() bool }
	if errors.As(err, &missing) && missing.RetryableReviewVisibility() {
		_ = s.Store.AgentRunAttention(ctx, runID, err.Error())
	} else if attentionNeeded(err) {
		_ = s.Store.UncertainAgentRun(ctx, runID, err.Error())
	}
}

func (s Service) ensureReviewWorkspace(ctx context.Context, job Job, original AgentRun, store ReviewStore, externals ReviewExternals) error {
	if original.ReviewerSandboxID != "" {
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
	if original.ReviewerSandboxID != "" {
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
	}
	return nil
}

func validateReviewNativeOwner(run AgentRun, appServerID, sessionID string) error {
	expectedController := ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce)
	if appServerID == "" || appServerID != expectedController || sessionID == "" || run.ReviewerAppServer != appServerID || run.SessionID != sessionID {
		return reviewBoundaryError("review native recovery conflicts with the exact reviewer app-server or Session")
	}
	return nil
}

func (s Service) reviewEvidence(run AgentRun, outcome NativeTurn, claimKind string) (Evidence, Evidence, error) {
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
	claim := Evidence{ID: EvidenceID(run.ID, claimKind), Digest: claimBlob.Digest, ByteSize: claimBlob.ByteSize, MediaType: "application/vnd.dorf.agent-claim+json", Producer: reviewEvidenceProducer, Provenance: "claim", Kind: claimKind, ActionID: run.ActionID, Revision: run.Revision, StartedAt: started, FinishedAt: finished}
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
