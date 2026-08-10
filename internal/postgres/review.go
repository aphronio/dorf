package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/postgres/dbsql"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

func (s Store) MarkChecksVerified(ctx context.Context, jobID, revision string, verifiedEvidenceIDs []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	job, err := queries.GetReviewJobForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Revision != revision || job.WorkflowPhase != "checking" {
		return fmt.Errorf("Revision %s Checks cannot be finalized from phase %s at Revision %s", revision, job.WorkflowPhase, job.Revision)
	}
	if err := verifyEvidenceSet(ctx, queries, jobID, revision, verifiedEvidenceIDs); err != nil {
		return err
	}
	if err := ensureInputsTerminalForWorkflowTx(ctx, tx, jobID); err != nil {
		return fmt.Errorf("automatic review planning blocked: %w", err)
	}
	if err := queries.InsertReviewPlan(ctx, dbsql.InsertReviewPlanParams{JobID: jobID, Revision: revision}); err != nil {
		return err
	}
	if err := expectOneRows(queries.AdvanceJobToReviewPlanning(ctx, dbsql.AdvanceJobToReviewPlanningParams{JobID: jobID, Revision: revision})); err != nil {
		return err
	}
	return tx.Commit()
}

func verifyEvidenceSet(ctx context.Context, queries *dbsql.Queries, jobID, revision string, verifiedEvidenceIDs []string) error {
	ids, err := queries.ListVerifiedReviewEvidenceIDs(ctx, dbsql.ListVerifiedReviewEvidenceIDsParams{JobID: jobID, Revision: revision})
	if err != nil {
		return err
	}
	proving := make([]string, 0, len(ids))
	for _, id := range ids {
		if !id.Valid || id.String == "" {
			return fmt.Errorf("Revision %s has a passing Check without Evidence", revision)
		}
		proving = append(proving, id.String)
	}
	declared, err := queries.CountDeclaredReviewChecks(ctx, jobID)
	if err != nil {
		return err
	}
	sort.Strings(proving)
	verified := append([]string(nil), verifiedEvidenceIDs...)
	sort.Strings(verified)
	if declared == 0 || int64(len(proving)) != declared || !slices.Equal(proving, verified) {
		return fmt.Errorf("Revision %s is not review-admissible: verified Evidence does not exactly match %d declared Checks", revision, declared)
	}
	return nil
}

func (s Store) ReviewPlan(ctx context.Context, jobID, revision string) (spine.ReviewPlanRecord, error) {
	row, err := dbsql.New(s.DB).GetReviewPlan(ctx, dbsql.GetReviewPlanParams{JobID: jobID, Revision: revision})
	if err != nil {
		return spine.ReviewPlanRecord{}, err
	}
	return reviewPlanRecord(row.JobID, row.Revision, row.State, row.Facts, row.Plan, row.PolicyDigest, row.CreatedAt, row.FinalizedAt)
}

func (s Store) ReviewPlans(ctx context.Context, jobID string) ([]spine.ReviewPlanRecord, error) {
	rows, err := dbsql.New(s.DB).ListReviewPlans(ctx, jobID)
	if err != nil {
		return nil, err
	}
	plans := make([]spine.ReviewPlanRecord, 0, len(rows))
	for _, row := range rows {
		plan, err := reviewPlanRecord(row.JobID, row.Revision, row.State, row.Facts, row.Plan, row.PolicyDigest, row.CreatedAt, row.FinalizedAt)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func reviewPlanTx(ctx context.Context, queries *dbsql.Queries, jobID, revision string) (spine.ReviewPlanRecord, error) {
	row, err := queries.GetReviewPlanForUpdate(ctx, dbsql.GetReviewPlanForUpdateParams{JobID: jobID, Revision: revision})
	if err != nil {
		return spine.ReviewPlanRecord{}, err
	}
	return reviewPlanRecord(row.JobID, row.Revision, row.State, row.Facts, row.Plan, row.PolicyDigest, row.CreatedAt, row.FinalizedAt)
}

func reviewPlanRecord(jobID, revision, state, facts, plan, digest string, createdAt time.Time, finalized sql.NullTime) (spine.ReviewPlanRecord, error) {
	record := spine.ReviewPlanRecord{JobID: jobID, Revision: revision, State: state, PolicyDigest: digest, CreatedAt: createdAt}
	if finalized.Valid {
		record.FinalizedAt = finalized.Time
	}
	if facts != "{}" {
		if err := json.Unmarshal([]byte(facts), &record.Facts); err != nil {
			return record, err
		}
	}
	if plan != "{}" {
		if err := json.Unmarshal([]byte(plan), &record.Plan); err != nil {
			return record, err
		}
	}
	return record, nil
}

func policyDigest(facts policy.ChangeFacts, plan policy.ReviewPlan) (string, error) {
	contents, err := json.Marshal(struct {
		Facts policy.ChangeFacts `json:"facts"`
		Plan  policy.ReviewPlan  `json:"plan"`
	}{facts, plan})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}

func (s Store) RecordReviewPolicy(ctx context.Context, proposed spine.ReviewPlanRecord) error {
	digest, err := policyDigest(proposed.Facts, proposed.Plan)
	if err != nil {
		return err
	}
	if proposed.Facts.Revision != proposed.Revision {
		return fmt.Errorf("review policy does not match its immutable Revision or decision")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	stored, err := reviewPlanTx(ctx, queries, proposed.JobID, proposed.Revision)
	if err != nil {
		return err
	}
	if stored.PolicyDigest != "" {
		if stored.PolicyDigest != digest {
			return fmt.Errorf("mandatory review policy result changed across retry")
		}
		return tx.Commit()
	}
	if stored.State != "pending" {
		return fmt.Errorf("review policy conflicts with its durable per-Revision state")
	}
	factsJSON, _ := json.Marshal(proposed.Facts)
	planJSON, _ := json.Marshal(proposed.Plan)
	phase := "reviewing"
	if proposed.Plan.Decision == "no-review" {
		phase = "ready"
	}
	if err := queries.FinalizeReviewPlan(ctx, dbsql.FinalizeReviewPlanParams{
		Facts: factsJSON, Plan: planJSON, PolicyDigest: digest, JobID: proposed.JobID, Revision: proposed.Revision,
	}); err != nil {
		return err
	}
	for _, role := range proposed.Plan.Roles {
		if _, err := createReviewRunTx(ctx, queries, proposed.JobID, proposed.Revision, string(role), proposed.Facts); err != nil {
			return err
		}
	}
	if err := expectOneRows(queries.AdvanceReviewPolicyPhase(ctx, dbsql.AdvanceReviewPolicyPhaseParams{
		WorkflowPhase: phase, JobID: proposed.JobID, Revision: proposed.Revision,
	})); err != nil {
		return err
	}
	return tx.Commit()
}

func createReviewRunTx(ctx context.Context, queries *dbsql.Queries, jobID, revision, role string, facts policy.ChangeFacts) (string, error) {
	runID := spine.ReviewAgentRunID(jobID, revision, role)
	workspace := "/workspace/job"
	declaredChecks, err := queries.ListDeclaredReviewCheckNames(ctx, jobID)
	if err != nil {
		return "", err
	}
	input := policy.RolePrompt(policy.Role(role), facts, declaredChecks)
	turnAction := spine.ScopedActionID(jobID, spine.ActionTurnStart, runID)
	sessionAction := spine.ScopedActionID(jobID, spine.ActionSessionStart, runID)
	sandboxCreateAction := spine.ScopedActionID(jobID, spine.ActionSandboxCreate, runID)
	routeCreateAction := spine.ScopedActionID(jobID, spine.ActionRouteCreate, runID)
	createAction := spine.ScopedActionID(jobID, spine.ActionReviewWorkspaceCreate, runID)
	routeRevokeAction := spine.ScopedActionID(jobID, spine.ActionRouteRevoke, runID)
	sandboxDeleteAction := spine.ScopedActionID(jobID, spine.ActionSandboxDelete, runID)
	for _, action := range []struct {
		id   string
		kind spine.ActionKind
	}{{turnAction, spine.ActionTurnStart}, {sessionAction, spine.ActionSessionStart}, {sandboxCreateAction, spine.ActionSandboxCreate}, {routeCreateAction, spine.ActionRouteCreate}, {createAction, spine.ActionReviewWorkspaceCreate}, {routeRevokeAction, spine.ActionRouteRevoke}, {sandboxDeleteAction, spine.ActionSandboxDelete}} {
		if err := queries.InsertReviewAction(ctx, dbsql.InsertReviewActionParams{
			ID: action.id, JobID: jobID, Kind: action.kind, ScopeKey: runID,
		}); err != nil {
			return "", err
		}
	}
	if err := queries.InsertReviewAgentRun(ctx, dbsql.InsertReviewAgentRunParams{
		ID: runID, JobID: jobID, ActionID: turnAction, Role: role, Revision: revision,
		Capability: spine.ReviewReadOnlyCapability, Workspace: workspace, InputContract: input,
	}); err != nil {
		return "", err
	}
	ownerNonce, err := reviewNonce()
	if err != nil {
		return "", err
	}
	submissionNonce, err := reviewNonce()
	if err != nil {
		return "", err
	}
	inputSum := sha256.Sum256([]byte(input))
	if err := queries.InsertReviewResource(ctx, dbsql.InsertReviewResourceParams{
		RunID: runID, JobID: jobID, Revision: revision, SandboxName: spine.ReviewSandboxName(runID),
		OwnershipNonce: ownerNonce, SubmissionNonce: submissionNonce, InputDigest: hex.EncodeToString(inputSum[:]),
		SandboxCreateActionID: sandboxCreateAction, RouteCreateActionID: routeCreateAction,
		MaterializeActionID: createAction, RouteRevokeActionID: routeRevokeAction, SandboxDeleteActionID: sandboxDeleteAction,
	}); err != nil {
		return "", err
	}
	return runID, nil
}

func reviewNonce() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate review ownership nonce: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func (s Store) ReviewRun(ctx context.Context, runID string) (spine.ReviewRunView, error) {
	row, err := dbsql.New(s.DB).GetReviewRun(ctx, runID)
	if err != nil {
		return spine.ReviewRunView{}, err
	}
	return reviewRunView(row), nil
}

func reviewRunView(row dbsql.DorfReviewRunProjection) spine.ReviewRunView {
	view := spine.ReviewRunView{
		AgentRun: spine.AgentRun{
			ID: row.ID, JobID: row.JobID, MessageID: row.MessageID, ActionID: row.ActionID, SessionID: row.SessionID,
			State: spine.AgentRunState(row.State), BaselineRecorded: row.BaselineRecorded, BaselineTurnID: row.BaselineTurnID,
			NativeTurnID: row.NativeTurnID, NativeOutcome: row.NativeOutcome, Attention: row.Attention, Role: row.Role,
			Revision: row.Revision, Capability: row.Capability, Workspace: row.Workspace, InputContract: row.InputContract,
			InputTokens: row.InputTokens, CachedInputTokens: row.CachedInputTokens, OutputTokens: row.OutputTokens,
			CostMicrousd: row.CostMicrousd, UsageAvailable: row.UsageAvailable, YieldCount: int(row.YieldCount),
		},
		ReviewRunProjection: spine.ReviewRunProjection{
			ClaimEvidenceID: row.ClaimEvidenceID, ObservedEvidenceID: row.ObservedEvidenceID,
			ReviewerSandboxID: row.ReviewerSandboxID, ReviewerRouteID: row.ReviewerRouteID,
			ReviewerAppServer: row.ReviewerAppServer, ReviewerOwnerNonce: row.ReviewerOwnerNonce,
			SubmissionNonce: row.SubmissionNonce, InputDigest: row.InputDigest, RevisionTree: row.RevisionTree,
			ReviewerSandboxState: row.ReviewerSandboxState, ReviewerRouteState: row.ReviewerRouteState,
			CheckoutState: row.CheckoutState, PostReviewState: row.PostReviewState,
		},
	}
	if row.StartedAt.Valid {
		view.StartedAt = row.StartedAt.Time
	}
	if row.FinishedAt.Valid {
		view.FinishedAt = row.FinishedAt.Time
	}
	return view
}

func (s Store) ReviewRuns(ctx context.Context, jobID, revision string) ([]spine.ReviewRunView, error) {
	queries := dbsql.New(s.DB)
	if _, err := queries.GetReviewCurrentRevision(ctx, jobID); err != nil {
		return nil, err
	}
	rows, err := queries.ListReviewRuns(ctx, dbsql.ListReviewRunsParams{JobID: jobID, Revision: revision})
	if err != nil {
		return nil, err
	}
	views := make([]spine.ReviewRunView, 0, len(rows))
	for _, row := range rows {
		view := reviewRunView(row.DorfReviewRunProjection)
		view.FeedbackMessageID = row.FeedbackMessageID
		view.Stale = row.Stale
		views = append(views, view)
	}
	return views, nil
}

func (s Store) AllReviewRuns(ctx context.Context, jobID string) ([]spine.ReviewRunView, error) {
	rows, err := dbsql.New(s.DB).ListAllReviewRuns(ctx, jobID)
	if err != nil {
		return nil, err
	}
	result := make([]spine.ReviewRunView, 0, len(rows))
	for _, row := range rows {
		view := reviewRunView(row.DorfReviewRunProjection)
		view.FeedbackMessageID = row.FeedbackMessageID
		view.Stale = row.Stale
		result = append(result, view)
	}
	return result, nil
}

// CleanupReviewRuns returns only AgentRuns backed by persisted reviewer
// resources, including partial rows that cleanup must validate or reject.
func (s Store) CleanupReviewRuns(ctx context.Context, jobID string) ([]spine.ReviewRunView, error) {
	rows, err := dbsql.New(s.DB).ListCleanupReviewRuns(ctx, jobID)
	if err != nil {
		return nil, err
	}
	result := make([]spine.ReviewRunView, 0, len(rows))
	for _, row := range rows {
		result = append(result, reviewRunView(row))
	}
	return result, nil
}

func (s Store) beginReviewAction(ctx context.Context, runID string, kind spine.ActionKind) (spine.Action, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Action{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	switch kind {
	case spine.ActionSandboxCreate, spine.ActionRouteCreate, spine.ActionReviewWorkspaceCreate,
		spine.ActionSessionStart, spine.ActionRouteRevoke, spine.ActionSandboxDelete:
	default:
		return spine.Action{}, fmt.Errorf("unsupported review Action %q", kind)
	}
	actionID, err := queries.GetReviewActionID(ctx, dbsql.GetReviewActionIDParams{Kind: string(kind), RunID: runID})
	if err != nil {
		return spine.Action{}, err
	}
	row, err := queries.GetReviewActionForUpdate(ctx, actionID)
	if err != nil {
		return spine.Action{}, err
	}
	action := spine.Action{ID: row.ID, JobID: row.JobID, MessageID: row.MessageID, Kind: row.Kind, State: row.State, ExternalID: row.ExternalID, Outcome: row.ExternalOutcome, Scope: row.ScopeKey}
	if err = tx.Commit(); err != nil {
		return spine.Action{}, err
	}
	return action, nil
}

func (s Store) BeginReviewSandbox(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionSandboxCreate)
}
func (s Store) BeginReviewRoute(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionRouteCreate)
}
func (s Store) BeginReviewWorkspace(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionReviewWorkspaceCreate)
}
func (s Store) BeginReviewSession(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionSessionStart)
}

func (s Store) UncertainReviewSubmission(ctx context.Context, runID, sessionActionID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("review submission uncertainty requires a reason")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	turnActionID, err := queries.GetReviewTurnActionForUpdate(ctx, dbsql.GetReviewTurnActionForUpdateParams{RunID: runID, Capability: spine.ReviewReadOnlyCapability})
	if err != nil {
		return err
	}
	outcome := spine.ReviewSubmissionUncertainOutcome + ": " + reason
	if err := expectOneRows(queries.MarkReviewSessionActionUncertain(ctx, dbsql.MarkReviewSessionActionUncertainParams{Outcome: outcome, ActionID: sessionActionID, RunID: runID})); err != nil {
		return err
	}
	if err := expectOneRows(queries.MarkReviewTurnActionUncertain(ctx, dbsql.MarkReviewTurnActionUncertainParams{Outcome: outcome, ActionID: turnActionID})); err != nil {
		return err
	}
	if err := expectOneRows(queries.MarkReviewRunUncertain(ctx, dbsql.MarkReviewRunUncertainParams{Reason: reason, RunID: runID})); err != nil {
		return err
	}
	return tx.Commit()
}
func (s Store) BeginReviewRouteCleanup(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionRouteRevoke)
}
func (s Store) BeginReviewSandboxCleanup(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionSandboxDelete)
}

func (s Store) InterruptReviewRun(ctx context.Context, runID, reason string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	state, err := queries.GetReviewRunStateForUpdate(ctx, dbsql.GetReviewRunStateForUpdateParams{RunID: runID, Capability: spine.ReviewReadOnlyCapability})
	if err != nil {
		return err
	}
	if state == spine.AgentRunCompleted || state == spine.AgentRunFailed || state == spine.AgentRunInterrupted {
		return tx.Commit()
	}
	actionID, err := queries.InterruptReviewAgentRun(ctx, dbsql.InterruptReviewAgentRunParams{Reason: reason, RunID: runID})
	if err != nil {
		return err
	}
	if err := queries.FailInterruptedReviewAction(ctx, dbsql.FailInterruptedReviewActionParams{Reason: reason, ActionID: actionID}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) RecordReviewPostState(ctx context.Context, runID string, receipt spine.Receipt) error {
	revision, tree, err := parseReviewStateOutcome(receipt.Outcome)
	if err != nil {
		return err
	}
	return expectOneRows(dbsql.New(s.DB).VerifyReviewPostState(ctx, dbsql.VerifyReviewPostStateParams{
		RunID: runID, Revision: revision, RevisionTree: tree, Workspace: receipt.ExternalID,
	}))
}

func parseReviewStateOutcome(outcome string) (string, string, error) {
	parts := strings.Fields(outcome)
	if len(parts) != 3 || parts[2] != "clean" || !ValidRevision(parts[0]) || !ValidRevision(parts[1]) {
		return "", "", fmt.Errorf("review checkout observation is not exact Revision/tree/clean state")
	}
	return parts[0], parts[1], nil
}

// RecordReviewFeedback retains one reviewer's exact prose and feeds it back to
// the original implementation Session as an ordinary Message. The Job row is
// locked before allocating the Message sequence so concurrent reviewer
// completions remain deterministic and idempotent.
func (s Store) RecordReviewFeedback(ctx context.Context, runID string, outcome spine.NativeTurn, claim, observed spine.Evidence) (spine.Message, bool, error) {
	if outcome.Status != "completed" || strings.TrimSpace(outcome.Output) == "" {
		return spine.Message{}, false, fmt.Errorf("review feedback requires exact nonblank completed reviewer output")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Message{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	run, err := queries.GetReviewFeedbackRunForUpdate(ctx, runID)
	if err != nil {
		return spine.Message{}, false, err
	}
	if !policy.Allowed(policy.Role(run.Role)) || run.Capability != spine.ReviewReadOnlyCapability || run.State != spine.AgentRunCompleted ||
		run.NativeTurnID == "" || outcome.ID != run.NativeTurnID || run.Revision != run.CurrentRevision || claim.Revision != run.Revision || observed.Revision != run.Revision ||
		run.WorkflowPhase != "reviewing" && run.WorkflowPhase != "review-feedback" && run.WorkflowPhase != "ready" {
		return spine.Message{}, false, fmt.Errorf("review feedback conflicts with its completed exact-Revision reviewer AgentRun")
	}
	boundaryReady, err := queries.ReviewBoundaryReady(ctx, runID)
	if err != nil {
		return spine.Message{}, false, err
	}
	if !boundaryReady {
		return spine.Message{}, false, fmt.Errorf("review feedback lacks an attested isolated reviewer boundary")
	}
	if err := insertEvidence(ctx, tx, run.JobID, claim); err != nil {
		return spine.Message{}, false, err
	}
	if err := insertEvidence(ctx, tx, run.JobID, observed); err != nil {
		return spine.Message{}, false, err
	}
	storedEvidence, err := queries.GetReviewEvidenceIDs(ctx, runID)
	if err != nil {
		return spine.Message{}, false, err
	}
	if storedEvidence.ClaimEvidenceID != "" && storedEvidence.ClaimEvidenceID != claim.ID ||
		storedEvidence.ObservedEvidenceID != "" && storedEvidence.ObservedEvidenceID != observed.ID {
		return spine.Message{}, false, fmt.Errorf("review feedback retry conflicts with retained Evidence")
	}
	if err := queries.UpdateReviewEvidenceAndUsage(ctx, dbsql.UpdateReviewEvidenceAndUsageParams{
		ClaimEvidenceID: claim.ID, ObservedEvidenceID: observed.ID, InputTokens: outcome.InputTokens,
		CachedInputTokens: outcome.CachedInputTokens, OutputTokens: outcome.OutputTokens,
		CostMicrousd: outcome.CostMicrousd, UsageAvailable: outcome.UsageAvailable, RunID: runID,
	}); err != nil {
		return spine.Message{}, false, err
	}

	message := spine.Message{ID: spine.MessageID(run.JobID, spine.MessageFromAgent, runID), JobID: run.JobID, FromKind: spine.MessageFromAgent, FromID: runID, Input: outcome.Output, Intent: spine.MessageFollow}
	created := false
	storedMessage, err := queries.GetReviewFeedbackMessage(ctx, dbsql.GetReviewFeedbackMessageParams{JobID: run.JobID, RunID: runID})
	if err == nil {
		message = spine.Message{ID: storedMessage.ID, JobID: storedMessage.JobID, FromKind: storedMessage.FromKind,
			FromID: storedMessage.FromID, Sequence: storedMessage.Sequence, Input: storedMessage.Input,
			Intent: storedMessage.DeliveryIntent, TargetTurnID: storedMessage.SteerTargetTurnID}
		if message.Input != outcome.Output || message.Intent != spine.MessageFollow {
			return spine.Message{}, false, fmt.Errorf("reviewer AgentRun %s is already bound to different exact feedback", runID)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return spine.Message{}, false, err
	} else {
		message.Sequence, err = allocateMessageSequenceTx(ctx, tx, run.JobID)
		if err != nil {
			return spine.Message{}, false, err
		}
		if err := queries.InsertReviewFeedbackMessage(ctx, dbsql.InsertReviewFeedbackMessageParams{
			ID: message.ID, JobID: message.JobID, FromKind: message.FromKind, FromID: message.FromID,
			Sequence: message.Sequence, Input: message.Input,
		}); err != nil {
			return spine.Message{}, false, err
		}
		actionID, implementationRunID := spine.TurnActionID(message.ID), spine.AgentRunID(message.ID)
		if err := queries.InsertReviewFeedbackAction(ctx, dbsql.InsertReviewFeedbackActionParams{
			ID: actionID, JobID: run.JobID, MessageID: message.ID, Kind: spine.ActionTurnStart,
		}); err != nil {
			return spine.Message{}, false, err
		}
		if err := expectOneRows(queries.InsertReviewFeedbackAgentRun(ctx, dbsql.InsertReviewFeedbackAgentRunParams{
			ID: implementationRunID, JobID: run.JobID, MessageID: message.ID, ActionID: actionID,
		})); err != nil {
			return spine.Message{}, false, err
		}
		created = true
	}
	missing, err := queries.CountMissingReviewFeedback(ctx, dbsql.CountMissingReviewFeedbackParams{JobID: run.JobID, Revision: run.Revision})
	if err != nil {
		return spine.Message{}, false, err
	}
	if missing == 0 && run.WorkflowPhase == "reviewing" {
		if err := expectOneRows(queries.AdvanceJobToReviewFeedback(ctx, dbsql.AdvanceJobToReviewFeedbackParams{JobID: run.JobID, Revision: run.Revision})); err != nil {
			return spine.Message{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return spine.Message{}, false, err
	}
	return message, created, nil
}

// CompleteReviewFeedback accepts an unchanged clean checkout only while the
// named implementation AgentRun is still the latest accepted Message. A later
// Message wins the Job-row race and returns false so it can be delivered.
func (s Store) CompleteReviewFeedback(ctx context.Context, jobID, runID, revision string) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	job, err := queries.GetReviewJobForUpdate(ctx, jobID)
	if err != nil {
		return false, err
	}
	if job.Revision != revision || job.WorkflowPhase != "review-feedback" {
		return false, fmt.Errorf("unchanged review feedback conflicts with current Revision %s or workflow phase %s", job.Revision, job.WorkflowPhase)
	}
	candidate, ready, err := revisionCandidateTx(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if !ready || candidate.ID != runID {
		return false, nil
	}
	if err := expectOneRows(queries.AdvanceJobReviewFeedbackToReady(ctx, dbsql.AdvanceJobReviewFeedbackToReadyParams{JobID: jobID, Revision: revision})); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

var _ spine.ReviewStore = Store{}
