package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var ErrNotFound = errors.New("Dorf Job not found")
var fullCommitOID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

const (
	AbsurdReleaseCommit = "550d3b9e6f9382d96178de6ab8c90c7f8edf2227"
	AbsurdSchemaURL     = "https://raw.githubusercontent.com/earendil-works/absurd/" + AbsurdReleaseCommit + "/sql/absurd.sql"
	AbsurdSchemaSHA256  = "d34309370c539f3a51f2b36b69b1f77551f8e4a14480a1c8def8bb8f40fd9aab"
	MessageTaskName     = "dorf-coding-job-v3"
	initialFromID       = "dorf:initial"
)

func MessageTaskKey(jobID string) string { return "coding-job:v3:" + jobID }

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

type NewJob struct {
	AdmissionKey       string
	Goal               string
	Repository         string
	Revision           string
	Branch             string
	ProviderConnection string
	Model              string
	ReasoningEffort    string
	GitHubRepository   string
	GitHubInstallation string
	BaseBranch         string
}

type NewMessage struct {
	JobID    string
	FromKind spine.MessageFromKind
	FromID   string
	Input    string
	Intent   spine.MessageDeliveryIntent
}

type ActionView struct {
	ID             string            `json:"id"`
	Kind           spine.ActionKind  `json:"kind"`
	State          spine.ActionState `json:"state"`
	ExternalID     string            `json:"external_id,omitempty"`
	Scope          string            `json:"scope,omitempty"`
	EvidenceDigest string            `json:"evidence_digest,omitempty"`
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
	var installed bool
	if err := tx.QueryRowContext(ctx, `select to_regnamespace('dorf') is not null`).Scan(&installed); err != nil {
		return err
	}
	if installed {
		var migrationsTable bool
		if err := tx.QueryRowContext(ctx, `select to_regclass('dorf.schema_migrations') is not null`).Scan(&migrationsTable); err != nil {
			return err
		}
		if !migrationsTable {
			return fmt.Errorf("existing Dorf schema has no baseline identity; recreate this prototype database")
		}
		var total, baseline int
		if err := tx.QueryRowContext(ctx, `select count(*),count(*) filter(where name='001_baseline.sql') from dorf.schema_migrations`).Scan(&total, &baseline); err != nil {
			return err
		}
		if total != 1 || baseline != 1 {
			return fmt.Errorf("prototype Dorf migration history is incompatible with the greenfield baseline; recreate this database")
		}
	} else {
		contents, err := migrationFiles.ReadFile("migrations/001_baseline.sql")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			return fmt.Errorf("apply Dorf baseline schema: %w", err)
		}
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

func ValidRevision(value string) bool { return fullCommitOID.MatchString(value) }

func (s Store) Admit(ctx context.Context, input NewJob) (spine.Job, bool, error) {
	input.AdmissionKey = strings.TrimSpace(input.AdmissionKey)
	input.Repository = strings.TrimSpace(input.Repository)
	input.Revision = strings.TrimSpace(input.Revision)
	input.Branch = strings.TrimSpace(input.Branch)
	input.ProviderConnection = strings.TrimSpace(input.ProviderConnection)
	input.Model = strings.TrimSpace(input.Model)
	input.ReasoningEffort = strings.TrimSpace(input.ReasoningEffort)
	input.GitHubRepository = strings.TrimSpace(input.GitHubRepository)
	input.GitHubInstallation = strings.TrimSpace(input.GitHubInstallation)
	input.BaseBranch = strings.TrimSpace(input.BaseBranch)
	if input.AdmissionKey == "" || strings.TrimSpace(input.Goal) == "" || input.Repository == "" || input.Branch == "" || input.ProviderConnection == "" || input.Model == "" || input.GitHubRepository == "" || input.GitHubInstallation == "" || input.BaseBranch == "" {
		return spine.Job{}, false, fmt.Errorf("admission requires key, complete goal, repository, branch, Provider Connection, model, canonical GitHub repository, installation, and explicit base branch")
	}
	if err := githubapi.ValidateAuthority(input.Repository, input.GitHubRepository, input.GitHubInstallation, input.BaseBranch, input.Branch); err != nil {
		return spine.Job{}, false, err
	}
	if !ValidRevision(input.Revision) {
		return spine.Job{}, false, fmt.Errorf("admitted revision must be a lowercase full commit OID (40 hex for SHA-1 or 64 hex for SHA-256)")
	}
	if input.ReasoningEffort != "low" && input.ReasoningEffort != "medium" && input.ReasoningEffort != "high" && input.ReasoningEffort != "xhigh" {
		return spine.Job{}, false, fmt.Errorf("reasoning effort must be low, medium, high, or xhigh")
	}
	id := spine.JobID(input.AdmissionKey)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Job{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	rows, err := queries.InsertAdmittedJob(ctx, dbsql.InsertAdmittedJobParams{
		ID: id, AdmissionKey: input.AdmissionKey, Goal: input.Goal, Repository: input.Repository,
		Revision: input.Revision, Branch: input.Branch, ProviderConnection: input.ProviderConnection,
		Model: input.Model, ReasoningEffort: input.ReasoningEffort, GithubRepository: sql.NullString{String: input.GitHubRepository, Valid: true},
		GithubInstallationID: sql.NullString{String: input.GitHubInstallation, Valid: true}, BaseBranch: sql.NullString{String: input.BaseBranch, Valid: true},
	})
	if err != nil {
		return spine.Job{}, false, err
	}
	storedRow, err := queries.GetAdmittedJobForUpdate(ctx, input.AdmissionKey)
	if err != nil {
		return spine.Job{}, false, err
	}
	stored := NewJob{
		AdmissionKey: storedRow.AdmissionKey, Goal: storedRow.Goal, Repository: storedRow.Repository,
		Revision: storedRow.Revision, Branch: storedRow.Branch, ProviderConnection: storedRow.ProviderConnection,
		Model: storedRow.Model, ReasoningEffort: storedRow.ReasoningEffort,
		GitHubRepository: storedRow.GithubRepository, GitHubInstallation: storedRow.GithubInstallationID, BaseBranch: storedRow.BaseBranch,
	}
	if storedRow.ID != id || stored != input {
		return spine.Job{}, false, fmt.Errorf("admission key %q is already bound to different complete Job input", input.AdmissionKey)
	}
	messageID := spine.MessageID(id, "human", initialFromID)
	if err := queries.InsertInitialMessage(ctx, dbsql.InsertInitialMessageParams{ID: messageID, JobID: id, FromID: initialFromID, Input: input.Goal}); err != nil {
		return spine.Job{}, false, err
	}
	initial, err := queries.GetInitialMessage(ctx, dbsql.GetInitialMessageParams{JobID: id, FromID: initialFromID})
	if err != nil {
		return spine.Job{}, false, err
	}
	if initial.Sequence != 1 || initial.Input != input.Goal {
		return spine.Job{}, false, fmt.Errorf("Job %s initial message conflicts with complete admission input", id)
	}
	runID := spine.AgentRunID(initial.ID)
	sandboxID := spine.MainSandboxName(id)
	ownerNonce, err := reviewNonce()
	if err != nil {
		return spine.Job{}, false, err
	}
	if err := expectOneRows(queries.ReserveSandbox(ctx, dbsql.ReserveSandboxParams{ID: sandboxID, JobID: id, OwnershipNonce: ownerNonce})); err != nil {
		// A retry must retain the originally reserved ownership nonce.
		if _, getErr := queries.GetSandbox(ctx, sandboxID); getErr != nil {
			return spine.Job{}, false, err
		}
	}
	routeActionID := spine.ScopedActionID(id, spine.ActionRouteCreate, sandboxID)
	if err := expectOneRows(queries.ReserveRoute(ctx, dbsql.ReserveRouteParams{ID: spine.ProviderRouteID(routeActionID), SandboxID: sandboxID})); err != nil {
		return spine.Job{}, false, err
	}
	if _, err := queries.InsertImplementationAgentRun(ctx, dbsql.InsertImplementationAgentRunParams{ID: runID, JobID: id, MessageID: initial.ID, SandboxID: sandboxID}); err != nil {
		return spine.Job{}, false, err
	}
	if err := queries.InsertInitialRevision(ctx, dbsql.InsertInitialRevisionParams{JobID: id, OID: input.Revision, Branch: input.Branch}); err != nil {
		return spine.Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Job{}, false, err
	}
	job, err := s.Job(ctx, id)
	return job, rows == 1, err
}

func (s Store) AdmitMessage(ctx context.Context, input NewMessage) (spine.Message, bool, error) {
	return s.admitMessage(ctx, input)
}

func (s Store) admitMessage(ctx context.Context, input NewMessage) (spine.Message, bool, error) {
	input, err := normalizeMessage(input)
	if err != nil {
		return spine.Message{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Message{}, false, err
	}
	defer tx.Rollback()
	message, created, err := admitMessageTx(ctx, tx, input)
	if err != nil {
		return spine.Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Message{}, false, err
	}
	return message, created, nil
}

func normalizeMessage(input NewMessage) (NewMessage, error) {
	input.JobID = strings.TrimSpace(input.JobID)
	input.FromKind = spine.MessageFromKind(strings.TrimSpace(string(input.FromKind)))
	input.FromID = strings.TrimSpace(input.FromID)
	if input.FromKind == "" {
		input.FromKind = spine.MessageFromHuman
	}
	if input.Intent == "" {
		input.Intent = spine.MessageFollow
	}
	if input.JobID == "" || input.FromID == "" || strings.TrimSpace(input.Input) == "" {
		return NewMessage{}, fmt.Errorf("message admission requires Job ID, from ID, and complete input")
	}
	if input.FromKind != spine.MessageFromHuman && input.FromKind != spine.MessageFromAgent && input.FromKind != spine.MessageFromWorkflow {
		return NewMessage{}, fmt.Errorf("invalid message from kind")
	}
	if len(input.FromID) > 256 {
		return NewMessage{}, fmt.Errorf("from ID must be at most 256 characters")
	}
	if input.Intent != spine.MessageFollow && input.Intent != spine.MessageSteer {
		return NewMessage{}, fmt.Errorf("message intent must be follow or steer")
	}
	if len(input.Input) > 1<<20 {
		return NewMessage{}, fmt.Errorf("message input exceeds 1 MiB")
	}
	return input, nil
}

func admitMessageTx(ctx context.Context, tx *sql.Tx, input NewMessage) (spine.Message, bool, error) {
	queries := dbsql.New(tx)
	job, err := queries.GetJobAdmissionForUpdate(ctx, input.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return spine.Message{}, false, ErrNotFound
		}
		return spine.Message{}, false, err
	}
	row, err := queries.GetMessageBySender(ctx, dbsql.GetMessageBySenderParams{JobID: input.JobID, FromKind: input.FromKind, FromID: input.FromID})
	if err == nil {
		message := messageFromValues(row.ID, row.JobID, row.FromKind, row.FromID, row.Sequence, row.Input, row.DeliveryIntent, row.SteerTargetTurnID)
		if message.Input != input.Input || message.Intent != input.Intent {
			return spine.Message{}, false, fmt.Errorf("sender %s/%q is already bound to different complete message input or delivery intent", input.FromKind, input.FromID)
		}
		return message, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spine.Message{}, false, err
	}
	if !job.AdmissionOpen {
		return spine.Message{}, false, fmt.Errorf("Job %s admission is closed for cleanup", input.JobID)
	}
	if job.WorkflowPhase == "published" && input.Intent == spine.MessageFollow {
		updated, err := queries.ReopenPublishedForFollow(ctx, input.JobID)
		if err != nil {
			return spine.Message{}, false, err
		}
		if updated != 1 {
			return spine.Message{}, false, fmt.Errorf("Job %s outcome is already recorded", input.JobID)
		}
		job.WorkflowPhase = "implementing"
	}
	if job.WorkflowPhase != "setup" && job.WorkflowPhase != "implementing" && job.WorkflowPhase != "review-feedback" {
		return spine.Message{}, false, fmt.Errorf("Job %s does not accept implementation Messages during workflow phase %s", input.JobID, job.WorkflowPhase)
	}
	var message spine.Message
	role, harness, threadID := "implement", "", ""
	if input.Intent == spine.MessageSteer {
		active, err := queries.GetActiveImplementationTurn(ctx, input.JobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return spine.Message{}, false, fmt.Errorf("steer delivery requires an exact active regular harness Turn")
			}
			return spine.Message{}, false, err
		}
		message.TargetTurnID, harness, threadID, role = active.TurnID, active.Harness, active.ThreadID, active.Role
	}
	message.Sequence, err = queries.NextMessageSequence(ctx, input.JobID)
	if err != nil {
		return spine.Message{}, false, err
	}
	message.ID = spine.MessageID(input.JobID, input.FromKind, input.FromID)
	message.JobID, message.FromKind, message.FromID, message.Input, message.Intent = input.JobID, input.FromKind, input.FromID, input.Input, input.Intent
	if err := queries.InsertMessage(ctx, dbsql.InsertMessageParams{ID: message.ID, JobID: message.JobID, FromKind: message.FromKind, FromID: message.FromID, Sequence: message.Sequence, Input: message.Input, DeliveryIntent: message.Intent, SteerTargetTurnID: message.TargetTurnID}); err != nil {
		return spine.Message{}, false, err
	}
	runID := spine.AgentRunID(message.ID)
	if role != "implement" {
		return spine.Message{}, false, fmt.Errorf("steer target AgentRun has unsupported role %s", role)
	}
	if _, err := queries.InsertImplementationAgentRun(ctx, dbsql.InsertImplementationAgentRunParams{ID: runID, JobID: message.JobID, MessageID: message.ID, SandboxID: spine.MainSandboxName(message.JobID)}); err != nil {
		return spine.Message{}, false, err
	}
	if harness != "" || threadID != "" {
		if err := expectOneRows(queries.BindAgentRunIdentity(ctx, dbsql.BindAgentRunIdentityParams{Harness: sql.NullString{String: harness, Valid: true}, ThreadID: sql.NullString{String: threadID, Valid: true}, RunID: runID})); err != nil {
			return spine.Message{}, false, err
		}
	}
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

func (s Store) Job(ctx context.Context, id string) (spine.Job, error) {
	row, err := dbsql.New(s.DB).GetJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return spine.Job{}, ErrNotFound
	}
	if err != nil {
		return spine.Job{}, err
	}
	return spine.Job{
		ID: row.ID, AdmissionKey: row.AdmissionKey, Goal: row.Goal, Repository: row.Repository,
		Revision: row.Revision, RevisionGeneration: int(row.RevisionGeneration), StartingRevision: row.StartingRevision, Branch: row.Branch,
		GitHubRepository: row.GithubRepository, GitHubInstallation: row.GithubInstallationID, BaseBranch: row.BaseBranch,
		ProviderConnection: row.ProviderConnection,
		Model:              row.Model, ReasoningEffort: row.ReasoningEffort, AdmissionOpen: row.AdmissionOpen, CleanupState: spine.CleanupState(row.CleanupState),
		TaskID: row.TaskID, CleanupTaskID: row.CleanupTaskID, WorkflowPhase: row.WorkflowPhase,
		WorkflowAttention: row.WorkflowAttention, CleanupAttention: row.CleanupAttention,
	}, nil
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

// AttachMessageTask binds the public Spawn result for the one exact consumer
// admitted for this Job. The deterministic Absurd idempotency key supplies
// task identity; Dorf only compare-and-sets its cross-authority binding.
func (s Store) AttachMessageTask(ctx context.Context, jobID, proposedTaskID string) error {
	return expectOneRows(dbsql.New(s.DB).AttachMessageTask(ctx, dbsql.AttachMessageTaskParams{JobID: jobID, TaskID: sql.NullString{String: proposedTaskID, Valid: true}}))
}

func messageFromValues(id, jobID string, fromKind spine.MessageFromKind, fromID string, sequence int64, input string, intent spine.MessageDeliveryIntent, targetTurnID string) spine.Message {
	return spine.Message{ID: id, JobID: jobID, FromKind: fromKind, FromID: fromID, Sequence: sequence, Input: input, Intent: intent, TargetTurnID: targetTurnID}
}

func actionFromValues(id, jobID string, kind spine.ActionKind, state spine.ActionState, externalID, outcome, scope string) spine.Action {
	return spine.Action{ID: id, JobID: jobID, Kind: kind, State: state, ExternalID: externalID, Outcome: outcome, Scope: scope}
}

func agentRunFromValues(id, jobID, messageID string, state spine.AgentRunState, harness, threadID string, baselineRecorded bool, baselineTurnID, turnID, turnOutcome, attention, role string) spine.AgentRun {
	return spine.AgentRun{ID: id, JobID: jobID, MessageID: messageID, Harness: harness, ThreadID: threadID, State: state, BaselineRecorded: baselineRecorded, BaselineTurnID: baselineTurnID, TurnID: turnID, TurnOutcome: turnOutcome, Attention: attention, Role: role}
}

func checkFromValues(id, jobID, name, command, revision, state string, exitCode int32, evidenceID, evidenceDigest string, startedAt, finishedAt sql.NullTime) spine.Check {
	return spine.Check{ID: id, JobID: jobID, Name: name, Command: command, Revision: revision, State: state, ExitCode: int(exitCode), EvidenceID: evidenceID, EvidenceDigest: evidenceDigest, StartedAt: timeValue(startedAt), FinishedAt: timeValue(finishedAt)}
}

func timeValue(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}

func (s Store) CloseAdmission(ctx context.Context, jobID string) error {
	return expectOneRows(dbsql.New(s.DB).CloseAdmission(ctx, jobID))
}

func (s Store) SetCleanupTaskID(ctx context.Context, jobID, taskID string) error {
	return expectOneRows(dbsql.New(s.DB).SetCleanupTaskID(ctx, dbsql.SetCleanupTaskIDParams{CleanupTaskID: sql.NullString{String: taskID, Valid: true}, JobID: jobID}))
}

func (s Store) BeginAction(ctx context.Context, jobID string, kind spine.ActionKind) (spine.Action, error) {
	if kind == spine.ActionSandboxCreate || kind == spine.ActionRouteCreate || kind == spine.ActionRouteRevoke || kind == spine.ActionSandboxDelete {
		return s.BeginResourceAction(ctx, spine.MainSandboxName(jobID), kind)
	}
	desiredID := spine.ActionID(jobID, kind)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Action{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	if err := queries.InsertActionIfAbsent(ctx, dbsql.InsertActionIfAbsentParams{ID: desiredID, JobID: jobID, Kind: kind}); err != nil {
		return spine.Action{}, err
	}
	row, err := queries.GetUnscopedActionForUpdate(ctx, dbsql.GetUnscopedActionForUpdateParams{JobID: jobID, Kind: kind})
	if err != nil {
		return spine.Action{}, err
	}
	action := actionFromValues(row.ID, row.JobID, row.Kind, row.State, row.ExternalID, row.ExternalOutcome, row.ScopeKey)
	if err := tx.Commit(); err != nil {
		return spine.Action{}, err
	}
	return action, nil
}

func (s Store) BeginResourceAction(ctx context.Context, sandboxID string, kind spine.ActionKind) (spine.Action, error) {
	sandbox, err := dbsql.New(s.DB).GetSandbox(ctx, sandboxID)
	if err != nil {
		return spine.Action{}, err
	}
	switch kind {
	case spine.ActionSandboxCreate, spine.ActionRouteCreate, spine.ActionReviewWorkspaceCreate, spine.ActionRouteRevoke, spine.ActionSandboxDelete:
	default:
		return spine.Action{}, fmt.Errorf("unsupported resource Action %q", kind)
	}
	id := spine.ScopedActionID(sandbox.JobID, kind, sandboxID)
	q := dbsql.New(s.DB)
	if err := expectOneRows(q.InsertScopedAction(ctx, dbsql.InsertScopedActionParams{ID: id, JobID: sandbox.JobID, Kind: kind, ScopeKey: sandboxID})); err != nil {
		row, getErr := q.GetScopedActionBySandbox(ctx, dbsql.GetScopedActionBySandboxParams{JobID: sandbox.JobID, Kind: kind, SandboxID: sandboxID})
		if getErr != nil {
			return spine.Action{}, err
		}
		return actionFromValues(row.ID, row.JobID, row.Kind, row.State, row.ExternalID, row.ExternalOutcome, row.ScopeKey), nil
	}
	row, err := q.GetScopedActionBySandbox(ctx, dbsql.GetScopedActionBySandboxParams{JobID: sandbox.JobID, Kind: kind, SandboxID: sandboxID})
	if err != nil {
		return spine.Action{}, err
	}
	return actionFromValues(row.ID, row.JobID, row.Kind, row.State, row.ExternalID, row.ExternalOutcome, row.ScopeKey), nil
}

func (s Store) Sandbox(ctx context.Context, id string) (spine.Sandbox, error) {
	row, err := dbsql.New(s.DB).GetSandbox(ctx, id)
	if err != nil {
		return spine.Sandbox{}, err
	}
	return spine.Sandbox{ID: row.ID, JobID: row.JobID, State: row.State, OwnershipNonce: row.OwnershipNonce}, nil
}
func (s Store) Route(ctx context.Context, sandboxID string) (spine.Route, error) {
	row, err := dbsql.New(s.DB).GetRouteBySandbox(ctx, sandboxID)
	if err != nil {
		return spine.Route{}, err
	}
	return spine.Route{ID: row.ID, SandboxID: row.SandboxID, State: row.State}, nil
}
func (s Store) Sandboxes(ctx context.Context, jobID string) ([]spine.Sandbox, error) {
	rows, err := dbsql.New(s.DB).ListJobSandboxes(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]spine.Sandbox, 0, len(rows))
	for _, r := range rows {
		out = append(out, spine.Sandbox{ID: r.ID, JobID: r.JobID, State: r.State, OwnershipNonce: r.OwnershipNonce})
	}
	return out, nil
}

func (s Store) AgentRuns(ctx context.Context, jobID string) ([]spine.AgentRun, error) {
	rows, err := dbsql.New(s.DB).ListJobAgentRuns(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]spine.AgentRun, 0, len(rows))
	for _, r := range rows {
		out = append(out, spine.AgentRun{ID: r.ID, JobID: r.JobID, MessageID: r.MessageID, State: r.State, Harness: r.Harness, ThreadID: r.ThreadID, BaselineRecorded: r.BaselineTurnID != "", BaselineTurnID: r.BaselineTurnID, TurnID: r.TurnID, TurnOutcome: r.TurnOutcome, Attention: r.Attention, Role: r.Role, Revision: r.Revision, Capability: r.Capability, SandboxID: r.SandboxID, SubmissionNonce: r.SubmissionNonce, StartedAt: timeValue(r.StartedAt), FinishedAt: timeValue(r.FinishedAt)})
	}
	return out, nil
}

func (s Store) InterruptAgentRun(ctx context.Context, runID, reason string) error {
	q := dbsql.New(s.DB)
	row, err := q.GetAgentRunForBinding(ctx, runID)
	if err != nil {
		return err
	}
	if row.State == spine.AgentRunCompleted || row.State == spine.AgentRunFailed || row.State == spine.AgentRunInterrupted {
		return nil
	}
	return expectOneRows(q.InterruptAgentRun(ctx, dbsql.InterruptAgentRunParams{Reason: reason, RunID: runID}))
}

// BeginSetup returns only the setup generation currently selected by the Job.
// A failed generation is terminal; RetrySetup selects a new scoped Action
// without changing or deleting its retained Evidence.
func (s Store) BeginSetup(ctx context.Context, jobID string) (spine.Action, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Action{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	desiredID := spine.ActionID(jobID, spine.ActionRepositorySetup)
	currentID, err := queries.GetSetupActionIDForUpdate(ctx, jobID)
	if err != nil {
		return spine.Action{}, err
	}
	if currentID == "" {
		if err := queries.InsertActionIfAbsent(ctx, dbsql.InsertActionIfAbsentParams{ID: desiredID, JobID: jobID, Kind: spine.ActionRepositorySetup}); err != nil {
			return spine.Action{}, err
		}
		if err := expectOneRows(queries.SelectInitialSetupAction(ctx, dbsql.SelectInitialSetupActionParams{ActionID: sql.NullString{String: desiredID, Valid: true}, JobID: jobID})); err != nil {
			return spine.Action{}, err
		}
		currentID = desiredID
	}
	row, err := queries.GetActionForUpdate(ctx, dbsql.GetActionForUpdateParams{ID: currentID, JobID: jobID, Kind: spine.ActionRepositorySetup})
	if err != nil {
		return spine.Action{}, err
	}
	action := actionFromValues(row.ID, row.JobID, row.Kind, row.State, row.ExternalID, row.ExternalOutcome, row.ScopeKey)
	if err := tx.Commit(); err != nil {
		return spine.Action{}, err
	}
	return action, nil
}

// RetrySetup atomically selects one explicit setup generation and admits its
// durable wake after a terminal failure. retryID is the stable operator
// identity for the Action/message pair.
func (s Store) RetrySetup(ctx context.Context, jobID, retryID, input string) (spine.Action, spine.Message, bool, error) {
	jobID = strings.TrimSpace(jobID)
	retryID = strings.TrimSpace(retryID)
	if jobID == "" || retryID == "" {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("setup retry requires a Job ID and stable retry identity")
	}
	if len(retryID) > 239 || strings.HasPrefix(retryID, "dorf:") || strings.HasPrefix(retryID, "review:") {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("setup retry identity must be at most 239 characters and must not use a reserved dorf: or review: prefix")
	}
	// The workflow owns this durable wake. Keep its stable retry identity in
	// FromID, while avoiding the human namespace used by public callers.
	messageInput, err := normalizeMessage(NewMessage{JobID: jobID, FromKind: spine.MessageFromWorkflow, FromID: retryID, Input: input})
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetSetupRetryJobForUpdate(ctx, jobID)
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	desiredID := spine.ScopedActionID(jobID, spine.ActionRepositorySetup, retryID)
	existingRow, err := queries.GetAction(ctx, dbsql.GetActionParams{ID: desiredID, JobID: jobID, Kind: spine.ActionRepositorySetup})
	if err == nil {
		existing := actionFromValues(existingRow.ID, existingRow.JobID, existingRow.Kind, existingRow.State, existingRow.ExternalID, existingRow.ExternalOutcome, existingRow.ScopeKey)
		message, _, messageErr := admitMessageTx(ctx, tx, messageInput)
		if messageErr != nil {
			return spine.Action{}, spine.Message{}, false, fmt.Errorf("recover setup retry Action/message pair: %w", messageErr)
		}
		if err := tx.Commit(); err != nil {
			return spine.Action{}, spine.Message{}, false, err
		}
		return existing, message, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spine.Action{}, spine.Message{}, false, err
	}
	if !locked.AdmissionOpen {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("Job %s admission is closed; repository setup cannot be retried", jobID)
	}
	if locked.WorkflowPhase != "blocked" || locked.SetupActionID == "" {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("Job %s repository setup retry is not admissible during workflow phase %s", jobID, locked.WorkflowPhase)
	}
	currentState, err := queries.GetActionStateForUpdate(ctx, dbsql.GetActionStateForUpdateParams{ID: locked.SetupActionID, JobID: jobID, Kind: spine.ActionRepositorySetup})
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	if spine.ActionState(currentState) != spine.ActionFailed {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("Job %s current repository setup Action %s is %s, not terminal failed", jobID, locked.SetupActionID, currentState)
	}
	createdRows, err := queries.InsertScopedAction(ctx, dbsql.InsertScopedActionParams{ID: desiredID, JobID: jobID, Kind: spine.ActionRepositorySetup, ScopeKey: retryID})
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	if createdRows != 1 {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("setup retry identity %q was already used by another generation", retryID)
	}
	if err := expectOneRows(queries.SelectSetupRetry(ctx, dbsql.SelectSetupRetryParams{ActionID: sql.NullString{String: desiredID, Valid: true}, JobID: jobID, PreviousActionID: sql.NullString{String: locked.SetupActionID, Valid: true}})); err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	message, messageCreated, err := admitMessageTx(ctx, tx, messageInput)
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	if !messageCreated {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("setup retry identity %q already has a message without its Action", retryID)
	}
	action := spine.Action{ID: desiredID, JobID: jobID, Kind: spine.ActionRepositorySetup, State: spine.ActionPending, Scope: retryID}
	if err := tx.Commit(); err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	return action, message, true, nil
}

func revisionCandidateTx(ctx context.Context, tx *sql.Tx, jobID string) (spine.AgentRun, bool, error) {
	queries := dbsql.New(tx)
	unsettled, err := queries.CountUnsettledInputs(ctx, jobID)
	if err != nil {
		return spine.AgentRun{}, false, err
	}
	if unsettled != 0 {
		return spine.AgentRun{}, false, nil
	}
	row, err := queries.GetLatestFollowRun(ctx, jobID)
	if err != nil {
		return spine.AgentRun{}, false, err
	}
	run := spine.AgentRun{ID: row.ID, JobID: row.JobID, State: row.State, Role: row.Role}
	if run.State != spine.AgentRunCompleted || run.Role != "implement" {
		return spine.AgentRun{}, false, nil
	}
	return run, true, nil
}

// RevisionCandidate identifies the completed implementation AgentRun
// whose final clean Git state may become the next accepted Revision. It does
// not reserve an Action or close message admission; RecordRevision repeats the
// same checks atomically so a late accepted Message cannot be skipped.
func (s Store) RevisionCandidate(ctx context.Context, jobID, comparisonBase string) (spine.AgentRun, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.AgentRun{}, false, err
	}
	defer tx.Rollback()
	locked, err := dbsql.New(s.DB).WithTx(tx).GetRevisionPhaseForUpdate(ctx, jobID)
	if err != nil {
		return spine.AgentRun{}, false, err
	}
	if locked.Revision != comparisonBase {
		return spine.AgentRun{}, false, fmt.Errorf("Revision comparison base %s conflicts with current Revision %s", comparisonBase, locked.Revision)
	}
	if locked.WorkflowPhase != "implementing" && locked.WorkflowPhase != "review-feedback" {
		return spine.AgentRun{}, false, fmt.Errorf("Revision observation is not admissible during workflow phase %s", locked.WorkflowPhase)
	}
	run, ready, err := revisionCandidateTx(ctx, tx, jobID)
	if err != nil {
		return spine.AgentRun{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.AgentRun{}, false, err
	}
	return run, ready, nil
}

func insertEvidence(ctx context.Context, tx *sql.Tx, jobID string, evidence spine.Evidence) error {
	queries := dbsql.New(tx)
	err := queries.InsertEvidence(ctx, dbsql.InsertEvidenceParams{
		ID: evidence.ID, JobID: jobID, Digest: evidence.Digest, ByteSize: evidence.ByteSize,
		MediaType: evidence.MediaType, Producer: evidence.Producer,
		Kind: evidence.Kind, ActionID: evidence.ActionID, CheckID: evidence.CheckID, AgentRunID: evidence.AgentRunID, Revision: evidence.Revision,
		StartedAt: nullableTime(evidence.StartedAt), FinishedAt: nullableTime(evidence.FinishedAt),
	})
	if err != nil {
		return err
	}
	stored, err := queries.GetEvidenceIdentity(ctx, evidence.ID)
	if err != nil {
		return err
	}
	if stored.JobID != jobID || stored.Digest != evidence.Digest || stored.ByteSize != evidence.ByteSize || stored.MediaType != evidence.MediaType || stored.Producer != evidence.Producer || stored.Kind != evidence.Kind || stored.ActionID != evidence.ActionID || stored.CheckID != evidence.CheckID || stored.AgentRunID != evidence.AgentRunID || stored.Revision != evidence.Revision || !stored.StartedAt.Equal(evidence.StartedAt) || !stored.FinishedAt.Equal(evidence.FinishedAt) {
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

func (s Store) RecordSetup(ctx context.Context, actionID string, evidence spine.Evidence, observation spine.CommandObservation, checks []spine.DeclaredCheck) error {
	if len(checks) == 0 {
		return fmt.Errorf("repository setup requires at least one pinned Check")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	setup, err := queries.GetSetupActionForUpdate(ctx, actionID)
	if err != nil {
		return err
	}
	if spine.ActionKind(setup.Kind) != spine.ActionRepositorySetup || evidence.ActionID != actionID || setup.SetupActionID != actionID {
		return fmt.Errorf("setup Evidence does not match its Action")
	}
	if err := insertEvidence(ctx, tx, setup.JobID, evidence); err != nil {
		return err
	}
	commands := append([]spine.DeclaredCheck{{Name: "prepare", Command: observation.Command}}, checks...)
	for _, command := range commands {
		if command.Name != "prepare" && command.Name != "check" && command.Name != "smoke" || strings.TrimSpace(command.Command) == "" {
			return fmt.Errorf("invalid pinned repository command %q", command.Name)
		}
		if err := queries.InsertRepositoryCommand(ctx, dbsql.InsertRepositoryCommandParams{JobID: setup.JobID, Name: command.Name, Command: command.Command}); err != nil {
			return err
		}
		storedCommand, err := queries.GetRepositoryCommand(ctx, dbsql.GetRepositoryCommandParams{JobID: setup.JobID, Name: command.Name})
		if err != nil {
			return err
		}
		if storedCommand != command.Command {
			return fmt.Errorf("repository command %s conflicts with its pinned command", command.Name)
		}
	}
	state, phase, attention := "succeeded", "implementing", ""
	if observation.ExitCode != 0 {
		state, phase = "failed", "blocked"
		attention = fmt.Sprintf("repository setup failed with exit %d; Evidence %s", observation.ExitCode, evidence.Digest)
	}
	if err := queries.FinishSetupAction(ctx, dbsql.FinishSetupActionParams{State: spine.ActionState(state), ExternalID: sql.NullString{String: evidence.Digest, Valid: true}, ExternalOutcome: sql.NullString{String: fmt.Sprintf("exit=%d", observation.ExitCode), Valid: true}, ActionID: actionID}); err != nil {
		return err
	}
	if err := expectOneRows(queries.SetWorkflowPhaseAfterSetup(ctx, dbsql.SetWorkflowPhaseAfterSetupParams{WorkflowPhase: phase, WorkflowAttention: attention, JobID: setup.JobID})); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) DeclaredChecks(ctx context.Context, jobID string) ([]spine.DeclaredCheck, error) {
	rows, err := dbsql.New(s.DB).ListDeclaredChecks(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var checks []spine.DeclaredCheck
	for _, row := range rows {
		checks = append(checks, spine.DeclaredCheck{Name: row.Name, Command: row.Command})
	}
	if len(checks) == 0 {
		return nil, fmt.Errorf("pinned repository contract has no Checks")
	}
	return checks, nil
}

func (s Store) RecordRevision(ctx context.Context, jobID, runID string, observation spine.RevisionObservation, evidence spine.Evidence) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetRevisionJobForUpdate(ctx, jobID)
	if err != nil {
		return false, err
	}
	if locked.Revision != observation.ComparisonBase || locked.Branch != observation.Branch || locked.WorkflowPhase != "implementing" && locked.WorkflowPhase != "review-feedback" ||
		evidence.ID != spine.EvidenceID(runID, "git-revision") || evidence.ActionID != "" || evidence.CheckID != "" || evidence.AgentRunID != "" || evidence.Revision != observation.Revision ||
		!ValidRevision(observation.ComparisonBase) || !ValidRevision(observation.Revision) || !ValidRevision(observation.Tree) || observation.Revision == observation.ComparisonBase {
		return false, fmt.Errorf("Git Revision observation conflicts with durable comparison base, branch, AgentRun, or Evidence")
	}
	candidate, ready, err := revisionCandidateTx(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if !ready || candidate.ID != runID {
		return false, nil
	}
	if err := insertEvidence(ctx, tx, jobID, evidence); err != nil {
		return false, err
	}
	generation, err := queries.NextRevisionGeneration(ctx, jobID)
	if err != nil {
		return false, err
	}
	if err := queries.InsertRevision(ctx, dbsql.InsertRevisionParams{
		JobID: jobID, OID: observation.Revision, ComparisonBaseOID: observation.ComparisonBase,
		TreeOID: observation.Tree, Branch: observation.Branch, Generation: generation, EvidenceID: evidence.ID,
	}); err != nil {
		return false, err
	}
	updated, err := queries.AdvanceJobRevision(ctx, dbsql.AdvanceJobRevisionParams{JobID: jobID, Revision: observation.Revision, ComparisonBaseOID: observation.ComparisonBase})
	if err != nil {
		return false, err
	}
	if updated != 1 {
		return false, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// CompleteUnchangedRun records a completed no-change implementation result only
// if the same AgentRun is still the latest accepted candidate. A follow-up to an
// exact published proposal returns that proposal to published; initial no-change
// runs remain blocked.
func (s Store) CompleteUnchangedRun(ctx context.Context, jobID, runID, comparisonBase, reason string) (bool, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return false, fmt.Errorf("no-Revision attention requires a reason")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetRevisionPhaseForUpdate(ctx, jobID)
	if err != nil {
		return false, err
	}
	if locked.Revision != comparisonBase || locked.WorkflowPhase != "implementing" {
		return false, fmt.Errorf("no-Revision result conflicts with current Revision %s or workflow phase %s", locked.Revision, locked.WorkflowPhase)
	}
	candidate, ready, err := revisionCandidateTx(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if !ready || candidate.ID != runID {
		return false, nil
	}
	if err := expectOneRows(queries.CompleteUnchangedRun(ctx, dbsql.CompleteUnchangedRunParams{Reason: sql.NullString{String: reason, Valid: true}, JobID: jobID, Revision: comparisonBase})); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s Store) BeginCheck(ctx context.Context, jobID, revision, name, command string) (spine.Check, error) {
	id := spine.CheckID(jobID, revision, name)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Check{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	if err := queries.InsertCheck(ctx, dbsql.InsertCheckParams{ID: id, JobID: jobID, Name: name, Command: command, Revision: revision}); err != nil {
		return spine.Check{}, err
	}
	row, err := queries.GetCheck(ctx, id)
	if err != nil {
		return spine.Check{}, err
	}
	check := checkFromValues(row.ID, row.JobID, row.Name, row.Command, row.Revision, row.State, row.ExitCode, row.EvidenceID, "", row.StartedAt, row.FinishedAt)
	if check.Command != command {
		return spine.Check{}, fmt.Errorf("Check %s command conflicts at Revision %s", name, revision)
	}
	if check.EvidenceID != "" {
		check.EvidenceDigest, err = queries.GetEvidenceDigest(ctx, check.EvidenceID)
		if err != nil {
			return spine.Check{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return spine.Check{}, err
	}
	return check, nil
}

func (s Store) RecordCheck(ctx context.Context, check spine.Check, evidence spine.Evidence, observation spine.CommandObservation) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetCheckForUpdate(ctx, check.ID)
	if err != nil {
		return err
	}
	if locked.JobID != check.JobID || locked.Revision != check.Revision || locked.Command != observation.Command || evidence.CheckID != check.ID || evidence.Revision != locked.Revision {
		return fmt.Errorf("Check observation conflicts with its exact Revision or command")
	}
	if err := insertEvidence(ctx, tx, locked.JobID, evidence); err != nil {
		return err
	}
	state := "passed"
	if observation.ExitCode != 0 {
		state = "failed"
	}
	if err := queries.CompleteCheck(ctx, dbsql.CompleteCheckParams{State: state, ExitCode: sql.NullInt32{Int32: int32(observation.ExitCode), Valid: true}, EvidenceID: sql.NullString{String: evidence.ID, Valid: true}, StartedAt: nullableTime(observation.StartedAt), FinishedAt: nullableTime(observation.FinishedAt), ID: check.ID}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) AdmitCheckMessage(ctx context.Context, check spine.Check) (spine.Message, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Message{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetRevisionPhaseForUpdate(ctx, check.JobID)
	if err != nil {
		return spine.Message{}, false, err
	}
	existingRow, err := queries.GetCheckMessage(ctx, dbsql.GetCheckMessageParams{JobID: check.JobID, FromID: check.ID})
	if err == nil {
		existing := messageFromValues(existingRow.ID, existingRow.JobID, existingRow.FromKind, existingRow.FromID, existingRow.Sequence, existingRow.Input, existingRow.DeliveryIntent, existingRow.SteerTargetTurnID)
		if err := tx.Commit(); err != nil {
			return spine.Message{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spine.Message{}, false, err
	}
	if locked.WorkflowPhase != "checking" || locked.Revision != check.Revision || check.State != "failed" {
		return spine.Message{}, false, fmt.Errorf("failed Check Message is not admissible for the current Check")
	}
	if err := ensureInputsTerminalForWorkflowTx(ctx, tx, check.JobID); err != nil {
		return spine.Message{}, false, fmt.Errorf("failed Check Message admission blocked: %w", err)
	}
	sequence, err := allocateMessageSequenceTx(ctx, tx, check.JobID)
	if err != nil {
		return spine.Message{}, false, err
	}
	input := fmt.Sprintf("The deterministic %s Check failed at exact Revision %s with exit %d. Its command was %q and observed Evidence digest is %s. Resolve the failure if code changes are warranted, keep the checkout clean, and return control so Dorf can observe either a new Revision or an unchanged HEAD before rerunning verification programmatically.", check.Name, check.Revision, check.ExitCode, check.Command, check.EvidenceDigest)
	message := spine.Message{ID: spine.MessageID(check.JobID, spine.MessageFromWorkflow, check.ID), JobID: check.JobID, FromKind: spine.MessageFromWorkflow, FromID: check.ID, Sequence: sequence, Input: input, Intent: spine.MessageFollow}
	if err := queries.InsertMessage(ctx, dbsql.InsertMessageParams{ID: message.ID, JobID: message.JobID, FromKind: message.FromKind, FromID: message.FromID, Sequence: message.Sequence, Input: message.Input, DeliveryIntent: message.Intent}); err != nil {
		return spine.Message{}, false, err
	}
	runID := spine.AgentRunID(message.ID)
	if err := expectOneRows(queries.InsertImplementationAgentRun(ctx, dbsql.InsertImplementationAgentRunParams{ID: runID, JobID: message.JobID, MessageID: message.ID, SandboxID: spine.MainSandboxName(message.JobID)})); err != nil {
		return spine.Message{}, false, err
	}
	if err := expectOneRows(queries.ReturnFailedCheckToImplementation(ctx, dbsql.ReturnFailedCheckToImplementationParams{JobID: check.JobID, Revision: check.Revision})); err != nil {
		return spine.Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Message{}, false, err
	}
	return message, true, nil
}

func (s Store) MarkReady(ctx context.Context, jobID, revision string, verifiedEvidenceIDs []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetRevisionPhaseForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if locked.Revision != revision || locked.WorkflowPhase != "checking" {
		return fmt.Errorf("Revision %s cannot become ready from phase %s at Revision %s", revision, locked.WorkflowPhase, locked.Revision)
	}
	rows, err := queries.ListPassingCheckEvidence(ctx, dbsql.ListPassingCheckEvidenceParams{Revision: revision, JobID: jobID})
	if err != nil {
		return err
	}
	var proving []string
	for _, id := range rows {
		if !id.Valid || id.String == "" {
			return fmt.Errorf("Revision %s has a passing Check without Evidence", revision)
		}
		proving = append(proving, id.String)
	}
	declared, err := queries.CountDeclaredChecks(ctx, jobID)
	if err != nil {
		return err
	}
	sort.Strings(proving)
	verified := append([]string(nil), verifiedEvidenceIDs...)
	sort.Strings(verified)
	if declared == 0 || int64(len(proving)) != declared || !slices.Equal(proving, verified) {
		return fmt.Errorf("Revision %s is not ready: verified Evidence does not exactly match %d declared Checks", revision, declared)
	}
	if err := expectOneRows(queries.MarkReady(ctx, dbsql.MarkReadyParams{JobID: jobID, Revision: revision})); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) BlockWorkflow(ctx context.Context, jobID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).BlockWorkflow(ctx, dbsql.BlockWorkflowParams{Reason: sql.NullString{String: reason, Valid: true}, JobID: jobID}))
}

func (s Store) CompleteAction(ctx context.Context, id string, receipt spine.Receipt) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	completed, err := queries.CompleteAction(ctx, dbsql.CompleteActionParams{ExternalID: sql.NullString{String: receipt.ExternalID, Valid: true}, ExternalOutcome: receipt.Outcome, ID: id})
	if err != nil {
		return err
	}
	kind, scope := completed.Kind, completed.ScopeKey
	switch kind {
	case spine.ActionSandboxCreate:
		if scope == "" || receipt.ExternalID != scope {
			err = fmt.Errorf("Sandbox receipt conflicts with its exact identity")
			break
		}
		err = expectOneRows(queries.MarkSandboxCreated(ctx, scope))
	case spine.ActionRouteCreate:
		route, getErr := queries.GetRouteBySandbox(ctx, scope)
		if getErr != nil {
			err = getErr
			break
		}
		if receipt.ExternalID != route.ID || receipt.Outcome != scope {
			err = fmt.Errorf("provider route receipt conflicts with its exact Sandbox identity")
			break
		}
		err = expectOneRows(queries.MarkRouteActive(ctx, dbsql.MarkRouteActiveParams{RouteID: route.ID, SandboxID: scope}))
	case spine.ActionRouteRevoke:
		route, getErr := queries.GetRouteBySandbox(ctx, scope)
		if getErr != nil {
			err = getErr
			break
		}
		if receipt.Outcome != "revoked" || (receipt.ExternalID != "absent" && receipt.ExternalID != route.ID) {
			err = fmt.Errorf("route cleanup receipt conflicts with its exact route")
			break
		}
		err = expectOneRows(queries.MarkRouteRevoked(ctx, dbsql.MarkRouteRevokedParams{RouteID: route.ID, SandboxID: scope}))
	case spine.ActionSandboxDelete:
		if scope == "" || receipt.ExternalID != scope || receipt.Outcome != "deleted" {
			err = fmt.Errorf("Sandbox cleanup receipt conflicts with its exact identity")
			break
		}
		err = expectOneRows(queries.MarkSandboxDeleted(ctx, scope))
	case spine.ActionReviewWorkspaceCreate:
		if scope == "" || strings.TrimSpace(receipt.ExternalID) == "" {
			err = fmt.Errorf("review workspace receipt has no materialized path")
			break
		}
		expected, getErr := queries.GetReviewRevisionBySandbox(ctx, scope)
		if getErr != nil {
			err = getErr
			break
		}
		parts := strings.Fields(receipt.Outcome)
		if len(parts) != 3 || parts[2] != "clean" || !expected.Valid || parts[0] != expected.String || !ValidRevision(parts[1]) {
			err = fmt.Errorf("review workspace receipt conflicts with its exact Revision/tree/clean state")
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) UncertainAction(ctx context.Context, id string) error {
	return dbsql.New(s.DB).MarkActionUncertain(ctx, id)
}

func (s Store) NextDelivery(ctx context.Context, jobID string) (*spine.Delivery, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	workflowPhase, err := queries.GetWorkflowPhaseForUpdate(ctx, jobID)
	if err != nil {
		return nil, err
	}
	// A steer is a distinct priority lane aimed at the active harness Turn. It may
	// overtake older queued follow-ups; the immutable sequence still records
	// admission order, while follow-up turn starts remain FIFO.
	row, err := queries.NextDeliveryCandidate(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	message := spine.Message{
		ID: row.ID, JobID: row.JobID, FromKind: spine.MessageFromKind(row.FromKind), FromID: row.FromID,
		Sequence: row.Sequence, Input: row.Input, Intent: spine.MessageDeliveryIntent(row.DeliveryIntent), TargetTurnID: row.SteerTargetTurnID,
	}
	runID := spine.AgentRunID(message.ID)
	if _, err := queries.InsertImplementationAgentRun(ctx, dbsql.InsertImplementationAgentRunParams{ID: runID, JobID: jobID, MessageID: message.ID, SandboxID: spine.MainSandboxName(jobID)}); err != nil {
		return nil, err
	}
	runRow, err := queries.GetAgentRunByMessage(ctx, message.ID)
	if err != nil {
		return nil, err
	}
	run := agentRunFromValues(runRow.ID, runRow.JobID, runRow.MessageID, runRow.State, runRow.Harness, runRow.ThreadID, runRow.BaselineRecorded, runRow.BaselineTurnID, runRow.TurnID, runRow.TurnOutcome, runRow.Attention, runRow.Role)
	bindings, err := queries.ListImplementationThreadBindings(ctx, jobID)
	if err != nil {
		return nil, err
	}
	for i, binding := range bindings {
		if i > 0 && (binding.Harness != bindings[0].Harness || binding.ThreadID != bindings[0].ThreadID) ||
			run.ThreadID != "" && (run.Harness != binding.Harness.String || run.ThreadID != binding.ThreadID.String) {
			return nil, fmt.Errorf("Job %s implementation AgentRuns disagree on their harness Thread", jobID)
		}
	}
	allowed := run.Role == "implement" && (workflowPhase == "setup" || workflowPhase == "implementing" || workflowPhase == "review-feedback")
	if !allowed {
		reason := fmt.Sprintf("preserved admission sequence %d as attention: %s AgentRun cannot start during workflow phase %s", message.Sequence, run.Role, workflowPhase)
		if err := queries.BlockAgentRunDelivery(ctx, dbsql.BlockAgentRunDeliveryParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: run.ID}); err != nil {
			return nil, err
		}
		if err := queries.BlockDelivery(ctx, dbsql.BlockDeliveryParams{Reason: sql.NullString{String: reason, Valid: true}, JobID: jobID}); err != nil {
			return nil, err
		}
		run.State, run.Attention = spine.AgentRunUncertain, reason
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &spine.Delivery{Message: message, AgentRun: run}, nil
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
	state := spine.AgentRunActive
	outcome := ""
	attention := ""
	if status == "completed" {
		state, outcome = spine.AgentRunCompleted, status
	} else if status == "failed" {
		state, outcome = spine.AgentRunFailed, status
	} else if status == "interrupted" {
		state, outcome = spine.AgentRunInterrupted, status
	} else if status != "running" && status != "inProgress" {
		state = spine.AgentRunUncertain
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
	if run.State == spine.AgentRunCompleted || run.State == spine.AgentRunFailed || run.State == spine.AgentRunInterrupted {
		if run.State != state || run.TurnOutcome != outcome || run.Harness == "" || run.ThreadID == "" || run.TurnID == "" {
			return fmt.Errorf("AgentRun %s terminal outcome conflicts with observed harness status %q", runID, status)
		}
		return tx.Commit()
	}
	if run.State == spine.AgentRunPending {
		return fmt.Errorf("AgentRun %s must be prepared before binding a harness Turn", runID)
	}
	if run.Role == "implement" {
		bindings, err := queries.ListImplementationThreadBindings(ctx, run.JobID)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			if binding.Harness.String != harness || binding.ThreadID.String != threadID {
				return fmt.Errorf("AgentRun %s conflicts with Job %s implementation Thread", runID, run.JobID)
			}
		}
	} else {
		inherited, err := queries.ImplementationThreadExists(ctx, dbsql.ImplementationThreadExistsParams{Harness: sql.NullString{String: harness, Valid: true}, ThreadID: sql.NullString{String: threadID, Valid: true}})
		if err != nil {
			return err
		}
		if inherited {
			return fmt.Errorf("review AgentRun %s cannot inherit an implementation Thread", runID)
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

func (s Store) Messages(ctx context.Context, jobID string) ([]spine.MessageView, error) {
	rows, err := dbsql.New(s.DB).ListMessages(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var views []spine.MessageView
	var roles []string
	for _, row := range rows {
		view := spine.MessageView{
			Message:    messageFromValues(row.ID, row.JobID, row.FromKind, row.FromID, row.Sequence, row.Input, row.DeliveryIntent, row.SteerTargetTurnID),
			AgentRunID: row.AgentRunID, State: row.State, Harness: row.Harness, ThreadID: row.ThreadID, TurnID: row.TurnID,
			TurnOutcome: row.TurnOutcome, Attention: row.Attention, Delivered: row.Delivered,
		}
		views = append(views, view)
		roles = append(roles, row.Role)
	}
	var blocker *spine.MessageView
	for i := range views {
		view := &views[i]
		if roles[i] != "implement" {
			continue
		}
		if blocker != nil && !(view.Intent == spine.MessageSteer && blocker.State == spine.AgentRunActive && view.TargetTurnID == blocker.TurnID) {
			view.BlockingSeq = blocker.Sequence
			view.BlockingReason = string(blocker.State)
			if blocker.Attention != "" {
				view.BlockingReason += ": " + blocker.Attention
			}
		}
		turnStartActive := (view.State == spine.AgentRunActive || view.State == spine.AgentRunUncertain) && (view.Intent == spine.MessageFollow || view.TurnID != "" && view.TurnID != view.TargetTurnID)
		if blocker == nil && ((!view.Delivered && view.State != spine.AgentRunCompleted) || turnStartActive) {
			blocker = view
		}
	}
	return views, nil
}

func (s Store) HarnessMutationDelivery(ctx context.Context, jobID string) (*spine.Delivery, error) {
	row, err := dbsql.New(s.DB).GetHarnessMutationDelivery(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	delivery := spine.Delivery{
		Message:  messageFromValues(row.MessageID, row.JobID, row.FromKind, row.FromID, row.Sequence, row.Input, row.DeliveryIntent, row.SteerTargetTurnID),
		AgentRun: agentRunFromValues(row.AgentRunID, row.AgentRunJobID, row.AgentRunMessageID, row.State, row.Harness, row.ThreadID, row.BaselineRecorded, row.BaselineTurnID, row.TurnID, row.TurnOutcome, row.Attention, row.Role),
	}
	return &delivery, nil
}

func (s Store) SetCleanupAttention(ctx context.Context, jobID, detail string) error {
	detail = strings.TrimSpace(detail)
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return expectOneRows(dbsql.New(s.DB).SetCleanupAttention(ctx, dbsql.SetCleanupAttentionParams{Detail: detail, JobID: jobID}))
}

func (s Store) CompleteCleanup(ctx context.Context, jobID string) error {
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
	if job.AdmissionOpen || job.CleanupState != spine.CleanupScheduled || job.WorkflowAttention != "" {
		return fmt.Errorf("cleanup cannot complete while admission, workflow attention, or cleanup scheduling remains unsettled")
	}
	if job.CleanupTaskID == "" {
		return fmt.Errorf("cleanup cannot complete without its exact attached cleanup task")
	}
	harnessMutations, err := queries.CountImplementationHarnessMutations(ctx, jobID)
	if err != nil {
		return err
	}
	if harnessMutations != 0 {
		return fmt.Errorf("cleanup cannot complete with %d unsettled implementation harness mutations", harnessMutations)
	}
	runs, err := queries.ListJobAgentRuns(ctx, jobID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.State != spine.AgentRunCompleted && run.State != spine.AgentRunFailed && run.State != spine.AgentRunInterrupted {
			return fmt.Errorf("cleanup cannot complete with unsettled AgentRun %s", run.ID)
		}
	}
	unsettled, err := queries.CountUnsettledJobResources(ctx, jobID)
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

func (s Store) Actions(ctx context.Context, jobID string) ([]ActionView, error) {
	rows, err := dbsql.New(s.DB).ListActions(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var actions []ActionView
	for _, row := range rows {
		actions = append(actions, ActionView{ID: row.ID, Kind: row.Kind, State: row.State, ExternalID: row.ExternalID, Scope: row.ScopeKey, EvidenceDigest: row.EvidenceDigest})
	}
	return actions, nil
}

func (s Store) Checks(ctx context.Context, jobID string) ([]spine.Check, error) {
	rows, err := dbsql.New(s.DB).ListChecks(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var checks []spine.Check
	for _, row := range rows {
		checks = append(checks, checkFromValues(row.ID, row.JobID, row.Name, row.Command, row.Revision, row.State, row.ExitCode, row.EvidenceID, row.EvidenceDigest, row.StartedAt, row.FinishedAt))
	}
	return checks, nil
}

func (s Store) Evidence(ctx context.Context, jobID string) ([]spine.Evidence, error) {
	rows, err := dbsql.New(s.DB).ListEvidence(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var records []spine.Evidence
	for _, row := range rows {
		records = append(records, spine.Evidence{ID: row.ID, Digest: row.Digest, ByteSize: row.ByteSize, MediaType: row.MediaType, Producer: row.Producer, Kind: row.Kind, ActionID: row.ActionID, AgentRunID: row.AgentRunID, CheckID: row.CheckID, Revision: row.Revision, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt})
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
