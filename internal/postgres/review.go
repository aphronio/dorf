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

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

func (s Store) ReviewPlans(ctx context.Context, jobID string) ([]coding.ReviewPlanRecord, error) {
	rows, err := dbsql.New(s.DB).ListReviewPlans(ctx, jobID)
	if err != nil {
		return nil, err
	}
	plans := make([]coding.ReviewPlanRecord, 0, len(rows))
	for _, row := range rows {
		plan, err := reviewPlanRecord(row.JobID, row.Revision, row.Facts, row.Plan, row.CreatedAt)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func reviewPlanRecord(jobID, revision, facts, plan string, recordedAt time.Time) (coding.ReviewPlanRecord, error) {
	record := coding.ReviewPlanRecord{JobID: jobID, Revision: revision, RecordedAt: recordedAt}
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

func (s Store) RecordReviewPolicy(ctx context.Context, proposed coding.ReviewPlanRecord) error {
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
	job, err := queries.GetReviewJobForUpdate(ctx, dbsql.GetReviewJobForUpdateParams{JobID: proposed.JobID, PolicyRevision: proposed.Revision})
	if err != nil {
		return err
	}
	if job.Revision != proposed.Revision {
		return fmt.Errorf("review policy conflicts with current Revision %s", job.Revision)
	}
	if job.PolicyDigest != "" {
		if job.PolicyDigest != digest {
			return fmt.Errorf("mandatory review policy result changed across retry")
		}
		if err := clearReviewPolicyAttention(ctx, queries, proposed.JobID, proposed.Revision); err != nil {
			return err
		}
		return tx.Commit()
	}
	if !job.AdmissionOpen || job.OutcomeExists {
		return fmt.Errorf("Job %s cannot record a new review policy after admission closes or an Outcome is recorded", proposed.JobID)
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
		if _, err := createReviewRunTx(ctx, queries, proposed.JobID, proposed.Revision, string(role), proposed.Facts, nextSequence+int64(index)); err != nil {
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
		Source: sql.NullString{String: coding.ReviewPolicyAttentionSource(revision), Valid: true},
	})
	return err
}

func createReviewRunTx(ctx context.Context, queries *dbsql.Queries, jobID, revision, role string, facts policy.ChangeFacts, sequence int64) (string, error) {
	input := policy.RolePrompt(policy.Role(role), facts)
	fromID := coding.ReviewRequestFromID(revision, role)
	message := spine.Message{
		ID: coding.ReviewRequestMessageID(jobID, revision, role), JobID: jobID,
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
	sandboxID := coding.ReviewSandboxName(runID)
	if err := expectOneRows(queries.ReserveSandbox(ctx, dbsql.ReserveSandboxParams{ID: sandboxID, JobID: jobID, OwnershipNonce: ownerNonce})); err != nil {
		return "", err
	}
	if err := expectOneRows(queries.InsertReviewAgentRun(ctx, dbsql.InsertReviewAgentRunParams{ID: runID, JobID: jobID, MessageID: message.ID, Role: role, InputRevision: revision, Capability: coding.ReviewReadOnlyCapability, SandboxID: sandboxID, SubmissionNonce: submissionNonce})); err != nil {
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

func (s Store) ReviewRun(ctx context.Context, runID string) (coding.ReviewRunView, error) {
	row, err := dbsql.New(s.DB).GetReviewRun(ctx, runID)
	if err != nil {
		return coding.ReviewRunView{}, err
	}
	return reviewRunView(row), nil
}

func reviewRunView(row dbsql.DorfReviewRunProjection) coding.ReviewRunView {
	view := coding.ReviewRunView{
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
	if !policy.Allowed(policy.Role(run.Role)) || run.Capability != coding.ReviewReadOnlyCapability || run.State != spine.AgentRunCompleted ||
		run.TurnID == "" || outcome.ID != run.TurnID || run.InputRevision != run.CurrentRevision || observed.Revision != run.InputRevision || observed.AgentRunID != runID ||
		observed.ID != spine.EvidenceID(runID, "review-observation") || observed.Kind != "review-observation" || observed.ActionID != "" {
		return spine.Message{}, false, fmt.Errorf("review feedback conflicts with its completed exact-Revision reviewer AgentRun")
	}
	if !run.BoundaryReady {
		return spine.Message{}, false, fmt.Errorf("review feedback lacks an attested isolated reviewer boundary")
	}

	expectedMessage := spine.Message{ID: spine.MessageID(run.JobID, spine.MessageFromAgent, runID), JobID: run.JobID, FromKind: spine.MessageFromAgent, FromID: runID, Input: outcome.Output, Intent: spine.MessageFollow}
	created := false
	messageSender := dbsql.GetMessageBySenderParams{JobID: run.JobID, FromKind: spine.MessageFromAgent, FromID: runID}
	storedMessage, err := queries.GetMessageBySender(ctx, messageSender)
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
		storedMessage, err = queries.GetMessageBySender(ctx, messageSender)
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

var _ coding.Store = Store{}
