package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var ErrNotFound = errors.New("Dorf Session not found")
var ErrAdmissionConflict = errors.New("admission key is bound to different complete Session input")
var sha256Digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

const (
	AbsurdReleaseCommit = "550d3b9e6f9382d96178de6ab8c90c7f8edf2227"
	AbsurdSchemaURL     = "https://raw.githubusercontent.com/earendil-works/absurd/" + AbsurdReleaseCommit + "/sql/absurd.sql"
	AbsurdSchemaSHA256  = "d34309370c539f3a51f2b36b69b1f77551f8e4a14480a1c8def8bb8f40fd9aab"
)

var dorfMigrations = []string{"001_greenfield.sql", "002_non_expiring_client_credentials.sql", "003_message_interrupt.sql", "004_direct_conversation_setup.sql", "005_message_instructions.sql", "006_remove_message_instructions.sql", "007_job_client_attribution.sql", "008_message_skill_refresh.sql", "009_message_attachments.sql", "010_job_idle_policy.sql", "011_message_developer_instructions.sql", "012_sandbox_idle_grace.sql", "013_message_observation.sql", "014_job_execution_wakes.sql", "015_observation_auto.sql", "016_profile_revisions.sql", "017_sandbox_resources.sql", "018_sandbox_delivery_holds.sql", "019_sandbox_upgrades.sql", "020_sandbox_checkpoints.sql", "021_checkpoint_recovery.sql", "022_remove_investigation.sql", "023_remove_coding.sql", "024_job_thread.sql", "025_sessions.sql"}

type Store struct{ DB *sql.DB }

func (s Store) AbsurdReady(ctx context.Context) (bool, error) {
	var installed bool
	if err := s.DB.QueryRowContext(ctx, `select to_regprocedure('absurd.get_schema_version()') is not null`).Scan(&installed); err != nil {
		return false, err
	}
	if !installed {
		return false, nil
	}
	var version string
	if err := s.DB.QueryRowContext(ctx, `select absurd.get_schema_version()`).Scan(&version); err != nil {
		return false, err
	}
	if version != "0.5.0" {
		return false, fmt.Errorf("Absurd schema version is %q; Dorf requires 0.5.0", version)
	}
	return true, nil
}

func (s Store) BootstrapAbsurd(ctx context.Context, schema []byte) error {
	sum := fmt.Sprintf("%x", sha256.Sum256(schema))
	if sum != AbsurdSchemaSHA256 {
		return fmt.Errorf("Absurd schema checksum is %s; expected pinned 0.5.0 checksum %s", sum, AbsurdSchemaSHA256)
	}
	var installed bool
	if err := s.DB.QueryRowContext(ctx, `select to_regprocedure('absurd.get_schema_version()') is not null`).Scan(&installed); err != nil {
		return err
	}
	if !installed {
		if _, err := s.DB.ExecContext(ctx, string(schema)); err != nil {
			return fmt.Errorf("initialize Absurd 0.5.0 schema: %w", err)
		}
	}
	var version string
	if err := s.DB.QueryRowContext(ctx, `select absurd.get_schema_version()`).Scan(&version); err != nil {
		return err
	}
	if version != "0.5.0" {
		return fmt.Errorf("Absurd schema version is %q; expected 0.5.0", version)
	}
	return nil
}

func (s Store) Migrate(ctx context.Context) error {
	var version string
	if err := s.DB.QueryRowContext(ctx, `select absurd.get_schema_version()`).Scan(&version); err != nil {
		return fmt.Errorf("Absurd schema is not ready: %w (initialize pinned Absurd 0.5.0 first)", err)
	}
	if version != "0.5.0" {
		return fmt.Errorf("Absurd schema version is %q; Dorf requires 0.5.0", version)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock(hashtextextended('dorf-schema-baseline',0))`); err != nil {
		return err
	}
	if err := migrateDorf(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	client, err := absurd.New(absurd.Options{DB: s.DB, QueueName: "dorf_jobs"})
	if err != nil {
		return err
	}
	if err := client.CreateQueue(ctx, "dorf_jobs"); err != nil {
		return fmt.Errorf("create Absurd queue dorf_jobs: %w", err)
	}
	return nil
}

func migrateDorf(ctx context.Context, tx *sql.Tx) error {
	var installed bool
	if err := tx.QueryRowContext(ctx, `select to_regnamespace('dorf') is not null`).Scan(&installed); err != nil {
		return err
	}
	applied := map[string]bool{}
	if installed {
		var migrationsTable bool
		if err := tx.QueryRowContext(ctx, `select to_regclass('dorf.schema_migrations') is not null`).Scan(&migrationsTable); err != nil {
			return err
		}
		if !migrationsTable {
			return fmt.Errorf("existing Dorf schema has no baseline identity; recreate this prototype database")
		}
		rows, err := tx.QueryContext(ctx, `select name from dorf.schema_migrations order by name`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			applied[name] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if !applied["001_greenfield.sql"] {
			return fmt.Errorf("existing Dorf schema has no baseline identity; recreate this prototype database")
		}
		for name := range applied {
			known := false
			for _, migration := range dorfMigrations {
				known = known || name == migration
			}
			if !known {
				return fmt.Errorf("Dorf migration history contains unsupported migration %q", name)
			}
		}
	}
	for _, name := range dorfMigrations {
		if applied[name] {
			continue
		}
		contents, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			return fmt.Errorf("apply Dorf migration %s: %w", name, err)
		}
	}
	return nil
}

type admittedAgentRun struct {
	Role          string
	Capability    string
	InputRevision string
	SandboxID     string
}

func (s Store) admitMessage(ctx context.Context, input core.MessageAdmission) (core.MessageAdmissionResult, error) {
	input, err := normalizeMessage(input)
	if err != nil {
		return core.MessageAdmissionResult{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.MessageAdmissionResult{}, err
	}
	defer tx.Rollback()
	message, created, err := admitMessageTx(ctx, tx, input)
	if err != nil {
		return core.MessageAdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.MessageAdmissionResult{}, err
	}
	return core.MessageAdmissionResult{Message: message, SandboxID: input.SandboxID, Created: created}, nil
}

func normalizeMessage(input core.MessageAdmission) (core.MessageAdmission, error) {
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.SandboxID = strings.TrimSpace(input.SandboxID)
	input.FromKind = core.MessageFromKind(strings.TrimSpace(string(input.FromKind)))
	input.FromID = strings.TrimSpace(input.FromID)
	if input.FromKind == "" {
		input.FromKind = core.MessageFromHuman
	}
	if input.Intent == "" {
		input.Intent = core.MessageFollow
	}
	if input.SessionID == "" || input.SandboxID == "" || input.FromID == "" {
		return core.MessageAdmission{}, fmt.Errorf("message admission requires Session ID, exact Sandbox ID, from ID, and text or attachments")
	}
	if input.FromKind != core.MessageFromHuman && input.FromKind != core.MessageFromAgent && input.FromKind != core.MessageFromWorkflow {
		return core.MessageAdmission{}, fmt.Errorf("invalid message from kind")
	}
	if len(input.FromID) > 256 {
		return core.MessageAdmission{}, fmt.Errorf("from ID must be at most 256 characters")
	}
	if input.Intent != core.MessageFollow && input.Intent != core.MessageSteer && input.Intent != core.MessageAuto {
		return core.MessageAdmission{}, fmt.Errorf("message intent must be auto, follow, or steer")
	}
	if !core.ValidObservationDelivery(input.Observation, input.Intent, len(input.Attachments)) {
		return core.MessageAdmission{}, fmt.Errorf("observations require text-only follow or auto delivery")
	}
	if !core.ValidMessageInput(core.MessageInput{Text: input.Input, Attachments: input.Attachments, Observation: input.Observation, DeveloperInstructions: input.DeveloperInstructions}) {
		return core.MessageAdmission{}, fmt.Errorf("message text or attachments are invalid")
	}
	input.Attachments = append([]core.MessageAttachment(nil), input.Attachments...)
	return input, nil
}

func admitMessageTx(ctx context.Context, tx *sql.Tx, input core.MessageAdmission) (core.Message, bool, error) {
	queries := dbsql.New(tx)
	session, err := queries.GetSessionAdmissionForUpdate(ctx, input.SessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.Message{}, false, ErrNotFound
		}
		return core.Message{}, false, err
	}
	if session.WorkflowName != "" || session.WorkflowRevision != "" {
		return core.Message{}, false, fmt.Errorf("Session %s is not client-directed", input.SessionID)
	}
	row, err := queries.GetMessageBySender(ctx, dbsql.GetMessageBySenderParams{SessionID: input.SessionID, FromKind: input.FromKind, FromID: input.FromID})
	if err == nil {
		return replayMessageAdmission(ctx, queries, row, input)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.Message{}, false, err
	}
	if !session.AdmissionOpen {
		return core.Message{}, false, fmt.Errorf("%w for Session %s", core.ErrMessageAdmissionClosed, input.SessionID)
	}
	run, err := resolveDirectMessageEnvelope(input)
	if err != nil {
		return core.Message{}, false, err
	}
	if run.Role == "" || run.SandboxID != input.SandboxID {
		return core.Message{}, false, fmt.Errorf("Message execution envelope returned a foreign Sandbox delivery")
	}
	target, err := resolveMessageTarget(ctx, queries, input, run)
	if err != nil {
		return core.Message{}, false, err
	}
	var message core.Message
	message.TargetTurnID = target.turnID
	message.RefreshSkills = input.RefreshSkills
	message.Observation = input.Observation
	message.DeveloperInstructions = input.DeveloperInstructions
	message.Sequence, err = queries.NextMessageSequence(ctx, input.SessionID)
	if err != nil {
		return core.Message{}, false, err
	}
	message.ID = core.MessageID(input.SessionID, input.FromKind, input.FromID)
	message.SessionID, message.FromKind, message.FromID, message.Input, message.Attachments, message.Intent = input.SessionID, input.FromKind, input.FromID, input.Input, input.Attachments, target.intent
	message.RequestedIntent = input.Intent
	attachments, err := encodeMessageAttachments(message.Attachments)
	if err != nil {
		return core.Message{}, false, err
	}
	if err := queries.InsertMessage(ctx, dbsql.InsertMessageParams{ID: message.ID, SessionID: message.SessionID, FromKind: message.FromKind, FromID: message.FromID, Sequence: message.Sequence, Input: message.Input, Attachments: attachments, DeliveryIntent: message.Intent, RequestedIntent: string(input.Intent), Observation: input.Observation, DeveloperInstructions: instructionSQL(input.DeveloperInstructions), RefreshSkills: input.RefreshSkills, SteerTargetTurnID: message.TargetTurnID}); err != nil {
		return core.Message{}, false, err
	}
	runID := core.AgentRunID(message.ID)
	rows, err := queries.InsertAdmittedAgentRun(ctx, dbsql.InsertAdmittedAgentRunParams{
		ID: runID, SessionID: message.SessionID, MessageID: message.ID,
		Harness: nullableString(target.harness), ThreadID: nullableString(target.threadID),
		Role: run.Role, InputRevision: nullableString(run.InputRevision),
		Capability: nullableString(run.Capability), SandboxID: run.SandboxID,
	})
	if err := expectOneRows(rows, err); err != nil {
		return core.Message{}, false, fmt.Errorf("insert %s execution-envelope AgentRun: %w", run.Role, err)
	}
	storedMessage, err := queries.GetMessageBySender(ctx, dbsql.GetMessageBySenderParams{SessionID: message.SessionID, FromKind: message.FromKind, FromID: message.FromID})
	if err != nil {
		return core.Message{}, false, err
	}
	message.AdmittedAt = storedMessage.AdmittedAt
	return message, true, nil
}

func replayMessageAdmission(ctx context.Context, queries *dbsql.Queries, row dbsql.GetMessageBySenderRow, input core.MessageAdmission) (core.Message, bool, error) {
	message, err := messageFromSenderRow(row)
	if err != nil {
		return core.Message{}, false, err
	}
	run, err := queries.GetAgentRunByMessage(ctx, message.ID)
	if err != nil {
		return core.Message{}, false, fmt.Errorf("load durable AgentRun for Message replay: %w", err)
	}
	stored := core.MessageAdmission{
		SessionID: run.SessionID, SandboxID: run.SandboxID, FromKind: message.FromKind, FromID: message.FromID,
		Input: message.Input, Attachments: message.Attachments, Intent: core.MessageDeliveryIntent(row.RequestedIntent), RefreshSkills: message.RefreshSkills, Observation: message.Observation, DeveloperInstructions: message.DeveloperInstructions,
	}
	if !sameMessageAdmission(stored, input) {
		return core.Message{}, false, fmt.Errorf("%w: sender %s/%q", core.ErrMessageReplayConflict, input.FromKind, input.FromID)
	}
	return message, false, nil
}

type messageTarget struct {
	intent   core.MessageDeliveryIntent
	harness  string
	threadID string
	turnID   string
}

func resolveMessageTarget(ctx context.Context, queries *dbsql.Queries, input core.MessageAdmission, run admittedAgentRun) (messageTarget, error) {
	target := messageTarget{intent: core.MessageFollow}
	held, err := queries.SandboxDeliveryHeld(ctx, run.SandboxID)
	if err != nil {
		return messageTarget{}, err
	}
	if held {
		if input.Intent == core.MessageSteer {
			return messageTarget{}, core.ErrMessageSteerUnavailable
		}
		return target, nil
	}
	if input.Intent == core.MessageFollow {
		return target, nil
	}
	active, err := queries.GetActiveAgentTurn(ctx, dbsql.GetActiveAgentTurnParams{
		SessionID: input.SessionID, Role: run.Role, SandboxID: run.SandboxID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		if input.Intent == core.MessageSteer {
			return messageTarget{}, core.ErrMessageSteerUnavailable
		}
		return target, nil
	}
	if err != nil {
		return messageTarget{}, err
	}
	return messageTarget{intent: core.MessageSteer, harness: active.Harness, threadID: active.ThreadID, turnID: active.TurnID}, nil
}

func allocateMessageSequenceTx(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error) {
	return dbsql.New(tx).NextMessageSequence(ctx, sessionID)
}

func (s Store) Session(ctx context.Context, id string) (core.Session, error) {
	row, err := dbsql.New(s.DB).GetSession(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Session{}, ErrNotFound
	}
	if err != nil {
		return core.Session{}, err
	}
	return core.Session{
		CreatedByClientID: row.CreatedByClientID, CreatedByClientName: row.CreatedByClientName, ClientReference: row.ClientReference,
		ID: row.ID, AdmissionKey: row.AdmissionKey, Workflow: core.WorkflowName(row.WorkflowName), WorkflowRevision: row.WorkflowRevision,
		AgentsMD: row.AgentsMd,
		Harness:  row.Harness, ThreadID: row.ThreadID,
		SandboxProfile: row.SandboxProfile, SandboxProfileRevision: row.SandboxProfileRevision, ProviderConnection: row.ProviderConnection,
		KeepRunning: row.KeepRunning, Model: row.Model, ReasoningEffort: row.ReasoningEffort, AdmissionOpen: row.AdmissionOpen, CleanupState: core.CleanupState(row.CleanupState),
		CurrentTaskID:     row.CurrentTaskID,
		WorkflowAttention: row.WorkflowAttention, WorkflowAttentionSource: row.WorkflowAttentionSource,
		WorkflowAttentionAt: timeValue(row.WorkflowAttentionAt), CleanupAttention: row.CleanupAttention,
		AdmittedAt: row.AdmittedAt, CleanedAt: timeValue(row.CleanedAt),
	}, nil
}

func (s Store) SessionExists(ctx context.Context, id string) (bool, error) {
	_, err := s.Session(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s Store) SessionTasks(ctx context.Context, sessionID string) ([]core.SessionTask, error) {
	rows, err := dbsql.New(s.DB).ListSessionTasks(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	tasks := make([]core.SessionTask, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, core.SessionTask{
			SessionID: row.SessionID, Sequence: row.Sequence, TaskID: row.TaskID,
			TaskName: row.TaskName, AttachedAt: row.AttachedAt,
		})
	}
	return tasks, nil
}

// WithSessionFence serializes harness and other external mutation for one Session
// independently of an expiring Absurd claim. Message admission intentionally
// does not take this long-lived fence.
func (s Store) WithSessionFence(ctx context.Context, sessionID string, fn func() error) error {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := acquireSessionFenceTx(ctx, tx, sessionID); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return tx.Commit()
}

func acquireSessionFenceTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock(hashtextextended('dorf-job-effect:' || $1, 0))`, sessionID); err != nil {
		return fmt.Errorf("acquire Session execution fence: %w", err)
	}
	return nil
}

// AttachSessionTask appends one exact Absurd task handoff. The deterministic Absurd
// idempotency key supplies task identity; Dorf records only ordered attachment.
func (s Store) AttachSessionTask(ctx context.Context, sessionID, expectedCurrentTaskID, taskID, taskName string) error {
	return s.attachSessionTask(ctx, sessionID, expectedCurrentTaskID, taskID, taskName, false)
}

func messageFromValues(id, sessionID string, fromKind core.MessageFromKind, fromID string, sequence int64, input string, intent core.MessageDeliveryIntent, targetTurnID string) core.Message {
	return core.Message{ID: id, SessionID: sessionID, FromKind: fromKind, FromID: fromID, Sequence: sequence, Input: input, Intent: intent, TargetTurnID: targetTurnID}
}

func messageFromStoredValues(id, sessionID string, fromKind core.MessageFromKind, fromID string, sequence int64, input string, attachments []byte, intent core.MessageDeliveryIntent, targetTurnID string) (core.Message, error) {
	message := messageFromValues(id, sessionID, fromKind, fromID, sequence, input, intent, targetTurnID)
	decoded, err := decodeMessageAttachments(attachments)
	if err != nil {
		return core.Message{}, fmt.Errorf("Message %s has invalid durable attachments: %w", id, err)
	}
	message.Attachments = decoded
	return message, nil
}

func messageFromSenderRow(row dbsql.GetMessageBySenderRow) (core.Message, error) {
	message, err := messageFromStoredValues(
		row.ID, row.SessionID, row.FromKind, row.FromID, row.Sequence, row.Input,
		row.Attachments, row.DeliveryIntent, row.SteerTargetTurnID,
	)
	if err != nil {
		return core.Message{}, err
	}
	message.AdmittedAt = row.AdmittedAt
	message.RequestedIntent = core.MessageDeliveryIntent(row.RequestedIntent)
	message.RefreshSkills = row.RefreshSkills
	message.Observation = row.Observation
	message.DeveloperInstructions = instructionPointer(row.DeveloperInstructions)
	return message, nil
}

func encodeMessageAttachments(attachments []core.MessageAttachment) ([]byte, error) {
	if attachments == nil {
		attachments = []core.MessageAttachment{}
	}
	encoded, err := json.Marshal(attachments)
	if err != nil {
		return nil, fmt.Errorf("encode Message attachments: %w", err)
	}
	return encoded, nil
}

func decodeMessageAttachments(encoded []byte) ([]core.MessageAttachment, error) {
	var attachments []core.MessageAttachment
	if err := json.Unmarshal(encoded, &attachments); err != nil {
		return nil, err
	}
	if !core.ValidMessageAttachments(attachments) {
		return nil, fmt.Errorf("invalid attachment manifest")
	}
	if len(attachments) == 0 {
		return nil, nil
	}
	return attachments, nil
}

func sameMessageAdmission(left, right core.MessageAdmission) bool {
	if left.Observation != right.Observation || !core.SameDeveloperInstructions(left.DeveloperInstructions, right.DeveloperInstructions) || left.RefreshSkills != right.RefreshSkills || left.SessionID != right.SessionID || left.SandboxID != right.SandboxID ||
		left.FromKind != right.FromKind || left.FromID != right.FromID || left.Input != right.Input || left.Intent != right.Intent ||
		len(left.Attachments) != len(right.Attachments) {
		return false
	}
	for index := range left.Attachments {
		if left.Attachments[index] != right.Attachments[index] {
			return false
		}
	}
	return true
}

func actionFromValues(id, sessionID string, kind core.ActionKind, state core.ActionState, scope string, createdAt time.Time, settledAt sql.NullTime) core.Action {
	return core.Action{ID: id, SessionID: sessionID, Kind: kind, State: state, Scope: scope, CreatedAt: createdAt, SettledAt: timeValue(settledAt)}
}

func exactScopedAction(row dbsql.DorfAction, sessionID string, kind core.ActionKind, scope string) (core.Action, error) {
	expectedID := core.ScopedActionID(sessionID, kind, scope)
	if row.ID != expectedID || row.SessionID != sessionID || row.Kind != kind || row.ScopeKey != scope {
		return core.Action{}, fmt.Errorf("Action %s conflicts with exact Session %s, kind %s, and scope %s", row.ID, sessionID, kind, scope)
	}
	return actionFromValues(row.ID, row.SessionID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt), nil
}

func agentRunFromValues(id, sessionID, messageID string, state core.AgentRunState, harness, threadID string, baselineRecorded bool, baselineTurnID, turnID, turnOutcome, attention, role, inputRevision string) core.AgentRun {
	return core.AgentRun{ID: id, SessionID: sessionID, MessageID: messageID, Harness: harness, ThreadID: threadID, State: state, BaselineRecorded: baselineRecorded, BaselineTurnID: baselineTurnID, TurnID: turnID, TurnOutcome: turnOutcome, Attention: attention, Role: role, InputRevision: inputRevision}
}

func agentRunOutcome(state core.AgentRunState, outcome string) string {
	if state != core.AgentRunCompleted && state != core.AgentRunFailed && state != core.AgentRunInterrupted {
		return ""
	}
	if outcome == "completed" || outcome == "failed" || outcome == "interrupted" {
		return outcome
	}
	if state == core.AgentRunCompleted {
		return ""
	}
	return string(state)
}

func timeValue(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}

func (s Store) AttachCleanupTask(ctx context.Context, sessionID, expectedCurrentTaskID, taskID, taskName string) error {
	return s.attachSessionTask(ctx, sessionID, expectedCurrentTaskID, taskID, taskName, true)
}

func (s Store) attachSessionTask(ctx context.Context, sessionID, expectedCurrentTaskID, taskID, taskName string, cleanup bool) error {
	sessionID = strings.TrimSpace(sessionID)
	expectedCurrentTaskID = strings.TrimSpace(expectedCurrentTaskID)
	taskID = strings.TrimSpace(taskID)
	taskName = strings.TrimSpace(taskName)
	if sessionID == "" || taskID == "" || taskName == "" {
		return fmt.Errorf("Session task attachment requires exact Session, task, and task-name identities")
	}
	if cleanup && taskName != core.CleanupTaskName {
		return fmt.Errorf("Session cleanup task must use Core task name %s", core.CleanupTaskName)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := attachSessionTaskTx(ctx, dbsql.New(tx), sessionID, expectedCurrentTaskID, taskID, taskName, cleanup); err != nil {
		return err
	}
	return tx.Commit()
}

func attachSessionTaskTx(ctx context.Context, queries *dbsql.Queries, sessionID, expectedCurrentTaskID, taskID, taskName string, cleanup bool) error {
	current, err := queries.GetCurrentSessionTaskForUpdate(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := validateTaskAttachmentState(current, sessionID, taskID, cleanup); err != nil {
		return err
	}
	if current.TaskID == taskID {
		if current.TaskName != taskName {
			return fmt.Errorf("Absurd task %s is already attached as %s", taskID, current.TaskName)
		}
	} else {
		if current.TaskID != expectedCurrentTaskID {
			return fmt.Errorf("Session %s current task is %q, not expected predecessor %q", sessionID, current.TaskID, expectedCurrentTaskID)
		}
		inserted, err := queries.InsertSessionTask(ctx, dbsql.InsertSessionTaskParams{
			SessionID: sessionID, Sequence: current.Sequence + 1, TaskID: taskID, TaskName: taskName,
		})
		if err != nil {
			return err
		}
		if inserted != 1 {
			return fmt.Errorf("Absurd task %s is already attached to another Session", taskID)
		}
	}
	if cleanup && current.CleanupState == core.CleanupRequested {
		updated, err := queries.MarkCleanupScheduled(ctx, sessionID)
		if err != nil {
			return err
		}
		if updated != 1 {
			return fmt.Errorf("Session %s cleanup scheduling did not settle", sessionID)
		}
	}
	return nil
}

func validateTaskAttachmentState(current dbsql.GetCurrentSessionTaskForUpdateRow, sessionID, taskID string, cleanup bool) error {
	if cleanup {
		if current.AdmissionOpen || (current.CleanupState != core.CleanupRequested && current.CleanupState != core.CleanupScheduled) {
			return fmt.Errorf("Session %s cannot attach cleanup from state %s", sessionID, current.CleanupState)
		}
		if current.CleanupState == core.CleanupScheduled && current.TaskID != taskID {
			return fmt.Errorf("Session %s already has cleanup task %s", sessionID, current.TaskID)
		}
	} else if !current.AdmissionOpen || current.CleanupState != core.CleanupPending {
		return fmt.Errorf("Session %s cannot attach ordinary task after cleanup begins", sessionID)
	}
	return nil
}

func (s Store) GetOrCreateSandboxAction(ctx context.Context, sandboxID string, kind core.ActionKind) (core.Action, error) {
	sandbox, err := dbsql.New(s.DB).GetSandbox(ctx, sandboxID)
	if err != nil {
		return core.Action{}, err
	}
	id := core.ScopedActionID(sandbox.SessionID, kind, sandboxID)
	q := dbsql.New(s.DB)
	insertErr := expectOneRows(q.InsertScopedAction(ctx, dbsql.InsertScopedActionParams{ID: id, SessionID: sandbox.SessionID, Kind: kind, ScopeKey: sandboxID}))
	row, getErr := q.GetScopedAction(ctx, dbsql.GetScopedActionParams{SessionID: sandbox.SessionID, Kind: kind, ScopeKey: sandboxID})
	if getErr != nil {
		if insertErr != nil {
			return core.Action{}, insertErr
		}
		return core.Action{}, getErr
	}
	return exactScopedAction(row, sandbox.SessionID, kind, sandboxID)
}

func (s Store) Sandbox(ctx context.Context, id string) (core.Sandbox, error) {
	row, err := dbsql.New(s.DB).GetSandbox(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Sandbox{}, ErrNotFound
	}
	if err != nil {
		return core.Sandbox{}, err
	}
	return core.Sandbox{ID: row.ID, SessionID: row.SessionID, Name: row.Name, OwnershipNonce: row.OwnershipNonce, ResourceID: row.ActiveResourceID, ProviderID: row.ProviderID}, nil
}
func (s Store) Sandboxes(ctx context.Context, sessionID string) ([]core.Sandbox, error) {
	rows, err := dbsql.New(s.DB).ListSessionSandboxes(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]core.Sandbox, 0, len(rows))
	for _, r := range rows {
		out = append(out, core.Sandbox{ID: r.ID, SessionID: r.SessionID, Name: r.Name, OwnershipNonce: r.OwnershipNonce, ResourceID: r.ActiveResourceID, ProviderID: r.ProviderID})
	}
	return out, nil
}

func (s Store) Deliveries(ctx context.Context, sessionID string) ([]core.Delivery, error) {
	rows, err := dbsql.New(s.DB).ListDeliveries(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]core.Delivery, 0, len(rows))
	for _, r := range rows {
		if !r.AgentRunPresent {
			return nil, fmt.Errorf("Message %s (sequence %d) has no AgentRun", r.MessageID, r.Sequence)
		}
		if r.AgentRunMessageID != r.MessageID || r.AgentRunSessionID != r.MessageSessionID {
			return nil, fmt.Errorf("Message %s (Session %s) has mismatched AgentRun %s (Message %s, Session %s)", r.MessageID, r.MessageSessionID, r.AgentRunID, r.AgentRunMessageID, r.AgentRunSessionID)
		}
		message, err := messageFromStoredValues(r.MessageID, r.MessageSessionID, r.FromKind, r.FromID, r.Sequence, r.Input, r.Attachments, r.DeliveryIntent, r.SteerTargetTurnID)
		if err != nil {
			return nil, err
		}
		message.AdmittedAt = r.AdmittedAt
		message.RequestedIntent = core.MessageDeliveryIntent(r.RequestedIntent)
		message.RefreshSkills = r.RefreshSkills
		message.Observation = r.Observation
		message.DeveloperInstructions = instructionPointer(r.DeveloperInstructions)
		run := agentRunFromValues(r.AgentRunID, r.AgentRunSessionID, r.AgentRunMessageID, r.State, r.Harness, r.ThreadID, r.BaselineRecorded, r.BaselineTurnID, r.TurnID, r.TurnOutcome, r.Attention, r.Role, r.InputRevision)
		run.Capability = r.Capability
		run.SandboxID = r.SandboxID
		run.SubmissionNonce = r.SubmissionNonce
		run.InterruptRequested = r.InterruptRequested
		run.StartedAt = timeValue(r.StartedAt)
		run.FinishedAt = timeValue(r.FinishedAt)
		out = append(out, core.Delivery{Message: message, AgentRun: run})
	}
	return out, nil
}

// AgentMessageExecution reloads the exact durable execution aggregate by the
// stable Message identity. Callers that may touch the Harness invoke this only
// while holding the owning Session's effect fence and discard earlier snapshots.
func (s Store) AgentMessageExecution(ctx context.Context, messageID string) (core.AgentMessageExecution, error) {
	queries := dbsql.New(s.DB)
	messageRow, err := queries.GetMessage(ctx, messageID)
	if err != nil {
		return core.AgentMessageExecution{}, err
	}
	message, err := messageFromStoredValues(messageRow.ID, messageRow.SessionID, messageRow.FromKind, messageRow.FromID, messageRow.Sequence, messageRow.Input, messageRow.Attachments, messageRow.DeliveryIntent, messageRow.SteerTargetTurnID)
	if err != nil {
		return core.AgentMessageExecution{}, err
	}
	message.AdmittedAt = messageRow.AdmittedAt
	message.RequestedIntent = core.MessageDeliveryIntent(messageRow.RequestedIntent)
	message.RefreshSkills = messageRow.RefreshSkills
	message.Observation = messageRow.Observation
	message.DeveloperInstructions = instructionPointer(messageRow.DeveloperInstructions)
	runRow, err := queries.GetAgentRunByMessage(ctx, message.ID)
	if err != nil {
		return core.AgentMessageExecution{}, fmt.Errorf("Message %s has no atomically admitted AgentRun: %w", message.ID, err)
	}
	run := agentRunFromValues(runRow.ID, runRow.SessionID, runRow.MessageID, runRow.State, runRow.Harness, runRow.ThreadID, runRow.BaselineRecorded, runRow.BaselineTurnID, runRow.TurnID, runRow.TurnOutcome, runRow.Attention, runRow.Role, runRow.InputRevision)
	run.Capability = runRow.Capability
	run.SandboxID = runRow.SandboxID
	run.SubmissionNonce = runRow.SubmissionNonce
	run.InterruptRequested = runRow.InterruptRequested
	run.StartedAt = timeValue(runRow.StartedAt)
	run.FinishedAt = timeValue(runRow.FinishedAt)
	session, err := s.Session(ctx, message.SessionID)
	if err != nil {
		return core.AgentMessageExecution{}, err
	}
	sandbox, err := s.Sandbox(ctx, run.SandboxID)
	if err != nil {
		return core.AgentMessageExecution{}, err
	}
	if run.MessageID != message.ID || run.SessionID != session.ID || message.SessionID != session.ID || sandbox.SessionID != session.ID || run.SandboxID != sandbox.ID {
		return core.AgentMessageExecution{}, fmt.Errorf("Message %s execution does not match its authoritative Session, AgentRun, and Sandbox", message.ID)
	}
	refreshSkills, err := queries.AgentMessageNeedsSkillRefresh(ctx, messageID)
	if err != nil {
		return core.AgentMessageExecution{}, err
	}
	return core.AgentMessageExecution{Session: session, Message: message, AgentRun: run, Sandbox: sandbox, RefreshSkills: refreshSkills}, nil
}

func (s Store) InterruptAgentRun(ctx context.Context, runID, reason string) error {
	q := dbsql.New(s.DB)
	row, err := q.GetAgentRunForBinding(ctx, runID)
	if err != nil {
		return err
	}
	if row.State == core.AgentRunCompleted || row.State == core.AgentRunFailed || row.State == core.AgentRunInterrupted {
		return nil
	}
	return expectOneRows(q.InterruptAgentRun(ctx, dbsql.InterruptAgentRunParams{Reason: reason, RunID: runID}))
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func (s Store) SetWorkflowAttention(ctx context.Context, sessionID, source, detail string) error {
	source, detail = strings.TrimSpace(source), strings.TrimSpace(detail)
	if sessionID == "" || source == "" || detail == "" {
		return fmt.Errorf("workflow attention requires Session ID, exact source, and detail")
	}
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return expectOneRows(dbsql.New(s.DB).SetWorkflowAttention(ctx, dbsql.SetWorkflowAttentionParams{SessionID: sessionID, Source: sql.NullString{String: source, Valid: true}, Detail: sql.NullString{String: detail, Valid: true}}))
}

func (s Store) ClearWorkflowAttention(ctx context.Context, sessionID, source string) error {
	source = strings.TrimSpace(source)
	if sessionID == "" || source == "" {
		return fmt.Errorf("workflow attention clearing requires Session ID and exact source")
	}
	rows, err := dbsql.New(s.DB).ClearWorkflowAttention(ctx, dbsql.ClearWorkflowAttentionParams{
		SessionID: sessionID, Source: sql.NullString{String: source, Valid: true},
	})
	if err != nil {
		return err
	}
	if rows > 1 {
		return fmt.Errorf("workflow attention source %s changed %d Sessions", source, rows)
	}
	return nil
}

func (s Store) RecordSandboxActionSuccess(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	completed, err := authorizeSandboxActionTx(ctx, queries, id, "", "", false)
	if err != nil {
		return err
	}
	if completed.Action.State == core.ActionSucceeded {
		return tx.Commit()
	}
	if err := expectOneRows(queries.RecordSandboxActionSuccess(ctx, id)); err != nil {
		return err
	}
	if completed.Action.Kind == core.ActionSandboxDelete {
		if err := expectOneRows(queries.RecordSandboxResourceDeleted(ctx, dbsql.RecordSandboxResourceDeletedParams{
			ResourceID: completed.Sandbox.ResourceID, SandboxID: completed.Sandbox.ID,
		})); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AuthorizeSandboxAction validates immutable ownership and cleanup
// prerequisites before any provider mutation. In particular, external delete
// is never attempted until route revocation is durably settled.
func (s Store) AuthorizeSandboxAction(ctx context.Context, id, taskID, taskName string) (core.SandboxActionAuthorization, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.SandboxActionAuthorization{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	authorized, err := authorizeSandboxActionTx(ctx, queries, id, strings.TrimSpace(taskID), strings.TrimSpace(taskName), true)
	if err != nil {
		return core.SandboxActionAuthorization{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.SandboxActionAuthorization{}, err
	}
	return authorized, nil
}

func authorizeSandboxActionTx(ctx context.Context, queries *dbsql.Queries, id, taskID, taskName string, requireTask bool) (core.SandboxActionAuthorization, error) {
	row, err := queries.GetActionByIDForUpdate(ctx, id)
	if err != nil {
		return core.SandboxActionAuthorization{}, err
	}
	if row.State != core.ActionUnsettled && row.State != core.ActionSucceeded {
		return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox Action %s is %s, not unsettled or succeeded", id, row.State)
	}
	if row.ScopeKey == "" || row.ID != core.ScopedActionID(row.SessionID, row.Kind, row.ScopeKey) {
		return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox Action %s conflicts with its exact Session and Sandbox", id)
	}
	owned, err := queries.GetSandbox(ctx, row.ScopeKey)
	if err != nil {
		return core.SandboxActionAuthorization{}, err
	}
	if owned.SessionID != row.SessionID {
		return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox Action %s conflicts with its exact Session and Sandbox", id)
	}
	session, err := queries.GetSessionForSandboxActionAuthorization(ctx, row.SessionID)
	if err != nil {
		return core.SandboxActionAuthorization{}, err
	}
	if requireTask && (taskID == "" || taskName == "" || session.CurrentTaskID != taskID || session.CurrentTaskName != taskName) {
		return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox Action %s requires the exact current attached task", id)
	}
	cleanup := row.Kind == core.ActionRouteRevoke || row.Kind == core.ActionSandboxDelete
	if cleanup {
		if session.AdmissionOpen || session.CleanupState != core.CleanupScheduled || requireTask && taskName != core.CleanupTaskName {
			return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox cleanup Action %s requires a durably scheduled cleanup", id)
		}
	} else if !session.AdmissionOpen || session.CleanupState != core.CleanupPending {
		return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox Action %s cannot mutate provider after cleanup begins", id)
	}
	if row.Kind == core.ActionSandboxDelete {
		revoked, err := queries.GetScopedAction(ctx, dbsql.GetScopedActionParams{SessionID: row.SessionID, Kind: core.ActionRouteRevoke, ScopeKey: row.ScopeKey})
		if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.State != core.ActionSucceeded) {
			return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox cleanup cannot delete before its exact route revoke Action succeeds")
		}
		if err != nil {
			return core.SandboxActionAuthorization{}, err
		}
	}
	return core.SandboxActionAuthorization{
		Session: core.Session{
			CreatedByClientID: session.CreatedByClientID, CreatedByClientName: session.CreatedByClientName, ClientReference: session.ClientReference,
			ID: session.ID, AdmissionKey: session.AdmissionKey, Workflow: session.WorkflowName, WorkflowRevision: session.WorkflowRevision, AgentsMD: session.AgentsMd,
			Harness: session.Harness, ThreadID: session.ThreadID,
			KeepRunning: session.KeepRunning, SandboxProfile: session.SandboxProfile, SandboxProfileRevision: session.SandboxProfileRevision, ProviderConnection: session.ProviderConnection, Model: session.Model, ReasoningEffort: session.ReasoningEffort,
			AdmissionOpen: session.AdmissionOpen, CleanupState: session.CleanupState, CurrentTaskID: session.CurrentTaskID,
			WorkflowAttention: session.WorkflowAttention, WorkflowAttentionSource: session.WorkflowAttentionSource,
			WorkflowAttentionAt: timeValue(session.WorkflowAttentionAt), CleanupAttention: session.CleanupAttention,
			AdmittedAt: session.AdmittedAt, CleanedAt: timeValue(session.CleanedAt),
		},
		Sandbox: core.Sandbox{ID: owned.ID, SessionID: owned.SessionID, Name: owned.Name, OwnershipNonce: owned.OwnershipNonce, ResourceID: owned.ActiveResourceID, ProviderID: owned.ProviderID},
		Action:  actionFromValues(row.ID, row.SessionID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt),
		TaskID:  session.CurrentTaskID, TaskName: session.CurrentTaskName,
	}, nil
}

// AgentMessage selects one opaque Message across the whole Session.
// Steer priority, Follow FIFO, recovery ordering, and retained-Thread adoption
// are invariant for every consumer.
func (s Store) AgentMessage(ctx context.Context, sessionID string) (*core.AgentMessageWork, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("Agent Message selection requires an exact Session")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	session, err := queries.GetSessionAdmissionForUpdate(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !session.AdmissionOpen || session.CleanupState != core.CleanupPending {
		return nil, nil
	}
	row, err := queries.NextAgentMessage(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	message := core.Message{
		ID: row.ID, SessionID: row.SessionID, FromKind: core.MessageFromKind(row.FromKind), FromID: row.FromID,
		RefreshSkills: row.RefreshSkills, Sequence: row.Sequence, Intent: core.MessageDeliveryIntent(row.DeliveryIntent), RequestedIntent: core.MessageDeliveryIntent(row.RequestedIntent), TargetTurnID: row.SteerTargetTurnID, AdmittedAt: row.AdmittedAt,
	}
	runRow, err := queries.GetAgentRunByMessage(ctx, message.ID)
	if err != nil {
		return nil, fmt.Errorf("delivery Message %s has no atomically admitted AgentRun: %w", message.ID, err)
	}
	run := agentRunFromValues(runRow.ID, runRow.SessionID, runRow.MessageID, runRow.State, runRow.Harness, runRow.ThreadID, runRow.BaselineRecorded, runRow.BaselineTurnID, runRow.TurnID, runRow.TurnOutcome, runRow.Attention, runRow.Role, runRow.InputRevision)
	run.SandboxID = runRow.SandboxID
	if message.Intent == core.MessageFollow && run.State == core.AgentRunPending && run.ThreadID == "" && session.ThreadID != "" {
		if err := expectOneRows(queries.BindPendingFollowToSessionThread(ctx, message.ID)); err != nil {
			return nil, err
		}
		run.Harness, run.ThreadID = session.Harness, session.ThreadID
	}
	if run.Role == "" || run.SandboxID == "" {
		return nil, fmt.Errorf("delivery candidate AgentRun %s has an incomplete execution envelope", run.ID)
	}
	if run.ThreadID != "" && (run.Harness != session.Harness || run.ThreadID != session.ThreadID) {
		return nil, fmt.Errorf("AgentRun %s conflicts with Session %s Thread", run.ID, sessionID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &core.AgentMessageWork{MessageID: message.ID, SandboxID: run.SandboxID}, nil
}

func (s Store) HasImmediatelyEligibleAgentMessage(ctx context.Context, sessionID string) (bool, error) {
	q := dbsql.New(s.DB)
	selected, err := q.NextAgentMessage(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	run, err := q.GetAgentRunByMessage(ctx, selected.ID)
	if err != nil {
		return false, err
	}
	return run.State == core.AgentRunPending, nil
}

func (s Store) PrepareAgentRun(ctx context.Context, runID, harness, baselineTurnID string) error {
	if strings.TrimSpace(harness) == "" {
		return fmt.Errorf("AgentRun preparation requires a harness")
	}
	queries := dbsql.New(s.DB)
	rows, err := queries.PrepareAgentRun(ctx, dbsql.PrepareAgentRunParams{Harness: sql.NullString{String: harness, Valid: true}, BaselineTurnID: sql.NullString{String: baselineTurnID, Valid: true}, RunID: runID})
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	prepared, err := queries.GetAgentRunPreparation(ctx, runID)
	if err != nil {
		return err
	}
	if prepared.Harness != harness || !prepared.Recorded || prepared.BaselineTurnID != baselineTurnID {
		return fmt.Errorf("AgentRun %s harness baseline conflicts with durable baseline", runID)
	}
	return nil
}

func (s Store) BindAgentRun(ctx context.Context, runID, harness, threadID, turnID, status string) error {
	if strings.TrimSpace(harness) == "" || strings.TrimSpace(threadID) == "" || strings.TrimSpace(turnID) == "" {
		return fmt.Errorf("AgentRun binding requires harness, Thread ID, and Turn ID")
	}
	state := core.AgentRunActive
	outcome := ""
	attention := ""
	if status == "completed" {
		state, outcome = core.AgentRunCompleted, status
	} else if status == "failed" {
		state, outcome = core.AgentRunFailed, status
	} else if status == "interrupted" {
		state, outcome = core.AgentRunInterrupted, status
	} else if status != "running" && status != "inProgress" {
		state = core.AgentRunUncertain
		attention = fmt.Sprintf("harness Turn %s has unsupported status %q", turnID, status)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	// Lock the Session before the run, matching admission and delivery selection.
	// The binding and accepted Turn commit together, including on recovery.
	if err := expectOneRows(queries.BindSessionThread(ctx, dbsql.BindSessionThreadParams{RunID: runID, Harness: harness, ThreadID: nullableString(threadID)})); err != nil {
		return fmt.Errorf("AgentRun %s cannot bind the Session Thread: %w", runID, err)
	}
	run, err := queries.GetAgentRunForBinding(ctx, runID)
	if err != nil {
		return err
	}
	if run.Harness != "" && run.Harness != harness || run.ThreadID != "" && run.ThreadID != threadID || run.TurnID != "" && run.TurnID != turnID {
		return fmt.Errorf("AgentRun %s harness Thread/Turn binding conflicts with its durable identity", runID)
	}
	if run.State == core.AgentRunCompleted || run.State == core.AgentRunFailed || run.State == core.AgentRunInterrupted {
		if run.State != state || run.TurnOutcome != outcome || run.Harness == "" || run.ThreadID == "" || run.TurnID == "" {
			return fmt.Errorf("AgentRun %s terminal outcome conflicts with observed harness status %q", runID, status)
		}
		return tx.Commit()
	}
	if run.State == core.AgentRunPending {
		return fmt.Errorf("AgentRun %s must be prepared before binding a harness Turn", runID)
	}
	if err := expectOneRows(queries.BindAgentRunIdentity(ctx, dbsql.BindAgentRunIdentityParams{Harness: sql.NullString{String: harness, Valid: true}, ThreadID: sql.NullString{String: threadID, Valid: true}, RunID: runID})); err != nil {
		return err
	}
	if err := expectOneRows(queries.BindHarnessTurn(ctx, dbsql.BindHarnessTurnParams{TurnID: sql.NullString{String: turnID, Valid: true}, State: state, TurnOutcome: outcome, Attention: attention, RunID: runID, Harness: sql.NullString{String: harness, Valid: true}, ThreadID: sql.NullString{String: threadID, Valid: true}})); err != nil {
		return err
	}
	if outcome != "" {
		if err := queries.PropagateTurnOutcomeToSteers(ctx, dbsql.PropagateTurnOutcomeToSteersParams{TurnOutcome: sql.NullString{String: outcome, Valid: true}, RunID: runID, TurnID: sql.NullString{String: turnID, Valid: true}}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s Store) BindSteer(ctx context.Context, runID, turnID, status string) error {
	outcome := ""
	if status == "completed" || status == "failed" || status == "interrupted" {
		outcome = status
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	bound, err := queries.BindSteer(ctx, dbsql.BindSteerParams{TurnID: sql.NullString{String: turnID, Valid: true}, TurnOutcome: outcome, RunID: runID})
	if err != nil {
		return err
	}
	if outcome != "" && bound != outcome {
		return fmt.Errorf("AgentRun %s outcome %s conflicts with observed %s", runID, bound, outcome)
	}
	return tx.Commit()
}

func (s Store) RequeueAutoMessageAsFollow(ctx context.Context, runID, targetTurnID, acceptedTurnID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := dbsql.New(s.DB).WithTx(tx).RequeueAutoMessageAsFollow(ctx, dbsql.RequeueAutoMessageAsFollowParams{
		RunID: runID, TargetTurnID: sql.NullString{String: targetTurnID, Valid: true}, AcceptedTurnID: acceptedTurnID,
	})
	if err := expectOneRows(rows, err); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) FailAgentRun(ctx context.Context, runID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).FailAgentRun(ctx, dbsql.FailAgentRunParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: runID}))
}

func (s Store) UncertainAgentRun(ctx context.Context, runID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).MarkAgentRunUncertain(ctx, dbsql.MarkAgentRunUncertainParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: runID}))
}

func (s Store) AgentRunAttention(ctx context.Context, runID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).SetAgentRunAttention(ctx, dbsql.SetAgentRunAttentionParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: runID}))
}

func (s Store) UnsettledAgentMessages(ctx context.Context, sessionID string) ([]core.AgentMessageWork, error) {
	rows, err := dbsql.New(s.DB).ListUnsettledAgentMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages := make([]core.AgentMessageWork, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, core.AgentMessageWork{MessageID: row.MessageID, SandboxID: row.SandboxID})
	}
	return messages, nil
}

func (s Store) SetCleanupAttention(ctx context.Context, sessionID, detail string) error {
	detail = strings.TrimSpace(detail)
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return expectOneRows(dbsql.New(s.DB).SetCleanupAttention(ctx, dbsql.SetCleanupAttentionParams{Detail: detail, SessionID: sessionID}))
}

func (s Store) CompleteCleanup(ctx context.Context, sessionID, taskID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	session, err := queries.GetCleanupSessionForUpdate(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.CurrentTaskID == "" || session.CurrentTaskID != strings.TrimSpace(taskID) {
		return fmt.Errorf("cleanup cannot complete without ownership by the Session's current attached cleanup task")
	}
	tasks, err := queries.ListSessionTasks(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(tasks) == 0 || tasks[len(tasks)-1].TaskID != taskID || tasks[len(tasks)-1].TaskName != core.CleanupTaskName {
		return fmt.Errorf("cleanup cannot complete without the exact Core cleanup task attachment")
	}
	if !session.AdmissionOpen && session.CleanupState == core.CleanupComplete {
		return tx.Commit()
	}
	if session.AdmissionOpen || session.CleanupState != core.CleanupScheduled {
		return fmt.Errorf("cleanup cannot complete while admission or cleanup scheduling remains unsettled")
	}
	deliveries, err := queries.ListDeliveries(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if !delivery.AgentRunPresent {
			return fmt.Errorf("cleanup cannot complete because Message %s has no AgentRun", delivery.MessageID)
		}
		if delivery.AgentRunMessageID != delivery.MessageID || delivery.AgentRunSessionID != delivery.MessageSessionID {
			return fmt.Errorf("cleanup cannot complete because Message %s has a mismatched AgentRun %s", delivery.MessageID, delivery.AgentRunID)
		}
		run := delivery
		if run.State != core.AgentRunCompleted && run.State != core.AgentRunFailed && run.State != core.AgentRunInterrupted {
			return fmt.Errorf("cleanup cannot complete with unsettled AgentRun %s", run.AgentRunID)
		}
	}
	unsettled, err := queries.CountUnsettledSandboxCleanupActions(ctx, sessionID)
	if err != nil {
		return err
	}
	if unsettled != 0 {
		return fmt.Errorf("cleanup cannot complete with %d unsettled Session resources", unsettled)
	}
	if err := expectOneRows(queries.CompleteCleanup(ctx, sessionID)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) Actions(ctx context.Context, sessionID string) ([]core.Action, error) {
	rows, err := dbsql.New(s.DB).ListActions(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var actions []core.Action
	for _, row := range rows {
		actions = append(actions, actionFromValues(row.ID, row.SessionID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt))
	}
	return actions, nil
}

func expectOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return nil
}

func expectOneRows(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return nil
}

func instructionSQL(value *string) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *value, Valid: true}
}
func instructionPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
