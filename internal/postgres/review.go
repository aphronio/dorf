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
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/postgres/dbsql"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

func reviewCheckInputs(ctx context.Context, queries *dbsql.Queries, jobID, revision string) ([]string, error) {
	rows, err := queries.ListReviewCheckInputs(ctx, dbsql.ListReviewCheckInputsParams{JobID: jobID, Revision: revision})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	verified := 0
	for _, row := range rows {
		names = append(names, row.Name)
		if row.EvidenceID != "" {
			verified++
		}
	}
	if len(rows) == 0 || verified != len(rows) {
		return nil, fmt.Errorf("Revision %s is not review-admissible: %d of %d declared Checks have passing Evidence", revision, verified, len(rows))
	}
	return names, nil
}

func (s Store) ReviewPlan(ctx context.Context, jobID, revision string) (spine.ReviewPlanRecord, error) {
	row, err := dbsql.New(s.DB).GetReviewPlan(ctx, dbsql.GetReviewPlanParams{JobID: jobID, Revision: revision})
	if err != nil {
		return spine.ReviewPlanRecord{}, err
	}
	return reviewPlanRecord(row.JobID, row.Revision, row.Facts, row.Plan, row.CreatedAt)
}

func (s Store) ReviewPlans(ctx context.Context, jobID string) ([]spine.ReviewPlanRecord, error) {
	rows, err := dbsql.New(s.DB).ListReviewPlans(ctx, jobID)
	if err != nil {
		return nil, err
	}
	plans := make([]spine.ReviewPlanRecord, 0, len(rows))
	for _, row := range rows {
		plan, err := reviewPlanRecord(row.JobID, row.Revision, row.Facts, row.Plan, row.CreatedAt)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func reviewPlanRecord(jobID, revision, facts, plan string, recordedAt time.Time) (spine.ReviewPlanRecord, error) {
	record := spine.ReviewPlanRecord{JobID: jobID, Revision: revision, RecordedAt: recordedAt}
	if err := json.Unmarshal([]byte(facts), &record.Facts); err != nil {
		return record, err
	}
	if err := json.Unmarshal([]byte(plan), &record.Plan); err != nil {
		return record, err
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
	currentRevision, err := queries.GetReviewJobForUpdate(ctx, proposed.JobID)
	if err != nil {
		return err
	}
	if currentRevision != proposed.Revision {
		return fmt.Errorf("review policy conflicts with current Revision %s", currentRevision)
	}
	storedDigest, err := queries.GetReviewPlanDigestForUpdate(ctx, dbsql.GetReviewPlanDigestForUpdateParams{JobID: proposed.JobID, Revision: proposed.Revision})
	if err == nil {
		if storedDigest != digest {
			return fmt.Errorf("mandatory review policy result changed across retry")
		}
		if err := clearReviewPolicyAttention(ctx, queries, proposed.JobID, proposed.Revision); err != nil {
			return err
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	declaredChecks, err := reviewCheckInputs(ctx, queries, proposed.JobID, proposed.Revision)
	if err != nil {
		return err
	}
	if err := ensureInputsTerminalForWorkflowTx(ctx, tx, proposed.JobID); err != nil {
		return fmt.Errorf("review policy blocked: %w", err)
	}
	factsJSON, _ := json.Marshal(proposed.Facts)
	planJSON, _ := json.Marshal(proposed.Plan)
	if err := expectOneRows(queries.InsertReviewPlan(ctx, dbsql.InsertReviewPlanParams{
		JobID: proposed.JobID, Revision: proposed.Revision, Facts: factsJSON, Plan: planJSON, PolicyDigest: digest,
	})); err != nil {
		return err
	}
	var nextSequence int64
	if len(proposed.Plan.Roles) > 0 {
		nextSequence, err = queries.NextMessageSequence(ctx, proposed.JobID)
		if err != nil {
			return err
		}
	}
	for index, role := range proposed.Plan.Roles {
		if _, err := createReviewRunTx(ctx, queries, proposed.JobID, proposed.Revision, string(role), proposed.Facts, declaredChecks, nextSequence+int64(index)); err != nil {
			return err
		}
	}
	if err := clearReviewPolicyAttention(ctx, queries, proposed.JobID, proposed.Revision); err != nil {
		return err
	}
	return tx.Commit()
}

func clearReviewPolicyAttention(ctx context.Context, queries *dbsql.Queries, jobID, revision string) error {
	_, err := queries.ClearWorkflowAttention(ctx, dbsql.ClearWorkflowAttentionParams{
		JobID:  jobID,
		Source: sql.NullString{String: spine.ReviewPolicyAttentionSource(revision), Valid: true},
	})
	return err
}

func createReviewRunTx(ctx context.Context, queries *dbsql.Queries, jobID, revision, role string, facts policy.ChangeFacts, declaredChecks []string, sequence int64) (string, error) {
	input := policy.RolePrompt(policy.Role(role), facts, declaredChecks)
	fromID := spine.ReviewRequestFromID(revision, role)
	message := spine.Message{
		ID: spine.ReviewRequestMessageID(jobID, revision, role), JobID: jobID,
		FromKind: spine.MessageFromWorkflow, FromID: fromID, Input: input, Intent: spine.MessageFollow,
	}
	runID := spine.AgentRunID(message.ID)
	message.Sequence = sequence
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
	if err := expectOneRows(queries.InsertReviewAgentRun(ctx, dbsql.InsertReviewAgentRunParams{ID: runID, JobID: jobID, MessageID: message.ID, Role: role, InputRevision: revision, Capability: spine.ReviewReadOnlyCapability, SandboxID: sandboxID, SubmissionNonce: submissionNonce})); err != nil {
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
			InputRevision: row.InputRevision, Capability: row.Capability, SandboxID: row.SandboxID, SubmissionNonce: row.SubmissionNonce,
		},
		Request: messageFromValues(row.MessageID, row.JobID, spine.MessageFromKind(row.RequestFromKind), row.RequestFromID, row.RequestSequence, row.RequestInput, spine.MessageDeliveryIntent(row.RequestDeliveryIntent), row.RequestTargetTurnID),
		Sandbox: spine.Sandbox{ID: row.SandboxID, JobID: row.JobID, OwnershipNonce: row.OwnershipNonce},
	}
	view.Request.AdmittedAt = row.RequestAdmittedAt
	if row.StartedAt.Valid {
		view.StartedAt = row.StartedAt.Time
	}
	if row.FinishedAt.Valid {
		view.FinishedAt = row.FinishedAt.Time
	}
	return view
}

func (s Store) ReviewRuns(ctx context.Context, jobID, revision string) ([]spine.ReviewRunView, error) {
	rows, err := dbsql.New(s.DB).ListReviewRuns(ctx, dbsql.ListReviewRunsParams{JobID: jobID, Revision: revision})
	if err != nil {
		return nil, err
	}
	views := make([]spine.ReviewRunView, 0, len(rows))
	for _, row := range rows {
		views = append(views, reviewRunView(row.DorfReviewRunProjection))
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
		result = append(result, reviewRunView(row.DorfReviewRunProjection))
	}
	return result, nil
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
		run.TurnID == "" || outcome.ID != run.TurnID || run.InputRevision != run.CurrentRevision || observed.Revision != run.InputRevision || observed.AgentRunID != runID ||
		observed.ID != spine.EvidenceID(runID, "review-observation") || observed.Kind != "review-observation" || observed.ActionID != "" || observed.CheckID != "" {
		return spine.Message{}, false, fmt.Errorf("review feedback conflicts with its completed exact-Revision reviewer AgentRun")
	}
	boundaryReady, err := queries.ReviewBoundaryReady(ctx, runID)
	if err != nil {
		return spine.Message{}, false, err
	}
	if !boundaryReady {
		return spine.Message{}, false, fmt.Errorf("review feedback lacks an attested isolated reviewer boundary")
	}

	expectedMessage := spine.Message{ID: spine.MessageID(run.JobID, spine.MessageFromAgent, runID), JobID: run.JobID, FromKind: spine.MessageFromAgent, FromID: runID, Input: outcome.Output, Intent: spine.MessageFollow}
	created := false
	storedMessage, err := queries.GetReviewFeedbackMessage(ctx, dbsql.GetReviewFeedbackMessageParams{JobID: run.JobID, RunID: runID})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return spine.Message{}, false, err
	}
	missing := errors.Is(err, sql.ErrNoRows)
	if missing && (!run.AdmissionOpen || run.OutcomeExists) {
		return spine.Message{}, false, fmt.Errorf("Job %s cannot accept new review feedback after admission closes or an Outcome is recorded", run.JobID)
	}
	if err := insertEvidence(ctx, tx, run.JobID, observed); err != nil {
		return spine.Message{}, false, err
	}
	if missing {
		expectedMessage.Sequence, err = allocateMessageSequenceTx(ctx, tx, run.JobID)
		if err != nil {
			return spine.Message{}, false, err
		}
		if err := queries.InsertMessage(ctx, dbsql.InsertMessageParams{
			ID: expectedMessage.ID, JobID: expectedMessage.JobID, FromKind: expectedMessage.FromKind, FromID: expectedMessage.FromID,
			Sequence: expectedMessage.Sequence, Input: expectedMessage.Input, DeliveryIntent: expectedMessage.Intent,
		}); err != nil {
			return spine.Message{}, false, err
		}
		implementationRunID := spine.AgentRunID(expectedMessage.ID)
		if err := expectOneRows(queries.InsertImplementationAgentRun(ctx, dbsql.InsertImplementationAgentRunParams{
			ID: implementationRunID, JobID: run.JobID, MessageID: expectedMessage.ID, SandboxID: spine.MainSandboxName(run.JobID),
		})); err != nil {
			return spine.Message{}, false, err
		}
		created = true
		storedMessage, err = queries.GetReviewFeedbackMessage(ctx, dbsql.GetReviewFeedbackMessageParams{JobID: run.JobID, RunID: runID})
		if err != nil {
			return spine.Message{}, false, err
		}
	}
	message := messageFromValues(
		storedMessage.ID, storedMessage.JobID, storedMessage.FromKind, storedMessage.FromID,
		storedMessage.Sequence, storedMessage.Input, storedMessage.DeliveryIntent, storedMessage.SteerTargetTurnID,
	)
	message.AdmittedAt = storedMessage.AdmittedAt
	if message.ID != expectedMessage.ID || message.JobID != expectedMessage.JobID || message.FromKind != expectedMessage.FromKind ||
		message.FromID != expectedMessage.FromID || message.Input != expectedMessage.Input || message.Intent != expectedMessage.Intent || message.TargetTurnID != "" {
		return spine.Message{}, false, fmt.Errorf("reviewer AgentRun %s is already bound to different exact feedback", runID)
	}
	if _, err := queries.ClearWorkflowAttention(ctx, dbsql.ClearWorkflowAttentionParams{
		JobID:  run.JobID,
		Source: sql.NullString{String: runID, Valid: true},
	}); err != nil {
		return spine.Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Message{}, false, err
	}
	return message, created, nil
}

var _ spine.ServiceStore = Store{}
