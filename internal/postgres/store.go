package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var ErrNotFound = errors.New("Dorf Job not found")
var ErrAdmissionConflict = errors.New("admission key is bound to different complete Job input")
var ErrRevisionObservationSuperseded = errors.New("Revision observation is no longer current; retry derived workflow")
var fullCommitOID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
var sha256Digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var sandboxName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,126}$`)

const (
	AbsurdReleaseCommit = "550d3b9e6f9382d96178de6ab8c90c7f8edf2227"
	AbsurdSchemaURL     = "https://raw.githubusercontent.com/earendil-works/absurd/" + AbsurdReleaseCommit + "/sql/absurd.sql"
	AbsurdSchemaSHA256  = "d34309370c539f3a51f2b36b69b1f77551f8e4a14480a1c8def8bb8f40fd9aab"
	initialFromID       = "dorf:initial"
)

var dorfMigrations = []string{"001_greenfield.sql"}

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

func ValidRevision(value string) bool { return fullCommitOID.MatchString(value) }

func investigationSourceParams(jobID string, source investigation.Source) dbsql.InsertCodebaseInvestigationSourceParams {
	return dbsql.InsertCodebaseInvestigationSourceParams{
		JobID: jobID, Repository: source.Repository, Revision: source.Revision,
	}
}

func investigationSourceFromValues(jobID, repository, revision string) investigation.Source {
	return investigation.Source{JobID: jobID, Repository: repository, Revision: revision}
}

type admittedAgentRun struct {
	Role          string
	Capability    string
	InputRevision string
	SandboxID     string
}

type messageEnvelopeResolver func(context.Context, *dbsql.Queries, dbsql.GetJobAdmissionForUpdateRow, core.MessageAdmission) (admittedAgentRun, error)

func (s Store) AdmitCodingMessage(ctx context.Context, input core.MessageAdmission) (core.MessageAdmissionResult, error) {
	return s.admitMessage(ctx, input, coding.Workflow, coding.WorkflowRevision, resolveCodingMessageEnvelope)
}

func (s Store) AdmitInvestigationMessage(ctx context.Context, input core.MessageAdmission) (core.MessageAdmissionResult, error) {
	return s.admitMessage(ctx, input, investigation.Workflow, investigation.WorkflowRevision, resolveInvestigationMessageEnvelope)
}

func (s Store) admitMessage(ctx context.Context, input core.MessageAdmission, workflow core.WorkflowName, revision string, resolveEnvelope messageEnvelopeResolver) (core.MessageAdmissionResult, error) {
	input, err := normalizeMessage(input)
	if err != nil {
		return core.MessageAdmissionResult{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.MessageAdmissionResult{}, err
	}
	defer tx.Rollback()
	message, created, err := admitMessageTx(ctx, tx, input, workflow, revision, resolveEnvelope)
	if err != nil {
		return core.MessageAdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.MessageAdmissionResult{}, err
	}
	return core.MessageAdmissionResult{Message: message, SandboxID: input.SandboxID, Created: created}, nil
}

func normalizeMessage(input core.MessageAdmission) (core.MessageAdmission, error) {
	input.JobID = strings.TrimSpace(input.JobID)
	input.SandboxID = strings.TrimSpace(input.SandboxID)
	input.FromKind = core.MessageFromKind(strings.TrimSpace(string(input.FromKind)))
	input.FromID = strings.TrimSpace(input.FromID)
	if input.FromKind == "" {
		input.FromKind = core.MessageFromHuman
	}
	if input.Intent == "" {
		input.Intent = core.MessageFollow
	}
	if input.JobID == "" || input.SandboxID == "" || input.FromID == "" || strings.TrimSpace(input.Input) == "" {
		return core.MessageAdmission{}, fmt.Errorf("message admission requires Job ID, exact Sandbox ID, from ID, and complete input")
	}
	if input.FromKind != core.MessageFromHuman && input.FromKind != core.MessageFromAgent && input.FromKind != core.MessageFromWorkflow {
		return core.MessageAdmission{}, fmt.Errorf("invalid message from kind")
	}
	if len(input.FromID) > 256 {
		return core.MessageAdmission{}, fmt.Errorf("from ID must be at most 256 characters")
	}
	if input.Intent != core.MessageFollow && input.Intent != core.MessageSteer {
		return core.MessageAdmission{}, fmt.Errorf("message intent must be follow or steer")
	}
	if len(input.Input) > 1<<20 {
		return core.MessageAdmission{}, fmt.Errorf("message input exceeds 1 MiB")
	}
	return input, nil
}

func admitMessageTx(ctx context.Context, tx *sql.Tx, input core.MessageAdmission, workflow core.WorkflowName, revision string, resolveEnvelope messageEnvelopeResolver) (core.Message, bool, error) {
	queries := dbsql.New(tx)
	job, err := queries.GetJobAdmissionForUpdate(ctx, input.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.Message{}, false, ErrNotFound
		}
		return core.Message{}, false, err
	}
	if job.WorkflowName != workflow || job.WorkflowRevision != revision {
		consumer := "client-directed"
		if workflow != "" {
			consumer = fmt.Sprintf("%s revision %s", workflow, revision)
		}
		return core.Message{}, false, fmt.Errorf("Job %s is not %s", input.JobID, consumer)
	}
	row, err := queries.GetMessageBySender(ctx, dbsql.GetMessageBySenderParams{JobID: input.JobID, FromKind: input.FromKind, FromID: input.FromID})
	if err == nil {
		message := messageFromValues(row.ID, row.JobID, row.FromKind, row.FromID, row.Sequence, row.Input, row.DeliveryIntent, row.SteerTargetTurnID)
		message.AdmittedAt = row.AdmittedAt
		if message.Input != input.Input || message.Intent != input.Intent {
			return core.Message{}, false, fmt.Errorf("%w: sender %s/%q", core.ErrMessageReplayConflict, input.FromKind, input.FromID)
		}
		run, runErr := queries.GetAgentRunByMessage(ctx, message.ID)
		if runErr != nil {
			return core.Message{}, false, fmt.Errorf("load durable AgentRun for Message replay: %w", runErr)
		}
		if run.JobID != input.JobID || run.MessageID != message.ID || run.SandboxID != input.SandboxID {
			return core.Message{}, false, fmt.Errorf("%w: sender %s/%q changed Sandbox delivery", core.ErrMessageReplayConflict, input.FromKind, input.FromID)
		}
		return message, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.Message{}, false, err
	}
	if !job.AdmissionOpen {
		return core.Message{}, false, fmt.Errorf("%w for Job %s", core.ErrMessageAdmissionClosed, input.JobID)
	}
	if resolveEnvelope == nil {
		return core.Message{}, false, fmt.Errorf("Message execution-envelope resolution is not configured")
	}
	run, err := resolveEnvelope(ctx, queries, job, input)
	if err != nil {
		return core.Message{}, false, err
	}
	if run.Role == "" || run.SandboxID != input.SandboxID {
		return core.Message{}, false, fmt.Errorf("Message execution envelope returned a foreign Sandbox delivery")
	}
	harness, threadID, targetTurnID := "", "", ""
	if input.Intent == core.MessageSteer {
		active, err := queries.GetActiveAgentTurn(ctx, dbsql.GetActiveAgentTurnParams{
			JobID: input.JobID, Role: run.Role, SandboxID: run.SandboxID,
		})
		if errors.Is(err, sql.ErrNoRows) {
			return core.Message{}, false, core.ErrMessageSteerUnavailable
		}
		if err != nil {
			return core.Message{}, false, err
		}
		targetTurnID, harness, threadID = active.TurnID, active.Harness, active.ThreadID
	}
	var message core.Message
	message.TargetTurnID = targetTurnID
	message.Sequence, err = queries.NextMessageSequence(ctx, input.JobID)
	if err != nil {
		return core.Message{}, false, err
	}
	message.ID = core.MessageID(input.JobID, input.FromKind, input.FromID)
	message.JobID, message.FromKind, message.FromID, message.Input, message.Intent = input.JobID, input.FromKind, input.FromID, input.Input, input.Intent
	if err := queries.InsertMessage(ctx, dbsql.InsertMessageParams{ID: message.ID, JobID: message.JobID, FromKind: message.FromKind, FromID: message.FromID, Sequence: message.Sequence, Input: message.Input, DeliveryIntent: message.Intent, SteerTargetTurnID: message.TargetTurnID}); err != nil {
		return core.Message{}, false, err
	}
	runID := core.AgentRunID(message.ID)
	rows, err := queries.InsertAdmittedAgentRun(ctx, dbsql.InsertAdmittedAgentRunParams{
		ID: runID, JobID: message.JobID, MessageID: message.ID,
		Harness: nullableString(harness), ThreadID: nullableString(threadID),
		Role: run.Role, InputRevision: nullableString(run.InputRevision),
		Capability: nullableString(run.Capability), SandboxID: run.SandboxID,
	})
	if err := expectOneRows(rows, err); err != nil {
		return core.Message{}, false, fmt.Errorf("insert %s execution-envelope AgentRun: %w", run.Role, err)
	}
	storedMessage, err := queries.GetMessageBySender(ctx, dbsql.GetMessageBySenderParams{JobID: message.JobID, FromKind: message.FromKind, FromID: message.FromID})
	if err != nil {
		return core.Message{}, false, err
	}
	message.AdmittedAt = storedMessage.AdmittedAt
	return message, true, nil
}

func allocateMessageSequenceTx(ctx context.Context, tx *sql.Tx, jobID string) (int64, error) {
	return dbsql.New(tx).NextMessageSequence(ctx, jobID)
}

func ensureInputsTerminalForWorkflowTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	row, err := dbsql.New(tx).GetFirstUnsettledInput(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	state := string(row.State)
	if row.Attention != "" {
		state += ": " + row.Attention
	}
	return fmt.Errorf("FIFO sequence %d has not reached a terminal harness delivery (%s)", row.Sequence, state)
}

func (s Store) Job(ctx context.Context, id string) (core.Job, error) {
	row, err := dbsql.New(s.DB).GetJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Job{}, ErrNotFound
	}
	if err != nil {
		return core.Job{}, err
	}
	return core.Job{
		ID: row.ID, AdmissionKey: row.AdmissionKey, Workflow: core.WorkflowName(row.WorkflowName), WorkflowRevision: row.WorkflowRevision,
		Goal:           row.Goal,
		SandboxProfile: row.SandboxProfile, ProviderConnection: row.ProviderConnection,
		Model: row.Model, ReasoningEffort: row.ReasoningEffort, AdmissionOpen: row.AdmissionOpen, CleanupState: core.CleanupState(row.CleanupState),
		CurrentTaskID:     row.CurrentTaskID,
		WorkflowAttention: row.WorkflowAttention, WorkflowAttentionSource: row.WorkflowAttentionSource,
		WorkflowAttentionAt: timeValue(row.WorkflowAttentionAt), CleanupAttention: row.CleanupAttention,
		AdmittedAt: row.AdmittedAt, CleanedAt: timeValue(row.CleanedAt),
	}, nil
}

func (s Store) JobExists(ctx context.Context, id string) (bool, error) {
	_, err := s.Job(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s Store) CodingJob(ctx context.Context, id string) (coding.Job, error) {
	row, err := dbsql.New(s.DB).GetCodingJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return coding.Job{}, ErrNotFound
	}
	if err != nil {
		return coding.Job{}, err
	}
	return coding.Job{
		Job: core.Job{
			ID: row.ID, AdmissionKey: row.AdmissionKey, Workflow: core.WorkflowName(row.WorkflowName), WorkflowRevision: row.WorkflowRevision,
			Goal: row.Goal, SandboxProfile: row.SandboxProfile, ProviderConnection: row.ProviderConnection,
			Model: row.Model, ReasoningEffort: row.ReasoningEffort, AdmissionOpen: row.AdmissionOpen, CleanupState: core.CleanupState(row.CleanupState),
			CurrentTaskID: row.CurrentTaskID, WorkflowAttention: row.WorkflowAttention, WorkflowAttentionSource: row.WorkflowAttentionSource,
			WorkflowAttentionAt: timeValue(row.WorkflowAttentionAt), CleanupAttention: row.CleanupAttention,
			AdmittedAt: row.AdmittedAt, CleanedAt: timeValue(row.CleanedAt),
		},
		Repository: row.Repository, StartingRevision: row.StartingRevision, Revision: row.Revision, Branch: row.Branch,
		GitHubRepository: row.GithubRepository, GitHubInstallation: row.GithubInstallationID, BaseBranch: row.BaseBranch,
	}, nil
}

func (s Store) JobTasks(ctx context.Context, jobID string) ([]core.JobTask, error) {
	rows, err := dbsql.New(s.DB).ListJobTasks(ctx, jobID)
	if err != nil {
		return nil, err
	}
	tasks := make([]core.JobTask, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, core.JobTask{
			JobID: row.JobID, Sequence: row.Sequence, TaskID: row.TaskID,
			TaskName: row.TaskName, AttachedAt: row.AttachedAt,
		})
	}
	return tasks, nil
}

func (s Store) Revisions(ctx context.Context, jobID string) ([]coding.Revision, error) {
	rows, err := dbsql.New(s.DB).ListRevisions(ctx, jobID)
	if err != nil {
		return nil, err
	}
	revisions := make([]coding.Revision, 0, len(rows))
	for _, row := range rows {
		revisions = append(revisions, coding.Revision{
			JobID: row.JobID, OID: row.OID, ComparisonBase: row.ComparisonBaseOID,
			Tree: row.TreeOID, Branch: row.Branch, Generation: int(row.Generation),
			EvidenceID: row.EvidenceID, ObservedAt: row.ObservedAt,
		})
	}
	return revisions, nil
}

// WithJobFence serializes harness and other external mutation for one Job
// independently of an expiring Absurd claim. Message admission intentionally
// does not take this long-lived fence.
func (s Store) WithJobFence(ctx context.Context, jobID string, fn func() error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := acquireJobFenceTx(ctx, tx, jobID); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return tx.Commit()
}

func acquireJobFenceTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock(hashtextextended('dorf-job-effect:' || $1, 0))`, jobID); err != nil {
		return fmt.Errorf("acquire Job execution fence: %w", err)
	}
	return nil
}

// AttachJobTask appends one exact Absurd task handoff. The deterministic Absurd
// idempotency key supplies task identity; Dorf records only ordered attachment.
func (s Store) AttachJobTask(ctx context.Context, jobID, expectedCurrentTaskID, taskID, taskName string) error {
	return s.attachJobTask(ctx, jobID, expectedCurrentTaskID, taskID, taskName, false)
}

func messageFromValues(id, jobID string, fromKind core.MessageFromKind, fromID string, sequence int64, input string, intent core.MessageDeliveryIntent, targetTurnID string) core.Message {
	return core.Message{ID: id, JobID: jobID, FromKind: fromKind, FromID: fromID, Sequence: sequence, Input: input, Intent: intent, TargetTurnID: targetTurnID}
}

func actionFromValues(id, jobID string, kind core.ActionKind, state core.ActionState, scope string, createdAt time.Time, settledAt sql.NullTime) core.Action {
	return core.Action{ID: id, JobID: jobID, Kind: kind, State: state, Scope: scope, CreatedAt: createdAt, SettledAt: timeValue(settledAt)}
}

func exactScopedAction(row dbsql.DorfAction, jobID string, kind core.ActionKind, scope string) (core.Action, error) {
	expectedID := core.ScopedActionID(jobID, kind, scope)
	if row.ID != expectedID || row.JobID != jobID || row.Kind != kind || row.ScopeKey != scope {
		return core.Action{}, fmt.Errorf("Action %s conflicts with exact Job %s, kind %s, and scope %s", row.ID, jobID, kind, scope)
	}
	return actionFromValues(row.ID, row.JobID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt), nil
}

func agentRunFromValues(id, jobID, messageID string, state core.AgentRunState, harness, threadID string, baselineRecorded bool, baselineTurnID, turnID, turnOutcome, attention, role, inputRevision string) core.AgentRun {
	return core.AgentRun{ID: id, JobID: jobID, MessageID: messageID, Harness: harness, ThreadID: threadID, State: state, BaselineRecorded: baselineRecorded, BaselineTurnID: baselineTurnID, TurnID: turnID, TurnOutcome: turnOutcome, Attention: attention, Role: role, InputRevision: inputRevision}
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

func (s Store) RequestCleanup(ctx context.Context, jobID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	if _, err := queries.GetCleanupJobForUpdate(ctx, jobID); err != nil {
		return err
	}
	closed, err := queries.RequestCleanup(ctx, jobID)
	if err != nil {
		return err
	}
	if closed != 1 {
		return fmt.Errorf("Job %s cannot record cleanup request from its current state", jobID)
	}
	return tx.Commit()
}

func (s Store) CleanupRequests(ctx context.Context) ([]string, error) {
	return dbsql.New(s.DB).ListCleanupRequests(ctx)
}

func (s Store) AttachCleanupTask(ctx context.Context, jobID, expectedCurrentTaskID, taskID, taskName string) error {
	return s.attachJobTask(ctx, jobID, expectedCurrentTaskID, taskID, taskName, true)
}

func (s Store) attachJobTask(ctx context.Context, jobID, expectedCurrentTaskID, taskID, taskName string, cleanup bool) error {
	jobID = strings.TrimSpace(jobID)
	expectedCurrentTaskID = strings.TrimSpace(expectedCurrentTaskID)
	taskID = strings.TrimSpace(taskID)
	taskName = strings.TrimSpace(taskName)
	if jobID == "" || taskID == "" || taskName == "" {
		return fmt.Errorf("Job task attachment requires exact Job, task, and task-name identities")
	}
	if cleanup && taskName != core.CleanupTaskName {
		return fmt.Errorf("Job cleanup task must use Core task name %s", core.CleanupTaskName)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	current, err := queries.GetCurrentJobTaskForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if cleanup {
		if current.AdmissionOpen || (current.CleanupState != core.CleanupRequested && current.CleanupState != core.CleanupScheduled) {
			return fmt.Errorf("Job %s cannot attach cleanup from state %s", jobID, current.CleanupState)
		}
		if current.CleanupState == core.CleanupScheduled && current.TaskID != taskID {
			return fmt.Errorf("Job %s already has cleanup task %s", jobID, current.TaskID)
		}
	} else if !current.AdmissionOpen || current.CleanupState != core.CleanupPending {
		return fmt.Errorf("Job %s cannot attach ordinary task after cleanup begins", jobID)
	}
	if current.TaskID == taskID {
		if current.TaskName != taskName {
			return fmt.Errorf("Absurd task %s is already attached as %s", taskID, current.TaskName)
		}
	} else {
		if current.TaskID != expectedCurrentTaskID {
			return fmt.Errorf("Job %s current task is %q, not expected predecessor %q", jobID, current.TaskID, expectedCurrentTaskID)
		}
		inserted, err := queries.InsertJobTask(ctx, dbsql.InsertJobTaskParams{
			JobID: jobID, Sequence: current.Sequence + 1, TaskID: taskID, TaskName: taskName,
		})
		if err != nil {
			return err
		}
		if inserted != 1 {
			return fmt.Errorf("Absurd task %s is already attached to another Job", taskID)
		}
	}
	if cleanup && current.CleanupState == core.CleanupRequested {
		updated, err := queries.MarkCleanupScheduled(ctx, jobID)
		if err != nil {
			return err
		}
		if updated != 1 {
			return fmt.Errorf("Job %s cleanup scheduling did not settle", jobID)
		}
	}
	return tx.Commit()
}

func (s Store) GetOrCreateSandboxAction(ctx context.Context, sandboxID string, kind core.ActionKind) (core.Action, error) {
	sandbox, err := dbsql.New(s.DB).GetSandbox(ctx, sandboxID)
	if err != nil {
		return core.Action{}, err
	}
	id := core.ScopedActionID(sandbox.JobID, kind, sandboxID)
	q := dbsql.New(s.DB)
	insertErr := expectOneRows(q.InsertScopedAction(ctx, dbsql.InsertScopedActionParams{ID: id, JobID: sandbox.JobID, Kind: kind, ScopeKey: sandboxID}))
	row, getErr := q.GetScopedAction(ctx, dbsql.GetScopedActionParams{JobID: sandbox.JobID, Kind: kind, ScopeKey: sandboxID})
	if getErr != nil {
		if insertErr != nil {
			return core.Action{}, insertErr
		}
		return core.Action{}, getErr
	}
	return exactScopedAction(row, sandbox.JobID, kind, sandboxID)
}

func (s Store) Sandbox(ctx context.Context, id string) (core.Sandbox, error) {
	row, err := dbsql.New(s.DB).GetSandbox(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Sandbox{}, ErrNotFound
	}
	if err != nil {
		return core.Sandbox{}, err
	}
	return core.Sandbox{ID: row.ID, JobID: row.JobID, Name: row.Name, OwnershipNonce: row.OwnershipNonce}, nil
}
func (s Store) Sandboxes(ctx context.Context, jobID string) ([]core.Sandbox, error) {
	rows, err := dbsql.New(s.DB).ListJobSandboxes(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]core.Sandbox, 0, len(rows))
	for _, r := range rows {
		out = append(out, core.Sandbox{ID: r.ID, JobID: r.JobID, Name: r.Name, OwnershipNonce: r.OwnershipNonce})
	}
	return out, nil
}

// EnsureSandbox durably reserves one stable logical Sandbox identity. Provider
// reconciliation is deliberately outside this transaction and is protected by
// the Job effect fence plus the Sandbox's stable Action.
func (s Store) EnsureSandbox(ctx context.Context, jobID, name string) (core.Sandbox, error) {
	jobID = strings.TrimSpace(jobID)
	name = strings.TrimSpace(name)
	if name == "" {
		name = core.DefaultSandbox
	}
	if jobID == "" || !sandboxName.MatchString(name) {
		return core.Sandbox{}, fmt.Errorf("Sandbox ensure requires a Job and a lowercase name containing only letters, digits, and hyphens")
	}
	id := core.NamedSandboxID(jobID, name)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.Sandbox{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	job, err := queries.GetJobForSandboxEnsure(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Sandbox{}, ErrNotFound
	}
	if err != nil {
		return core.Sandbox{}, err
	}
	if !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		return core.Sandbox{}, fmt.Errorf("Job %s cannot admit Sandbox %q after cleanup begins", jobID, name)
	}
	row, err := queries.GetJobSandboxByNameForUpdate(ctx, dbsql.GetJobSandboxByNameForUpdateParams{JobID: jobID, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		foreign, foreignErr := queries.GetSandboxForUpdate(ctx, id)
		if foreignErr == nil {
			return core.Sandbox{}, fmt.Errorf("Sandbox identity %s is already owned by Job %s as name %q", id, foreign.JobID, foreign.Name)
		}
		if !errors.Is(foreignErr, sql.ErrNoRows) {
			return core.Sandbox{}, foreignErr
		}
		nonce, nonceErr := reviewNonce()
		if nonceErr != nil {
			return core.Sandbox{}, nonceErr
		}
		inserted, insertErr := queries.ReserveSandbox(ctx, dbsql.ReserveSandboxParams{ID: id, JobID: jobID, Name: name, OwnershipNonce: nonce})
		if insertErr != nil {
			return core.Sandbox{}, insertErr
		}
		if inserted != 1 {
			return core.Sandbox{}, fmt.Errorf("Sandbox %q conflicts with an existing durable resource", name)
		}
		row, err = queries.GetJobSandboxByNameForUpdate(ctx, dbsql.GetJobSandboxByNameForUpdateParams{JobID: jobID, Name: name})
	}
	if err != nil {
		return core.Sandbox{}, err
	}
	if row.ID != id || row.JobID != jobID || row.Name != name || !sha256Digest.MatchString(row.OwnershipNonce) {
		return core.Sandbox{}, fmt.Errorf("Sandbox %q conflicts with its exact Job-owned identity", name)
	}
	if err := tx.Commit(); err != nil {
		return core.Sandbox{}, err
	}
	return core.Sandbox{ID: row.ID, JobID: row.JobID, Name: row.Name, OwnershipNonce: row.OwnershipNonce}, nil
}

func (s Store) Deliveries(ctx context.Context, jobID string) ([]core.Delivery, error) {
	rows, err := dbsql.New(s.DB).ListDeliveries(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]core.Delivery, 0, len(rows))
	for _, r := range rows {
		if !r.AgentRunPresent {
			return nil, fmt.Errorf("Message %s (sequence %d) has no AgentRun", r.MessageID, r.Sequence)
		}
		if r.AgentRunMessageID != r.MessageID || r.AgentRunJobID != r.MessageJobID {
			return nil, fmt.Errorf("Message %s (Job %s) has mismatched AgentRun %s (Message %s, Job %s)", r.MessageID, r.MessageJobID, r.AgentRunID, r.AgentRunMessageID, r.AgentRunJobID)
		}
		message := messageFromValues(r.MessageID, r.MessageJobID, r.FromKind, r.FromID, r.Sequence, r.Input, r.DeliveryIntent, r.SteerTargetTurnID)
		message.AdmittedAt = r.AdmittedAt
		run := agentRunFromValues(r.AgentRunID, r.AgentRunJobID, r.AgentRunMessageID, r.State, r.Harness, r.ThreadID, r.BaselineRecorded, r.BaselineTurnID, r.TurnID, r.TurnOutcome, r.Attention, r.Role, r.InputRevision)
		run.Capability = r.Capability
		run.SandboxID = r.SandboxID
		run.SubmissionNonce = r.SubmissionNonce
		run.StartedAt = timeValue(r.StartedAt)
		run.FinishedAt = timeValue(r.FinishedAt)
		out = append(out, core.Delivery{Message: message, AgentRun: run})
	}
	return out, nil
}

// CodingMessages converts Core's internal delivery facts into the
// coding-owned read projection. Ordinary workflow coordination never receives
// raw AgentRun lifecycle state, Thread, or Turn identities.
func (s Store) CodingMessages(ctx context.Context, jobID string) ([]coding.MessageRecord, []coding.ReviewRunView, error) {
	deliveries, err := s.Deliveries(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	sandboxes, err := s.Sandboxes(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	owned := make(map[string]core.Sandbox, len(sandboxes))
	for _, sandbox := range sandboxes {
		owned[sandbox.ID] = sandbox
	}
	messages := make([]coding.MessageRecord, 0, len(deliveries))
	reviews := make([]coding.ReviewRunView, 0)
	for _, delivery := range deliveries {
		run, message := delivery.AgentRun, delivery.Message
		outcome := agentRunOutcome(run.State, run.TurnOutcome)
		if run.Role == "implement" {
			messages = append(messages, coding.MessageRecord{
				Message: message, SandboxID: run.SandboxID, InputRevision: run.InputRevision,
				ProducerID: run.ID, Outcome: outcome, Attention: run.Attention,
				StartsTurn: message.Intent == core.MessageFollow,
			})
			continue
		}
		sandbox, ok := owned[run.SandboxID]
		if !ok || sandbox.JobID != run.JobID {
			return nil, nil, fmt.Errorf("review producer %s has no exact Job-owned Sandbox %s", run.ID, run.SandboxID)
		}
		reviews = append(reviews, coding.ReviewRunView{
			ID: run.ID, JobID: run.JobID, MessageID: run.MessageID, Harness: run.Harness,
			ThreadID: run.ThreadID, TurnID: run.TurnID, Outcome: outcome, Attention: run.Attention,
			Role: run.Role, InputRevision: run.InputRevision, Capability: run.Capability,
			SandboxID: run.SandboxID, SubmissionNonce: run.SubmissionNonce,
			StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, Request: message, Sandbox: sandbox,
		})
	}
	return messages, reviews, nil
}

// AgentMessageExecution reloads the exact durable execution aggregate by the
// stable Message identity. Callers that may touch the Harness invoke this only
// while holding the owning Job's effect fence and discard earlier snapshots.
func (s Store) AgentMessageExecution(ctx context.Context, messageID string) (core.AgentMessageExecution, error) {
	queries := dbsql.New(s.DB)
	messageRow, err := queries.GetMessage(ctx, messageID)
	if err != nil {
		return core.AgentMessageExecution{}, err
	}
	message := messageFromValues(messageRow.ID, messageRow.JobID, messageRow.FromKind, messageRow.FromID, messageRow.Sequence, messageRow.Input, messageRow.DeliveryIntent, messageRow.SteerTargetTurnID)
	message.AdmittedAt = messageRow.AdmittedAt
	runRow, err := queries.GetAgentRunByMessage(ctx, message.ID)
	if err != nil {
		return core.AgentMessageExecution{}, fmt.Errorf("Message %s has no atomically admitted AgentRun: %w", message.ID, err)
	}
	run := agentRunFromValues(runRow.ID, runRow.JobID, runRow.MessageID, runRow.State, runRow.Harness, runRow.ThreadID, runRow.BaselineRecorded, runRow.BaselineTurnID, runRow.TurnID, runRow.TurnOutcome, runRow.Attention, runRow.Role, runRow.InputRevision)
	run.Capability = runRow.Capability
	run.SandboxID = runRow.SandboxID
	run.SubmissionNonce = runRow.SubmissionNonce
	run.StartedAt = timeValue(runRow.StartedAt)
	run.FinishedAt = timeValue(runRow.FinishedAt)
	job, err := s.Job(ctx, message.JobID)
	if err != nil {
		return core.AgentMessageExecution{}, err
	}
	sandbox, err := s.Sandbox(ctx, run.SandboxID)
	if err != nil {
		return core.AgentMessageExecution{}, err
	}
	if run.MessageID != message.ID || run.JobID != job.ID || message.JobID != job.ID || sandbox.JobID != job.ID || run.SandboxID != sandbox.ID {
		return core.AgentMessageExecution{}, fmt.Errorf("Message %s execution does not match its authoritative Job, AgentRun, and Sandbox", message.ID)
	}
	return core.AgentMessageExecution{Job: job, Message: message, AgentRun: run, Sandbox: sandbox}, nil
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

func revisionCandidateTx(ctx context.Context, tx *sql.Tx, jobID string) (core.AgentRun, bool, error) {
	queries := dbsql.New(tx)
	unsettled, err := queries.CountUnsettledInputs(ctx, jobID)
	if err != nil {
		return core.AgentRun{}, false, err
	}
	if unsettled != 0 {
		return core.AgentRun{}, false, nil
	}
	latestInput, err := queries.GetLatestAgentRun(ctx, dbsql.GetLatestAgentRunParams{JobID: jobID, Role: coding.InitialAgentRole})
	if errors.Is(err, sql.ErrNoRows) {
		return core.AgentRun{}, false, nil
	}
	if err != nil {
		return core.AgentRun{}, false, err
	}
	if latestInput.State != core.AgentRunCompleted {
		return core.AgentRun{}, false, nil
	}
	row, err := queries.GetLatestTurnStartRun(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AgentRun{}, false, nil
	}
	if err != nil {
		return core.AgentRun{}, false, err
	}
	run := core.AgentRun{ID: row.ID, JobID: row.JobID, State: row.State, Role: row.Role, InputRevision: row.InputRevision}
	if row.Observed {
		return core.AgentRun{}, false, nil
	}
	if run.State != core.AgentRunCompleted || run.Role != "implement" {
		return core.AgentRun{}, false, nil
	}
	return run, true, nil
}

func insertEvidence(ctx context.Context, tx *sql.Tx, jobID string, evidence core.Evidence) error {
	queries := dbsql.New(tx)
	err := queries.InsertEvidence(ctx, dbsql.InsertEvidenceParams{
		ID: evidence.ID, JobID: jobID, Digest: evidence.Digest, ByteSize: evidence.ByteSize,
		MediaType: evidence.MediaType, Producer: evidence.Producer,
		Kind: evidence.Kind, ActionID: evidence.ActionID, AgentRunID: evidence.AgentRunID, Revision: evidence.Revision,
		StartedAt: nullableTime(evidence.StartedAt), FinishedAt: nullableTime(evidence.FinishedAt),
	})
	if err != nil {
		return err
	}
	stored, err := queries.GetEvidenceIdentity(ctx, evidence.ID)
	if err != nil {
		return err
	}
	if stored.JobID != jobID || stored.Digest != evidence.Digest || stored.ByteSize != evidence.ByteSize || stored.MediaType != evidence.MediaType || stored.Producer != evidence.Producer || stored.Kind != evidence.Kind || stored.ActionID != evidence.ActionID || stored.AgentRunID != evidence.AgentRunID || stored.Revision != evidence.Revision || !stored.StartedAt.Equal(evidence.StartedAt) || !stored.FinishedAt.Equal(evidence.FinishedAt) {
		return fmt.Errorf("Evidence identity %s conflicts with immutable retained metadata or content", evidence.ID)
	}
	return nil
}

func nullableTime(value time.Time) sql.NullTime {
	if value.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: value, Valid: true}
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func (s Store) SetWorkflowAttention(ctx context.Context, jobID, source, detail string) error {
	source, detail = strings.TrimSpace(source), strings.TrimSpace(detail)
	if jobID == "" || source == "" || detail == "" {
		return fmt.Errorf("workflow attention requires Job ID, exact source, and detail")
	}
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return expectOneRows(dbsql.New(s.DB).SetWorkflowAttention(ctx, dbsql.SetWorkflowAttentionParams{JobID: jobID, Source: sql.NullString{String: source, Valid: true}, Detail: sql.NullString{String: detail, Valid: true}}))
}

func (s Store) ClearWorkflowAttention(ctx context.Context, jobID, source string) error {
	source = strings.TrimSpace(source)
	if jobID == "" || source == "" {
		return fmt.Errorf("workflow attention clearing requires Job ID and exact source")
	}
	rows, err := dbsql.New(s.DB).ClearWorkflowAttention(ctx, dbsql.ClearWorkflowAttentionParams{
		JobID: jobID, Source: sql.NullString{String: source, Valid: true},
	})
	if err != nil {
		return err
	}
	if rows > 1 {
		return fmt.Errorf("workflow attention source %s changed %d Jobs", source, rows)
	}
	return nil
}

func (s Store) RecordRevisionObservation(ctx context.Context, jobID, runID string, observation gitworkspace.Observation, evidence core.Evidence) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetRevisionJobForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if evidence.ID != core.EvidenceID(runID, "git-revision") || evidence.ActionID != "" || evidence.AgentRunID != runID || evidence.Kind != "git-revision" || evidence.Revision != observation.Revision ||
		!ValidRevision(observation.ComparisonBase) || !ValidRevision(observation.Revision) || !ValidRevision(observation.Tree) {
		return fmt.Errorf("Git Revision observation conflicts with durable comparison base, branch, AgentRun, or Evidence")
	}
	if _, err := queries.GetEvidenceIdentity(ctx, evidence.ID); err == nil {
		if err := insertEvidence(ctx, tx, jobID, evidence); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !locked.AdmissionOpen || locked.OutcomeExists {
		return fmt.Errorf("%w: admission is closed or the Job has an Outcome", ErrRevisionObservationSuperseded)
	}
	if locked.Revision != observation.ComparisonBase {
		return fmt.Errorf("%w: comparison base %s is not current Revision %s", ErrRevisionObservationSuperseded, observation.ComparisonBase, locked.Revision)
	}
	if locked.Branch != observation.Branch {
		return fmt.Errorf("Git Revision observation branch %s conflicts with Job branch %s", observation.Branch, locked.Branch)
	}
	candidate, ready, err := revisionCandidateTx(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if !ready || candidate.ID != runID || candidate.InputRevision != observation.ComparisonBase {
		return fmt.Errorf("%w: AgentRun %s no longer owns the latest completed implementation turn", ErrRevisionObservationSuperseded, runID)
	}
	if err := insertEvidence(ctx, tx, jobID, evidence); err != nil {
		return err
	}
	if observation.Revision != observation.ComparisonBase {
		generation, err := queries.NextRevisionGeneration(ctx, jobID)
		if err != nil {
			return err
		}
		if err := queries.InsertRevision(ctx, dbsql.InsertRevisionParams{
			JobID: jobID, OID: observation.Revision, ComparisonBaseOID: observation.ComparisonBase,
			TreeOID: observation.Tree, Branch: observation.Branch, Generation: generation, EvidenceID: evidence.ID,
		}); err != nil {
			return err
		}
		updated, err := queries.AdvanceJobRevision(ctx, dbsql.AdvanceJobRevisionParams{JobID: jobID, Revision: observation.Revision, ComparisonBaseOID: observation.ComparisonBase})
		if err != nil {
			return err
		}
		if updated != 1 {
			return ErrNotFound
		}
	}
	if _, err := queries.ClearWorkflowAttention(ctx, dbsql.ClearWorkflowAttentionParams{JobID: jobID, Source: sql.NullString{String: runID, Valid: true}}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
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
	if row.ScopeKey == "" || row.ID != core.ScopedActionID(row.JobID, row.Kind, row.ScopeKey) {
		return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox Action %s conflicts with its exact Job and Sandbox", id)
	}
	owned, err := queries.GetSandbox(ctx, row.ScopeKey)
	if err != nil {
		return core.SandboxActionAuthorization{}, err
	}
	if owned.JobID != row.JobID {
		return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox Action %s conflicts with its exact Job and Sandbox", id)
	}
	job, err := queries.GetJobForSandboxActionAuthorization(ctx, row.JobID)
	if err != nil {
		return core.SandboxActionAuthorization{}, err
	}
	if requireTask && (taskID == "" || taskName == "" || job.CurrentTaskID != taskID || job.CurrentTaskName != taskName) {
		return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox Action %s requires the exact current attached task", id)
	}
	cleanup := row.Kind == core.ActionRouteRevoke || row.Kind == core.ActionSandboxDelete
	if cleanup {
		if job.AdmissionOpen || job.CleanupState != core.CleanupScheduled || requireTask && taskName != core.CleanupTaskName {
			return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox cleanup Action %s requires a durably scheduled cleanup", id)
		}
	} else if !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox Action %s cannot mutate provider after cleanup begins", id)
	}
	if row.Kind == core.ActionSandboxDelete {
		revoked, err := queries.GetScopedAction(ctx, dbsql.GetScopedActionParams{JobID: row.JobID, Kind: core.ActionRouteRevoke, ScopeKey: row.ScopeKey})
		if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.State != core.ActionSucceeded) {
			return core.SandboxActionAuthorization{}, fmt.Errorf("Sandbox cleanup cannot delete before its exact route revoke Action succeeds")
		}
		if err != nil {
			return core.SandboxActionAuthorization{}, err
		}
	}
	return core.SandboxActionAuthorization{
		Job: core.Job{
			ID: job.ID, AdmissionKey: job.AdmissionKey, Workflow: job.WorkflowName, WorkflowRevision: job.WorkflowRevision, Goal: job.Goal,
			SandboxProfile: job.SandboxProfile, ProviderConnection: job.ProviderConnection, Model: job.Model, ReasoningEffort: job.ReasoningEffort,
			AdmissionOpen: job.AdmissionOpen, CleanupState: job.CleanupState, CurrentTaskID: job.CurrentTaskID,
			WorkflowAttention: job.WorkflowAttention, WorkflowAttentionSource: job.WorkflowAttentionSource,
			WorkflowAttentionAt: timeValue(job.WorkflowAttentionAt), CleanupAttention: job.CleanupAttention,
			AdmittedAt: job.AdmittedAt, CleanedAt: timeValue(job.CleanedAt),
		},
		Sandbox: core.Sandbox{ID: owned.ID, JobID: owned.JobID, Name: owned.Name, OwnershipNonce: owned.OwnershipNonce},
		Action:  actionFromValues(row.ID, row.JobID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt),
		TaskID:  job.CurrentTaskID, TaskName: job.CurrentTaskName,
	}, nil
}

// AgentMessage selects one opaque Message across the whole Job.
// Steer priority, Follow FIFO, recovery ordering, and retained-Thread adoption
// are invariant for every consumer.
func (s Store) AgentMessage(ctx context.Context, jobID string) (*core.AgentMessageWork, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, fmt.Errorf("Agent Message selection requires an exact Job")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	job, err := queries.GetJobAdmissionForUpdate(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		return nil, nil
	}
	row, err := queries.NextAgentMessage(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	message := core.Message{
		ID: row.ID, JobID: row.JobID, FromKind: core.MessageFromKind(row.FromKind), FromID: row.FromID,
		Sequence: row.Sequence, Input: row.Input, Intent: core.MessageDeliveryIntent(row.DeliveryIntent), TargetTurnID: row.SteerTargetTurnID, AdmittedAt: row.AdmittedAt,
	}
	runRow, err := queries.GetAgentRunByMessage(ctx, message.ID)
	if err != nil {
		return nil, fmt.Errorf("delivery Message %s has no atomically admitted AgentRun: %w", message.ID, err)
	}
	run := agentRunFromValues(runRow.ID, runRow.JobID, runRow.MessageID, runRow.State, runRow.Harness, runRow.ThreadID, runRow.BaselineRecorded, runRow.BaselineTurnID, runRow.TurnID, runRow.TurnOutcome, runRow.Attention, runRow.Role, runRow.InputRevision)
	run.SandboxID = runRow.SandboxID
	if message.Intent == core.MessageFollow && run.State == core.AgentRunPending && run.ThreadID == "" && message.Sequence > 1 {
		if _, err := queries.BindPendingFollowToPriorThread(ctx, message.ID); err != nil {
			return nil, err
		}
		runRow, err = queries.GetAgentRunByMessage(ctx, message.ID)
		if err != nil {
			return nil, err
		}
		run = agentRunFromValues(runRow.ID, runRow.JobID, runRow.MessageID, runRow.State, runRow.Harness, runRow.ThreadID, runRow.BaselineRecorded, runRow.BaselineTurnID, runRow.TurnID, runRow.TurnOutcome, runRow.Attention, runRow.Role, runRow.InputRevision)
		run.SandboxID = runRow.SandboxID
		if run.ThreadID == "" {
			prior, priorErr := queries.GetLatestAgentThreadBinding(ctx, dbsql.GetLatestAgentThreadBindingParams{JobID: jobID, Role: run.Role, SandboxID: run.SandboxID})
			if priorErr == nil && prior.ThreadID != "" {
				return nil, fmt.Errorf("eligible Follow Message %s did not adopt authoritative prior Agent Thread %s", message.ID, prior.ThreadID)
			}
			if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
				return nil, priorErr
			}
		}
	}
	if run.Role == "" || run.SandboxID == "" {
		return nil, fmt.Errorf("delivery candidate AgentRun %s has an incomplete execution envelope", run.ID)
	}
	bindings, err := queries.ListAgentThreadBindings(ctx, dbsql.ListAgentThreadBindingsParams{JobID: jobID, Role: run.Role, SandboxID: run.SandboxID})
	if err != nil {
		return nil, err
	}
	for i, binding := range bindings {
		if i > 0 && (binding.Harness != bindings[0].Harness || binding.ThreadID != bindings[0].ThreadID) ||
			run.ThreadID != "" && (run.Harness != binding.Harness.String || run.ThreadID != binding.ThreadID.String) {
			return nil, fmt.Errorf("Job %s Agent lane %s/%s disagrees on its Harness Thread", jobID, run.Role, run.SandboxID)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &core.AgentMessageWork{MessageID: message.ID, SandboxID: run.SandboxID}, nil
}

// ValidateCodingAgentMessage validates only the static coding execution
// envelope used for prompt composition. Generic selection owns delivery order.
func (s Store) ValidateCodingAgentMessage(ctx context.Context, execution core.AgentMessageExecution) error {
	if execution.Job.Workflow != coding.Workflow || execution.Job.WorkflowRevision != coding.WorkflowRevision ||
		execution.Message.JobID != execution.Job.ID || execution.AgentRun.JobID != execution.Job.ID ||
		execution.AgentRun.MessageID != execution.Message.ID || execution.Sandbox.JobID != execution.Job.ID ||
		execution.AgentRun.SandboxID != execution.Sandbox.ID || execution.Sandbox.ID != core.MainSandboxName(execution.Job.ID) ||
		execution.AgentRun.Role != coding.InitialAgentRole || execution.AgentRun.Capability != "" {
		return fmt.Errorf("Message %s conflicts with the coding execution envelope", execution.Message.ID)
	}
	job, err := s.CodingJob(ctx, execution.Job.ID)
	if err != nil {
		return err
	}
	if execution.AgentRun.InputRevision != job.Revision {
		return fmt.Errorf("Message %s conflicts with the current coding input Revision", execution.Message.ID)
	}
	return nil
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
	bindings, err := queries.ListAgentThreadBindings(ctx, dbsql.ListAgentThreadBindingsParams{JobID: run.JobID, Role: run.Role, SandboxID: run.SandboxID})
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.Harness.String != harness || binding.ThreadID.String != threadID {
			return fmt.Errorf("AgentRun %s conflicts with Job %s Agent lane Thread", runID, run.JobID)
		}
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

func (s Store) FailAgentRun(ctx context.Context, runID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).FailAgentRun(ctx, dbsql.FailAgentRunParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: runID}))
}

func (s Store) UncertainAgentRun(ctx context.Context, runID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).MarkAgentRunUncertain(ctx, dbsql.MarkAgentRunUncertainParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: runID}))
}

func (s Store) AgentRunAttention(ctx context.Context, runID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).SetAgentRunAttention(ctx, dbsql.SetAgentRunAttentionParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: runID}))
}

func (s Store) UnsettledAgentMessages(ctx context.Context, jobID string) ([]core.AgentMessageWork, error) {
	rows, err := dbsql.New(s.DB).ListUnsettledAgentMessages(ctx, jobID)
	if err != nil {
		return nil, err
	}
	messages := make([]core.AgentMessageWork, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, core.AgentMessageWork{MessageID: row.MessageID, SandboxID: row.SandboxID})
	}
	return messages, nil
}

func (s Store) SetCleanupAttention(ctx context.Context, jobID, detail string) error {
	detail = strings.TrimSpace(detail)
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return expectOneRows(dbsql.New(s.DB).SetCleanupAttention(ctx, dbsql.SetCleanupAttentionParams{Detail: detail, JobID: jobID}))
}

func (s Store) CompleteCleanup(ctx context.Context, jobID, taskID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	job, err := queries.GetCleanupJobForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if job.CurrentTaskID == "" || job.CurrentTaskID != strings.TrimSpace(taskID) {
		return fmt.Errorf("cleanup cannot complete without ownership by the Job's current attached cleanup task")
	}
	tasks, err := queries.ListJobTasks(ctx, jobID)
	if err != nil {
		return err
	}
	if len(tasks) == 0 || tasks[len(tasks)-1].TaskID != taskID || tasks[len(tasks)-1].TaskName != core.CleanupTaskName {
		return fmt.Errorf("cleanup cannot complete without the exact Core cleanup task attachment")
	}
	if !job.AdmissionOpen && job.CleanupState == core.CleanupComplete {
		return tx.Commit()
	}
	if job.AdmissionOpen || job.CleanupState != core.CleanupScheduled {
		return fmt.Errorf("cleanup cannot complete while admission or cleanup scheduling remains unsettled")
	}
	deliveries, err := queries.ListDeliveries(ctx, jobID)
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if !delivery.AgentRunPresent {
			return fmt.Errorf("cleanup cannot complete because Message %s has no AgentRun", delivery.MessageID)
		}
		if delivery.AgentRunMessageID != delivery.MessageID || delivery.AgentRunJobID != delivery.MessageJobID {
			return fmt.Errorf("cleanup cannot complete because Message %s has a mismatched AgentRun %s", delivery.MessageID, delivery.AgentRunID)
		}
		run := delivery
		if run.State != core.AgentRunCompleted && run.State != core.AgentRunFailed && run.State != core.AgentRunInterrupted {
			return fmt.Errorf("cleanup cannot complete with unsettled AgentRun %s", run.AgentRunID)
		}
	}
	unsettled, err := queries.CountUnsettledSandboxCleanupActions(ctx, jobID)
	if err != nil {
		return err
	}
	if unsettled != 0 {
		return fmt.Errorf("cleanup cannot complete with %d unsettled Job resources", unsettled)
	}
	if err := expectOneRows(queries.CompleteCleanup(ctx, jobID)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) Actions(ctx context.Context, jobID string) ([]core.Action, error) {
	rows, err := dbsql.New(s.DB).ListActions(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var actions []core.Action
	for _, row := range rows {
		actions = append(actions, actionFromValues(row.ID, row.JobID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt))
	}
	return actions, nil
}

func (s Store) Evidence(ctx context.Context, jobID string) ([]core.Evidence, error) {
	rows, err := dbsql.New(s.DB).ListEvidence(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var records []core.Evidence
	for _, row := range rows {
		records = append(records, core.Evidence{ID: row.ID, Digest: row.Digest, ByteSize: row.ByteSize, MediaType: row.MediaType, Producer: row.Producer, Kind: row.Kind, ActionID: row.ActionID, AgentRunID: row.AgentRunID, Revision: row.Revision, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt})
	}
	return records, nil
}

func (s Store) NextWakeSequence(ctx context.Context, jobID string) (int64, error) {
	return dbsql.New(s.DB).NextWakeSequence(ctx, jobID)
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
