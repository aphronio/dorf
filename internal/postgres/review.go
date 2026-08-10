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
	job, err := queries.GetReviewJobForUpdate(ctx, proposed.JobID)
	if err != nil {
		return err
	}
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
	if job.Revision != proposed.Revision || job.WorkflowPhase != "review-planning" {
		return fmt.Errorf("review policy conflicts with current Revision %s or workflow phase %s", job.Revision, job.WorkflowPhase)
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
	declaredChecks, err := queries.ListDeclaredReviewCheckNames(ctx, jobID)
	if err != nil {
		return "", err
	}
	input := policy.RolePrompt(policy.Role(role), facts, declaredChecks)
	fromID := spine.ReviewRequestFromID(revision, role)
	message := spine.Message{
		ID: spine.ReviewRequestMessageID(jobID, revision, role), JobID: jobID,
		FromKind: spine.MessageFromWorkflow, FromID: fromID, Input: input, Intent: spine.MessageFollow,
	}
	message.Sequence, err = queries.NextMessageSequence(ctx, jobID)
	if err != nil {
		return "", err
	}
	if err := queries.InsertMessage(ctx, dbsql.InsertMessageParams{
		ID: message.ID, JobID: message.JobID, FromKind: message.FromKind, FromID: message.FromID,
		Sequence: message.Sequence, Input: message.Input, DeliveryIntent: message.Intent,
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
	sandboxID := spine.ReviewSandboxName(runID)
	if err := expectOneRows(queries.ReserveSandbox(ctx, dbsql.ReserveSandboxParams{ID: sandboxID, JobID: jobID, OwnershipNonce: ownerNonce})); err != nil {
		return "", err
	}
	routeActionID := spine.ScopedActionID(jobID, spine.ActionRouteCreate, sandboxID)
	if err := expectOneRows(queries.ReserveRoute(ctx, dbsql.ReserveRouteParams{ID: spine.ProviderRouteID(routeActionID), SandboxID: sandboxID})); err != nil {
		return "", err
	}
	if err := queries.InsertReviewAgentRun(ctx, dbsql.InsertReviewAgentRunParams{ID: runID, JobID: jobID, MessageID: message.ID, Role: role, Revision: revision, Capability: spine.ReviewReadOnlyCapability, SandboxID: sandboxID, SubmissionNonce: submissionNonce}); err != nil {
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
			ID: row.ID, JobID: row.JobID, MessageID: row.MessageID, Harness: row.Harness, ThreadID: row.ThreadID,
			State: spine.AgentRunState(row.State), BaselineRecorded: row.BaselineRecorded, BaselineTurnID: row.BaselineTurnID,
			TurnID: row.TurnID, TurnOutcome: row.TurnOutcome, Attention: row.Attention, Role: row.Role,
			Revision: row.Revision, Capability: row.Capability, SandboxID: row.SandboxID, SubmissionNonce: row.SubmissionNonce,
		},
		Request: messageFromValues(row.MessageID, row.JobID, spine.MessageFromKind(row.RequestFromKind), row.RequestFromID, row.RequestSequence, row.RequestInput, spine.MessageDeliveryIntent(row.RequestDeliveryIntent), row.RequestTargetTurnID),
		Sandbox: spine.Sandbox{ID: row.SandboxID, JobID: row.JobID, State: row.SandboxState, OwnershipNonce: row.OwnershipNonce},
		Route:   spine.Route{ID: row.RouteID, SandboxID: row.SandboxID, State: row.RouteState},
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

func (s Store) BeginReviewCheckout(ctx context.Context, runID string) (spine.Action, error) {
	run, err := s.ReviewRun(ctx, runID)
	if err != nil {
		return spine.Action{}, err
	}
	return s.BeginResourceAction(ctx, run.Sandbox.ID, spine.ActionReviewCheckout)
}

// RecordReviewFeedback retains one reviewer's exact prose and feeds it back to
// the original implementation Thread as an ordinary Message. The Job row is
// locked before allocating the Message sequence so concurrent reviewer
// completions remain deterministic and idempotent.
func (s Store) RecordReviewFeedback(ctx context.Context, runID string, outcome spine.HarnessTurn, observed spine.Evidence) (spine.Message, bool, error) {
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
		run.TurnID == "" || outcome.ID != run.TurnID || run.Revision != run.CurrentRevision || observed.Revision != run.Revision || observed.AgentRunID != runID ||
		observed.ID != spine.EvidenceID(runID, "review-observation") || observed.Kind != "review-observation" || observed.ActionID != "" || observed.CheckID != "" ||
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
	if err := insertEvidence(ctx, tx, run.JobID, observed); err != nil {
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
		if err := queries.InsertMessage(ctx, dbsql.InsertMessageParams{
			ID: message.ID, JobID: message.JobID, FromKind: message.FromKind, FromID: message.FromID,
			Sequence: message.Sequence, Input: message.Input, DeliveryIntent: message.Intent,
		}); err != nil {
			return spine.Message{}, false, err
		}
		implementationRunID := spine.AgentRunID(message.ID)
		if err := expectOneRows(queries.InsertImplementationAgentRun(ctx, dbsql.InsertImplementationAgentRunParams{
			ID: implementationRunID, JobID: run.JobID, MessageID: message.ID, SandboxID: spine.MainSandboxName(run.JobID),
		})); err != nil {
			return spine.Message{}, false, err
		}
		created = true
	}
	missing, err := queries.CountMissingReviewFeedback(ctx, dbsql.CountMissingReviewFeedbackParams{JobID: run.JobID, Revision: sql.NullString{String: run.Revision, Valid: true}})
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
