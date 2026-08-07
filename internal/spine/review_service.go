package spine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
)

const (
	BarrierReviewWorkspaceReady = "review-workspace-ready-before-record"
	BarrierReviewResultReady    = "review-result-ready-before-record"
	reviewEvidenceProducer      = "dorf-codex-review"
)

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
		if view.State == AgentRunFailed || view.State == AgentRunInterrupted || view.State == AgentRunUncertain {
			return s.blockReview(ctx, job.ID, fmt.Sprintf("selected review Role %s is %s: %s", view.Role, view.State, view.Attention))
		}
		pending = append(pending, view.AgentRun)
	}
	if len(pending) > 0 {
		// Git worktree registration mutates one shared repository registry, so it
		// is reconciled serially before any independently read-only native turns.
		for _, run := range pending {
			if err := s.ensureReviewWorkspace(ctx, job, run, store, externals); err != nil {
				return RunIdle, false, err
			}
		}
		if err := validateIndependentReviewBatch(pending); err != nil {
			// A future non-read-only capability is deliberately serialized.
			for _, run := range pending {
				if _, oneErr := s.executeAndRecordReview(ctx, job, run, store, externals); oneErr != nil {
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
	workspaces := map[string]bool{}
	for _, run := range runs {
		if run.Capability != ReviewReadOnlyCapability || run.Revision == "" || run.Workspace == "" || workspaces[run.Workspace] || run.SessionID != "" && run.State == AgentRunPending {
			return fmt.Errorf("review AgentRuns are not proven independent immutable read-only inputs")
		}
		workspaces[run.Workspace] = true
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

func (s Service) executeReviewRun(ctx context.Context, job Job, original AgentRun, externals ReviewExternals, store ReviewStore) (NativeTurn, error) {
	if err := s.ensureReviewWorkspace(ctx, job, original, store, externals); err != nil {
		return NativeTurn{}, err
	}
	run, err := store.ReviewRun(ctx, original.ID)
	if err != nil {
		return NativeTurn{}, err
	}
	if run.State == AgentRunCompleted {
		turns, err := externals.ReviewTurns(ctx, job, run)
		if err != nil {
			return NativeTurn{}, err
		}
		for _, turn := range turns {
			if turn.ID == run.NativeTurnID {
				return turn, nil
			}
		}
		return NativeTurn{}, fmt.Errorf("terminal review AgentRun %s native output is unavailable", run.ID)
	}
	if run.State == AgentRunFailed || run.State == AgentRunInterrupted || run.State == AgentRunUncertain {
		return NativeTurn{ID: run.NativeTurnID, Status: string(run.State)}, nil
	}
	session, err := store.BeginReviewSession(ctx, run.ID)
	if err != nil {
		return NativeTurn{}, err
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
		sessionID, turn, err := externals.ReviewInitialTurn(ctx, job, run)
		if err != nil {
			_ = s.Store.UncertainAction(ctx, session.ID)
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				if sessionID != "" {
					if bindErr := s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: sessionID}); bindErr != nil {
						return NativeTurn{}, bindErr
					}
				}
				if failErr := s.Store.FailAgentRun(ctx, run.ID, err.Error()); failErr != nil {
					return NativeTurn{}, failErr
				}
				return NativeTurn{Status: "failed"}, nil
			}
			if attentionNeeded(err) {
				if attentionErr := s.Store.UncertainAgentRun(ctx, run.ID, err.Error()); attentionErr != nil {
					return NativeTurn{}, attentionErr
				}
			}
			return NativeTurn{}, err
		}
		if sessionID == "" || turn.ID == "" {
			return NativeTurn{}, fmt.Errorf("review native submission returned incomplete Session or turn binding")
		}
		if err := s.reach(ctx, BarrierAfterSubmitBeforeBind, delivery); err != nil {
			return NativeTurn{}, err
		}
		if err := s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: sessionID}); err != nil {
			return NativeTurn{}, err
		}
		if err := s.Store.BindNativeTurn(ctx, run.ID, turn.ID, turn.Status); err != nil {
			return NativeTurn{}, err
		}
		run.SessionID, run.NativeTurnID = sessionID, turn.ID
		if terminalNative(turn.Status) {
			return turn, nil
		}
		outcome, err := externals.ReviewWait(ctx, job, run, turn.ID)
		if err != nil {
			_ = s.Store.AgentRunAttention(ctx, run.ID, "submitted review native turn outcome is currently unavailable: "+err.Error())
			return NativeTurn{}, err
		}
		if err := s.Store.BindNativeTurn(ctx, run.ID, outcome.ID, outcome.Status); err != nil {
			return NativeTurn{}, err
		}
		return outcome, nil
	}
	turns, err := externals.ReviewTurns(ctx, job, run)
	if err != nil {
		return NativeTurn{}, err
	}
	reconciliation := ReconcileTurns(run.BaselineRecorded, run.BaselineTurnID, run.NativeTurnID, turns)
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
		outcome, err := externals.ReviewWait(ctx, job, run, reconciliation.Turn.ID)
		if err != nil {
			_ = s.Store.AgentRunAttention(ctx, run.ID, "submitted review native turn outcome is currently unavailable: "+err.Error())
			return NativeTurn{}, err
		}
		if err := s.Store.BindNativeTurn(ctx, run.ID, outcome.ID, outcome.Status); err != nil {
			return NativeTurn{}, err
		}
		return outcome, nil
	}
	return reconciliation.Turn, nil
}

func (s Service) ensureReviewWorkspace(ctx context.Context, job Job, original AgentRun, store ReviewStore, externals ReviewExternals) error {
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
	artifact, err := json.Marshal(reviewObservationArtifact{run.ID, run.Revision, run.Role, run.Capability, run.Workspace, run.SessionID, outcome.ID, outcome.Status, outcome.InputTokens, outcome.CachedInputTokens, outcome.OutputTokens, outcome.CostMicrousd, outcome.UsageAvailable})
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
