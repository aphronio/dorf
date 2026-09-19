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

// The 029 repair runs before the obsolete intermediary constraint in 026.
// Published migration files remain unchanged.
var dorfMigrations = []string{"001_greenfield.sql", "002_non_expiring_client_credentials.sql", "003_message_interrupt.sql", "004_direct_conversation_setup.sql", "005_message_instructions.sql", "006_remove_message_instructions.sql", "007_job_client_attribution.sql", "008_message_skill_refresh.sql", "009_message_attachments.sql", "010_job_idle_policy.sql", "011_message_developer_instructions.sql", "012_sandbox_idle_grace.sql", "013_message_observation.sql", "014_job_execution_wakes.sql", "015_observation_auto.sql", "016_profile_revisions.sql", "017_sandbox_resources.sql", "018_sandbox_delivery_holds.sql", "019_sandbox_upgrades.sql", "020_sandbox_checkpoints.sql", "021_checkpoint_recovery.sql", "022_remove_investigation.sql", "023_remove_coding.sql", "024_job_thread.sql", "025_sessions.sql", "029_retire_workflow_messages.sql", "026_session_execution_facts.sql", "027_native_session_guard.sql", "028_session_lifecycle_queue.sql"}

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
	client, err := absurd.New(absurd.Options{DB: s.DB, QueueName: "dorf_sessions"})
	if err != nil {
		return err
	}
	if err := client.CreateQueue(ctx, "dorf_sessions"); err != nil {
		return fmt.Errorf("create Absurd queue dorf_sessions: %w", err)
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
		ID: row.ID, AdmissionKey: row.AdmissionKey, AgentsMD: row.AgentsMd,
		Harness: row.Harness, ThreadID: row.ThreadID,
		SandboxProfile: row.SandboxProfile, SandboxProfileRevision: row.SandboxProfileRevision, ProviderConnection: row.ProviderConnection,
		KeepRunning: row.KeepRunning, Model: row.Model, ReasoningEffort: row.ReasoningEffort, AdmissionOpen: row.AdmissionOpen, CleanupState: core.CleanupState(row.CleanupState),
		CurrentTaskID:      row.CurrentTaskID,
		ExecutionAttention: row.ExecutionAttention, ExecutionAttentionSource: row.ExecutionAttentionSource,
		ExecutionAttentionAt: timeValue(row.ExecutionAttentionAt), CleanupAttention: row.CleanupAttention,
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
// independently of an expiring Absurd claim. Native mutation uses the same fence.
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
	if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock(hashtextextended('dorf-session-effect:' || $1, 0))`, sessionID); err != nil {
		return fmt.Errorf("acquire Session execution fence: %w", err)
	}
	return nil
}

// AttachSessionTask appends one exact Absurd task handoff. The deterministic Absurd
// idempotency key supplies task identity; Dorf records only ordered attachment.
func (s Store) AttachSessionTask(ctx context.Context, sessionID, expectedCurrentTaskID, taskID, taskName string) error {
	return s.attachSessionTask(ctx, sessionID, expectedCurrentTaskID, taskID, taskName, false)
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

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func (s Store) SetExecutionAttention(ctx context.Context, sessionID, source, detail string) error {
	source, detail = strings.TrimSpace(source), strings.TrimSpace(detail)
	if sessionID == "" || source == "" || detail == "" {
		return fmt.Errorf("execution attention requires Session ID, exact source, and detail")
	}
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return expectOneRows(dbsql.New(s.DB).SetExecutionAttention(ctx, dbsql.SetExecutionAttentionParams{SessionID: sessionID, Source: sql.NullString{String: source, Valid: true}, Detail: sql.NullString{String: detail, Valid: true}}))
}

func (s Store) ClearExecutionAttention(ctx context.Context, sessionID, source string) error {
	source = strings.TrimSpace(source)
	if sessionID == "" || source == "" {
		return fmt.Errorf("execution attention clearing requires Session ID and exact source")
	}
	rows, err := dbsql.New(s.DB).ClearExecutionAttention(ctx, dbsql.ClearExecutionAttentionParams{
		SessionID: sessionID, Source: sql.NullString{String: source, Valid: true},
	})
	if err != nil {
		return err
	}
	if rows > 1 {
		return fmt.Errorf("execution attention source %s changed %d Sessions", source, rows)
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
			ID: session.ID, AdmissionKey: session.AdmissionKey, AgentsMD: session.AgentsMd,
			Harness: session.Harness, ThreadID: session.ThreadID,
			KeepRunning: session.KeepRunning, SandboxProfile: session.SandboxProfile, SandboxProfileRevision: session.SandboxProfileRevision, ProviderConnection: session.ProviderConnection, Model: session.Model, ReasoningEffort: session.ReasoningEffort,
			AdmissionOpen: session.AdmissionOpen, CleanupState: session.CleanupState, CurrentTaskID: session.CurrentTaskID,
			ExecutionAttention: session.ExecutionAttention, ExecutionAttentionSource: session.ExecutionAttentionSource,
			ExecutionAttentionAt: timeValue(session.ExecutionAttentionAt), CleanupAttention: session.CleanupAttention,
			AdmittedAt: session.AdmittedAt, CleanedAt: timeValue(session.CleanedAt),
		},
		Sandbox: core.Sandbox{ID: owned.ID, SessionID: owned.SessionID, Name: owned.Name, OwnershipNonce: owned.OwnershipNonce, ResourceID: owned.ActiveResourceID, ProviderID: owned.ProviderID},
		Action:  actionFromValues(row.ID, row.SessionID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt),
		TaskID:  session.CurrentTaskID, TaskName: session.CurrentTaskName,
	}, nil
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
