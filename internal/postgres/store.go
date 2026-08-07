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

	"github.com/aphronio/dorf/internal/spine"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var ErrNotFound = errors.New("Dorf Job not found")
var fullCommitOID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

const (
	AbsurdReleaseCommit = "550d3b9e6f9382d96178de6ab8c90c7f8edf2227"
	AbsurdSchemaURL     = "https://raw.githubusercontent.com/earendil-works/absurd/" + AbsurdReleaseCommit + "/sql/absurd.sql"
	AbsurdSchemaSHA256  = "d34309370c539f3a51f2b36b69b1f77551f8e4a14480a1c8def8bb8f40fd9aab"
	MessageTaskName     = "dorf-job-messages-v2"
	initialCallerID     = "dorf:initial"
	legacyRunTaskName   = "dorf-job-spine-v1"
)

func MessageTaskKey(jobID string) string { return "run:v2:" + jobID }

type Store struct{ DB *sql.DB }

type NewJob struct {
	AdmissionKey       string
	Goal               string
	Repository         string
	Revision           string
	Branch             string
	ProviderConnection string
	Model              string
	ReasoningEffort    string
}

type NewMessage struct {
	JobID    string
	CallerID string
	Input    string
}

type ActionView struct {
	ID             string            `json:"id"`
	MessageID      string            `json:"message_id,omitempty"`
	Kind           spine.ActionKind  `json:"kind"`
	State          spine.ActionState `json:"state"`
	ExternalID     string            `json:"external_id,omitempty"`
	Attempts       int               `json:"attempts"`
	Scope          string            `json:"scope,omitempty"`
	EvidenceDigest string            `json:"evidence_digest,omitempty"`
}

type TaskEvidence struct {
	TaskID      string `json:"task_id"`
	State       string `json:"state"`
	Attempts    int    `json:"attempts"`
	Checkpoints int    `json:"checkpoints"`
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
	for _, name := range []string{"001_dorf.sql", "002_run_terminal.sql", "003_exactly_once_messages.sql", "004_revision_evidence.sql", "005_commit_admission.sql", "006_setup_retry.sql", "007_review_policy.sql"} {
		var migrationsTable bool
		if err := tx.QueryRowContext(ctx, `select to_regclass('dorf.schema_migrations') is not null`).Scan(&migrationsTable); err != nil {
			return err
		}
		if migrationsTable {
			var applied bool
			if err := tx.QueryRowContext(ctx, `select exists(select 1 from dorf.schema_migrations where name=$1)`, name).Scan(&applied); err != nil {
				return err
			}
			if applied {
				continue
			}
		}
		contents, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			return fmt.Errorf("apply Dorf migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `insert into dorf.schema_migrations(name) values ($1) on conflict do nothing`, name); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `select absurd.create_queue('dorf_jobs')`); err != nil {
		return fmt.Errorf("create Absurd queue dorf_jobs: %w", err)
	}
	return tx.Commit()
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
	if input.AdmissionKey == "" || strings.TrimSpace(input.Goal) == "" || input.Repository == "" || input.Branch == "" || input.ProviderConnection == "" || input.Model == "" {
		return spine.Job{}, false, fmt.Errorf("admission requires key, complete goal, repository, branch, Provider Connection, and model")
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
	result, err := tx.ExecContext(ctx, `
		insert into dorf.jobs(id,admission_key,goal,repository,revision,starting_revision,branch,provider_connection,model,reasoning_effort)
		values($1,$2,$3,$4,$5,$5,$6,$7,$8,$9) on conflict(admission_key) do nothing`,
		id, input.AdmissionKey, input.Goal, input.Repository, input.Revision, input.Branch,
		input.ProviderConnection, input.Model, input.ReasoningEffort)
	if err != nil {
		return spine.Job{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return spine.Job{}, false, err
	}
	var stored NewJob
	var storedID string
	if err := tx.QueryRowContext(ctx, `select id,admission_key,goal,repository,revision,branch,provider_connection,model,reasoning_effort from dorf.jobs where admission_key=$1 for update`, input.AdmissionKey).Scan(
		&storedID, &stored.AdmissionKey, &stored.Goal, &stored.Repository, &stored.Revision, &stored.Branch,
		&stored.ProviderConnection, &stored.Model, &stored.ReasoningEffort); err != nil {
		return spine.Job{}, false, err
	}
	if storedID != id || stored != input {
		return spine.Job{}, false, fmt.Errorf("admission key %q is already bound to different complete Job input", input.AdmissionKey)
	}
	messageID := spine.MessageID(id, initialCallerID)
	if _, err := tx.ExecContext(ctx, `insert into dorf.job_messages(id,job_id,caller_id,sequence,input) values($1,$2,$3,1,$4) on conflict(job_id,caller_id) do nothing`, messageID, id, initialCallerID, input.Goal); err != nil {
		return spine.Job{}, false, err
	}
	var existingID, existingInput string
	var sequence int64
	if err := tx.QueryRowContext(ctx, `select id,sequence,input from dorf.job_messages where job_id=$1 and caller_id=$2`, id, initialCallerID).Scan(&existingID, &sequence, &existingInput); err != nil {
		return spine.Job{}, false, err
	}
	if sequence != 1 || existingInput != input.Goal {
		return spine.Job{}, false, fmt.Errorf("Job %s initial message conflicts with complete admission input", id)
	}
	actionID := spine.TurnActionID(existingID)
	if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,message_id,kind,state) values($1,$2,$3,$4,'pending') on conflict do nothing`, actionID, id, existingID, spine.ActionTurnStart); err != nil {
		return spine.Job{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `select id from dorf.actions where message_id=$1 and kind=$2`, existingID, spine.ActionTurnStart).Scan(&actionID); err != nil {
		return spine.Job{}, false, err
	}
	runID := spine.AgentRunID(existingID)
	if _, err := tx.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,message_id,action_id,role,state) values($1,$2,$3,$4,'implement','pending') on conflict do nothing`, runID, id, existingID, actionID); err != nil {
		return spine.Job{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.revisions(job_id,oid,branch,generation) values($1,$2,$3,0) on conflict do nothing`, id, input.Revision, input.Branch); err != nil {
		return spine.Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Job{}, false, err
	}
	job, err := s.Job(ctx, id)
	return job, rows == 1, err
}

func (s Store) AdmitMessage(ctx context.Context, input NewMessage) (spine.Message, bool, error) {
	return s.admitMessage(ctx, input, false)
}

func (s Store) admitMessage(ctx context.Context, input NewMessage, allowSetupRetry bool) (spine.Message, bool, error) {
	input, err := normalizeMessage(input, allowSetupRetry)
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

func normalizeMessage(input NewMessage, allowSetupRetry bool) (NewMessage, error) {
	input.JobID = strings.TrimSpace(input.JobID)
	input.CallerID = strings.TrimSpace(input.CallerID)
	if input.JobID == "" || input.CallerID == "" || strings.TrimSpace(input.Input) == "" {
		return NewMessage{}, fmt.Errorf("message admission requires Job ID, caller ID, and complete input")
	}
	if len(input.CallerID) > 256 || strings.HasPrefix(input.CallerID, "dorf:") && !allowSetupRetry {
		return NewMessage{}, fmt.Errorf("caller ID must be at most 256 characters and must not use the reserved dorf: prefix")
	}
	if allowSetupRetry && !strings.HasPrefix(input.CallerID, "dorf:setup-retry:") {
		return NewMessage{}, fmt.Errorf("internal setup retry caller ID is invalid")
	}
	if len(input.Input) > 1<<20 {
		return NewMessage{}, fmt.Errorf("message input exceeds 1 MiB")
	}
	return input, nil
}

func admitMessageTx(ctx context.Context, tx *sql.Tx, input NewMessage) (spine.Message, bool, error) {
	var admissionOpen bool
	var workflowPhase string
	if err := tx.QueryRowContext(ctx, `select admission_open,workflow_phase from dorf.jobs where id=$1 for update`, input.JobID).Scan(&admissionOpen, &workflowPhase); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return spine.Message{}, false, ErrNotFound
		}
		return spine.Message{}, false, err
	}
	var message spine.Message
	err := tx.QueryRowContext(ctx, `select id,job_id,caller_id,sequence,input from dorf.job_messages where job_id=$1 and caller_id=$2`, input.JobID, input.CallerID).Scan(&message.ID, &message.JobID, &message.CallerID, &message.Sequence, &message.Input)
	if err == nil {
		if message.Input != input.Input {
			return spine.Message{}, false, fmt.Errorf("caller ID %q is already bound to different complete message input", input.CallerID)
		}
		return message, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spine.Message{}, false, err
	}
	if !admissionOpen {
		return spine.Message{}, false, fmt.Errorf("Job %s admission is closed for cleanup", input.JobID)
	}
	if workflowPhase != "setup" && workflowPhase != "implementing" {
		return spine.Message{}, false, fmt.Errorf("Job %s no longer accepts implementation steering after its first Revision; deterministic Check repair is workflow-owned", input.JobID)
	}
	if err := tx.QueryRowContext(ctx, `select coalesce(max(sequence),0)+1 from dorf.job_messages where job_id=$1`, input.JobID).Scan(&message.Sequence); err != nil {
		return spine.Message{}, false, err
	}
	message.ID = spine.MessageID(input.JobID, input.CallerID)
	message.JobID, message.CallerID, message.Input = input.JobID, input.CallerID, input.Input
	if _, err := tx.ExecContext(ctx, `insert into dorf.job_messages(id,job_id,caller_id,sequence,input) values($1,$2,$3,$4,$5)`, message.ID, message.JobID, message.CallerID, message.Sequence, message.Input); err != nil {
		return spine.Message{}, false, err
	}
	actionID, runID := spine.TurnActionID(message.ID), spine.AgentRunID(message.ID)
	if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,message_id,kind,state) values($1,$2,$3,$4,'pending')`, actionID, message.JobID, message.ID, spine.ActionTurnStart); err != nil {
		return spine.Message{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,message_id,action_id,role,state) values($1,$2,$3,$4,'implement','pending')`, runID, message.JobID, message.ID, actionID); err != nil {
		return spine.Message{}, false, err
	}
	return message, true, nil
}

func (s Store) Job(ctx context.Context, id string) (spine.Job, error) {
	var job spine.Job
	err := s.DB.QueryRowContext(ctx, `
		select j.id,j.admission_key,j.goal,j.repository,j.revision,coalesce(rv.generation,0),j.starting_revision,j.branch,
		       j.provider_connection,j.model,j.reasoning_effort,j.state,j.admission_open,
		       j.cleanup_state,coalesce(j.task_id,''),coalesce(j.cleanup_task_id,''),
		       coalesce(sb.incus_name,''),coalesce(r.route_id,''),coalesce(se.native_session_id,''),
		       coalesce(j.run_terminal_state,''),j.workflow_phase,j.repair_count,coalesce(j.workflow_attention,''),j.review_repair_count
		from dorf.jobs j
		left join dorf.sandboxes sb on sb.job_id=j.id
		left join dorf.routes r on r.job_id=j.id
		left join dorf.sessions se on se.job_id=j.id
		left join dorf.revisions rv on rv.job_id=j.id and rv.oid=j.revision
		where j.id=$1`, id).Scan(
		&job.ID, &job.AdmissionKey, &job.Goal, &job.Repository, &job.Revision, &job.RevisionGeneration, &job.StartingRevision, &job.Branch,
		&job.ProviderConnection, &job.Model, &job.ReasoningEffort, &job.State, &job.AdmissionOpen,
		&job.CleanupState, &job.TaskID, &job.CleanupTaskID, &job.SandboxID, &job.RouteID,
		&job.SessionID, &job.RunTerminalState, &job.WorkflowPhase, &job.RepairCount, &job.WorkflowAttention, &job.ReviewRepairCount)
	if errors.Is(err, sql.ErrNoRows) {
		return spine.Job{}, ErrNotFound
	}
	return job, err
}

// WithJobFence serializes native and other external mutation for one Job
// independently of an expiring Absurd claim. Message admission intentionally
// does not take this long-lived fence.
func (s Store) WithJobFence(ctx context.Context, jobID string, fn func() error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock(hashtextextended('dorf-job-effect:' || $1, 0))`, jobID); err != nil {
		return fmt.Errorf("acquire Job execution fence: %w", err)
	}
	if err := fn(); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) SetTaskID(ctx context.Context, jobID, taskID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set task_id=coalesce(task_id,$2) where id=$1 and (task_id is null or task_id=$2)`, jobID, taskID))
}

// CheckMessageTaskAttachment prevents spawning a v2 consumer while an active
// or unrelated task is still authoritative for the Job.
func (s Store) CheckMessageTaskAttachment(ctx context.Context, jobID string) error {
	var taskID string
	var admissionOpen bool
	if err := s.DB.QueryRowContext(ctx, `select coalesce(task_id,''),admission_open from dorf.jobs where id=$1`, jobID).Scan(&taskID, &admissionOpen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !admissionOpen {
		return fmt.Errorf("Job %s admission is closed; refusing to attach a message consumer", jobID)
	}
	if taskID == "" {
		return nil
	}
	task, err := s.runTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("inspect current Absurd run task %s: %w", taskID, err)
	}
	return validateReplaceableRunTask(jobID, task, true)
}

// AttachMessageTask replaces only an exact terminal v1 predecessor. The old
// Absurd task remains untouched as historical execution evidence.
func (s Store) AttachMessageTask(ctx context.Context, jobID, proposedTaskID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentTaskID string
	var admissionOpen bool
	if err := tx.QueryRowContext(ctx, `select coalesce(task_id,''),admission_open from dorf.jobs where id=$1 for update`, jobID).Scan(&currentTaskID, &admissionOpen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !admissionOpen {
		return fmt.Errorf("Job %s admission closed before its message task could be attached", jobID)
	}
	proposed, err := scanRunTask(tx.QueryRowContext(ctx, `select task_id::text,task_name,state,coalesce(params->>'job_id',''),coalesce(idempotency_key,'') from absurd.t_dorf_jobs where task_id=$1::uuid`, proposedTaskID))
	if err != nil {
		return fmt.Errorf("inspect proposed Absurd message task %s: %w", proposedTaskID, err)
	}
	if err := validateCurrentMessageTask(jobID, proposed); err != nil {
		return err
	}
	if currentTaskID != "" && currentTaskID != proposedTaskID {
		current, err := scanRunTask(tx.QueryRowContext(ctx, `select task_id::text,task_name,state,coalesce(params->>'job_id',''),coalesce(idempotency_key,'') from absurd.t_dorf_jobs where task_id=$1::uuid`, currentTaskID))
		if err != nil {
			return fmt.Errorf("inspect current Absurd run task %s: %w", currentTaskID, err)
		}
		if err := validateReplaceableRunTask(jobID, current, false); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `update dorf.jobs set task_id=$2 where id=$1`, jobID, proposedTaskID); err != nil {
		return err
	}
	return tx.Commit()
}

type runTaskRecord struct {
	ID, Name, State, JobID, IdempotencyKey string
}

func (s Store) runTask(ctx context.Context, taskID string) (runTaskRecord, error) {
	return scanRunTask(s.DB.QueryRowContext(ctx, `select task_id::text,task_name,state,coalesce(params->>'job_id',''),coalesce(idempotency_key,'') from absurd.t_dorf_jobs where task_id=$1::uuid`, taskID))
}

type rowScanner interface{ Scan(...any) error }

func scanRunTask(row rowScanner) (runTaskRecord, error) {
	var task runTaskRecord
	err := row.Scan(&task.ID, &task.Name, &task.State, &task.JobID, &task.IdempotencyKey)
	return task, err
}

func validateCurrentMessageTask(jobID string, task runTaskRecord) error {
	if task.Name != MessageTaskName || task.JobID != jobID || task.IdempotencyKey != MessageTaskKey(jobID) {
		return fmt.Errorf("Absurd task %s is not the expected %s consumer for Job %s", task.ID, MessageTaskName, jobID)
	}
	if task.State != "pending" && task.State != "running" && task.State != "sleeping" {
		return fmt.Errorf("Absurd message task %s is %s, not an active consumer for open Job %s", task.ID, task.State, jobID)
	}
	return nil
}

func validateReplaceableRunTask(jobID string, task runTaskRecord, allowCurrentV2 bool) error {
	if allowCurrentV2 && task.Name == MessageTaskName && task.JobID == jobID && task.IdempotencyKey == MessageTaskKey(jobID) {
		return validateCurrentMessageTask(jobID, task)
	}
	if task.Name != legacyRunTaskName || task.JobID != jobID || task.IdempotencyKey != "run:"+jobID {
		return fmt.Errorf("current Absurd task %s does not belong to Job %s as the expected %s predecessor", task.ID, jobID, legacyRunTaskName)
	}
	if task.State != "completed" && task.State != "failed" && task.State != "cancelled" {
		return fmt.Errorf("current predecessor task %s is %s; refusing to replace a nonterminal run", task.ID, task.State)
	}
	return nil
}

func (s Store) StartRun(ctx context.Context, jobID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set state='running' where id=$1 and admission_open and cleanup_state='pending'`, jobID))
}

func (s Store) CloseAdmission(ctx context.Context, jobID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set admission_open=false where id=$1`, jobID))
}

func (s Store) SetCleanupTaskID(ctx context.Context, jobID, taskID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set cleanup_task_id=coalesce(cleanup_task_id,$2),cleanup_state=case when cleanup_state='complete' or cleaned_at is not null then 'complete' else 'scheduled' end where id=$1 and (cleanup_task_id is null or cleanup_task_id=$2)`, jobID, taskID))
}

func (s Store) BeginAction(ctx context.Context, jobID string, kind spine.ActionKind) (spine.Action, error) {
	desiredID := spine.ActionID(jobID, kind)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Action{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state) values($1,$2,$3,'pending') on conflict do nothing`, desiredID, jobID, kind); err != nil {
		return spine.Action{}, err
	}
	var action spine.Action
	if err := tx.QueryRowContext(ctx, `update dorf.actions set attempts=attempts+case when state in ('succeeded','failed') then 0 else 1 end,updated_at=clock_timestamp() where job_id=$1 and kind=$2 and message_id is null and scope_key='' returning id,job_id,coalesce(message_id,''),kind,state,coalesce(external_id,''),coalesce(external_outcome,''),scope_key`, jobID, kind).Scan(&action.ID, &action.JobID, &action.MessageID, &action.Kind, &action.State, &action.ExternalID, &action.Outcome, &action.Scope); err != nil {
		return spine.Action{}, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Action{}, err
	}
	return action, nil
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
	desiredID := spine.ActionID(jobID, spine.ActionRepositorySetup)
	var currentID string
	if err := tx.QueryRowContext(ctx, `select coalesce(setup_action_id,'') from dorf.jobs where id=$1 for update`, jobID).Scan(&currentID); err != nil {
		return spine.Action{}, err
	}
	if currentID == "" {
		if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state) values($1,$2,$3,'pending') on conflict do nothing`, desiredID, jobID, spine.ActionRepositorySetup); err != nil {
			return spine.Action{}, err
		}
		if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set setup_action_id=$2 where id=$1 and setup_action_id is null`, jobID, desiredID)); err != nil {
			return spine.Action{}, err
		}
		currentID = desiredID
	}
	var action spine.Action
	if err := tx.QueryRowContext(ctx, `update dorf.actions set attempts=attempts+case when state in ('succeeded','failed') then 0 else 1 end,updated_at=clock_timestamp() where id=$1 and job_id=$2 and kind=$3 and message_id is null returning id,job_id,coalesce(message_id,''),kind,state,coalesce(external_id,''),coalesce(external_outcome,''),scope_key`, currentID, jobID, spine.ActionRepositorySetup).Scan(&action.ID, &action.JobID, &action.MessageID, &action.Kind, &action.State, &action.ExternalID, &action.Outcome, &action.Scope); err != nil {
		return spine.Action{}, err
	}
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
	if len(retryID) > 239 || strings.HasPrefix(retryID, "dorf:") {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("setup retry identity must be at most 239 characters and must not use the reserved dorf: prefix")
	}
	messageInput, err := normalizeMessage(NewMessage{JobID: jobID, CallerID: "dorf:setup-retry:" + retryID, Input: input}, true)
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	defer tx.Rollback()
	var phase, currentID string
	var admissionOpen bool
	if err := tx.QueryRowContext(ctx, `select workflow_phase,coalesce(setup_action_id,''),admission_open from dorf.jobs where id=$1 for update`, jobID).Scan(&phase, &currentID, &admissionOpen); err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	desiredID := spine.ScopedActionID(jobID, spine.ActionRepositorySetup, retryID)
	var existing spine.Action
	err = tx.QueryRowContext(ctx, `select id,job_id,coalesce(message_id,''),kind,state,coalesce(external_id,''),coalesce(external_outcome,''),scope_key from dorf.actions where id=$1 and job_id=$2 and kind=$3`, desiredID, jobID, spine.ActionRepositorySetup).Scan(&existing.ID, &existing.JobID, &existing.MessageID, &existing.Kind, &existing.State, &existing.ExternalID, &existing.Outcome, &existing.Scope)
	if err == nil {
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
	if !admissionOpen {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("Job %s admission is closed; repository setup cannot be retried", jobID)
	}
	if phase != "blocked" || currentID == "" {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("Job %s repository setup retry is not admissible during workflow phase %s", jobID, phase)
	}
	var currentState spine.ActionState
	if err := tx.QueryRowContext(ctx, `select state from dorf.actions where id=$1 and job_id=$2 and kind=$3 for update`, currentID, jobID, spine.ActionRepositorySetup).Scan(&currentState); err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	if currentState != spine.ActionFailed {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("Job %s current repository setup Action %s is %s, not terminal failed", jobID, currentID, currentState)
	}
	result, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state,scope_key) values($1,$2,$3,'pending',$4) on conflict do nothing`, desiredID, jobID, spine.ActionRepositorySetup, retryID)
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	createdRows, err := result.RowsAffected()
	if err != nil {
		return spine.Action{}, spine.Message{}, false, err
	}
	if createdRows != 1 {
		return spine.Action{}, spine.Message{}, false, fmt.Errorf("setup retry identity %q was already used by another generation", retryID)
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set setup_action_id=$2,workflow_phase='setup',workflow_attention=null where id=$1 and setup_action_id=$3 and workflow_phase='blocked'`, jobID, desiredID, currentID)); err != nil {
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

// BeginCommit atomically closes implementation steering and reserves the
// Revision Action only after every already-admitted FIFO input is terminal.
// The additive committing phase is intentionally compatible with live v2
// message tasks: the same executor resumes it from the stable scoped Action.
func (s Store) BeginCommit(ctx context.Context, jobID, parent string) (spine.Action, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Action{}, false, err
	}
	defer tx.Rollback()
	var revision, phase string
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase from dorf.jobs where id=$1 for update`, jobID).Scan(&revision, &phase); err != nil {
		return spine.Action{}, false, err
	}
	if revision != parent {
		return spine.Action{}, false, fmt.Errorf("commit parent %s conflicts with current Revision %s", parent, revision)
	}
	if phase != "implementing" && phase != "repairing" && phase != "review-repairing" && phase != "committing" {
		return spine.Action{}, false, fmt.Errorf("Revision commit is not admissible during workflow phase %s", phase)
	}
	if phase != "committing" {
		var unsettled int
		if err := tx.QueryRowContext(ctx, `select count(*) from dorf.job_messages m left join dorf.agent_runs ar on ar.message_id=m.id where m.job_id=$1 and coalesce(ar.state,'') <> 'completed'`, jobID).Scan(&unsettled); err != nil {
			return spine.Action{}, false, err
		}
		if unsettled != 0 {
			if err := tx.Commit(); err != nil {
				return spine.Action{}, false, err
			}
			return spine.Action{}, false, nil
		}
		if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='committing' where id=$1 and revision=$2 and workflow_phase in ('implementing','repairing','review-repairing')`, jobID, parent)); err != nil {
			return spine.Action{}, false, err
		}
	}
	desiredID := spine.ScopedActionID(jobID, spine.ActionRepositoryCommit, parent)
	if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state,scope_key) values($1,$2,$3,'pending',$4) on conflict do nothing`, desiredID, jobID, spine.ActionRepositoryCommit, parent); err != nil {
		return spine.Action{}, false, err
	}
	var action spine.Action
	if err := tx.QueryRowContext(ctx, `update dorf.actions set attempts=attempts+case when state in ('succeeded','failed') then 0 else 1 end,updated_at=clock_timestamp() where id=$1 and job_id=$2 and kind=$3 and scope_key=$4 and message_id is null returning id,job_id,coalesce(message_id,''),kind,state,coalesce(external_id,''),coalesce(external_outcome,''),scope_key`, desiredID, jobID, spine.ActionRepositoryCommit, parent).Scan(&action.ID, &action.JobID, &action.MessageID, &action.Kind, &action.State, &action.ExternalID, &action.Outcome, &action.Scope); err != nil {
		return spine.Action{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Action{}, false, err
	}
	return action, true, nil
}

func insertEvidence(ctx context.Context, tx *sql.Tx, jobID string, evidence spine.Evidence) error {
	_, err := tx.ExecContext(ctx, `
		insert into dorf.evidence(id,job_id,digest,byte_size,media_type,producer,provenance,kind,action_id,check_id,revision,started_at,finished_at)
		values($1,$2,$3,$4,$5,$6,$7,$8,nullif($9,''),nullif($10,''),nullif($11,''),$12,$13)
		on conflict(id) do nothing`, evidence.ID, jobID, evidence.Digest, evidence.ByteSize, evidence.MediaType,
		evidence.Producer, evidence.Provenance, evidence.Kind, evidence.ActionID, evidence.CheckID,
		evidence.Revision, nullableTime(evidence.StartedAt), nullableTime(evidence.FinishedAt))
	if err != nil {
		return err
	}
	var storedJobID, digest, mediaType, producer, provenance, kind, actionID, checkID, revision string
	var size int64
	var startedAt, finishedAt time.Time
	if err := tx.QueryRowContext(ctx, `select job_id,digest,byte_size,media_type,producer,provenance,kind,coalesce(action_id,''),coalesce(check_id,''),coalesce(revision,''),started_at,finished_at from dorf.evidence where id=$1`, evidence.ID).Scan(&storedJobID, &digest, &size, &mediaType, &producer, &provenance, &kind, &actionID, &checkID, &revision, &startedAt, &finishedAt); err != nil {
		return err
	}
	if storedJobID != jobID || digest != evidence.Digest || size != evidence.ByteSize || mediaType != evidence.MediaType || producer != evidence.Producer || provenance != evidence.Provenance || kind != evidence.Kind || actionID != evidence.ActionID || checkID != evidence.CheckID || revision != evidence.Revision || !startedAt.Equal(evidence.StartedAt) || !finishedAt.Equal(evidence.FinishedAt) {
		return fmt.Errorf("Evidence identity %s conflicts with immutable retained metadata or content", evidence.ID)
	}
	return nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
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
	var jobID, currentSetupID string
	var kind spine.ActionKind
	if err := tx.QueryRowContext(ctx, `select a.job_id,a.kind,coalesce(j.setup_action_id,'') from dorf.actions a join dorf.jobs j on j.id=a.job_id where a.id=$1 for update of a,j`, actionID).Scan(&jobID, &kind, &currentSetupID); err != nil {
		return err
	}
	if kind != spine.ActionRepositorySetup || evidence.ActionID != actionID || currentSetupID != actionID {
		return fmt.Errorf("setup Evidence does not match its Action")
	}
	if err := insertEvidence(ctx, tx, jobID, evidence); err != nil {
		return err
	}
	var startingRevision string
	if err := tx.QueryRowContext(ctx, `select starting_revision from dorf.jobs where id=$1`, jobID).Scan(&startingRevision); err != nil {
		return err
	}
	commands := append([]spine.DeclaredCheck{{Name: "prepare", Command: observation.Command}}, checks...)
	for _, command := range commands {
		if command.Name != "prepare" && command.Name != "check" && command.Name != "smoke" || strings.TrimSpace(command.Command) == "" {
			return fmt.Errorf("invalid pinned repository command %q", command.Name)
		}
		if _, err := tx.ExecContext(ctx, `insert into dorf.repository_commands(job_id,name,command,starting_revision) values($1,$2,$3,$4) on conflict do nothing`, jobID, command.Name, command.Command, startingRevision); err != nil {
			return err
		}
		var storedCommand, storedRevision string
		if err := tx.QueryRowContext(ctx, `select command,starting_revision from dorf.repository_commands where job_id=$1 and name=$2`, jobID, command.Name).Scan(&storedCommand, &storedRevision); err != nil {
			return err
		}
		if storedCommand != command.Command || storedRevision != startingRevision {
			return fmt.Errorf("repository command %s conflicts with its pinned starting Revision", command.Name)
		}
	}
	state, phase, attention := "succeeded", "implementing", ""
	if observation.ExitCode != 0 {
		state, phase = "failed", "blocked"
		attention = fmt.Sprintf("repository setup failed with exit %d; Evidence %s", observation.ExitCode, evidence.Digest)
	}
	if _, err := tx.ExecContext(ctx, `update dorf.actions set state=$2,external_id=$3,external_outcome=$4,updated_at=clock_timestamp() where id=$1`, actionID, state, evidence.Digest, fmt.Sprintf("exit=%d", observation.ExitCode)); err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase=$2,workflow_attention=nullif($3,'') where id=$1 and workflow_phase in ('setup','blocked')`, jobID, phase, attention)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) DeclaredChecks(ctx context.Context, jobID string) ([]spine.DeclaredCheck, error) {
	rows, err := s.DB.QueryContext(ctx, `select name,command from dorf.repository_commands where job_id=$1 and name in ('check','smoke') order by case name when 'check' then 1 else 2 end`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checks []spine.DeclaredCheck
	for rows.Next() {
		var check spine.DeclaredCheck
		if err := rows.Scan(&check.Name, &check.Command); err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(checks) == 0 {
		return nil, fmt.Errorf("pinned repository contract has no Checks")
	}
	return checks, nil
}

func (s Store) RecordRevision(ctx context.Context, actionID string, observation spine.CommitObservation, evidence spine.Evidence) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var jobID, scope, currentRevision, branch, phase string
	var kind spine.ActionKind
	if err := tx.QueryRowContext(ctx, `select a.job_id,a.kind,a.scope_key,j.revision,j.branch,j.workflow_phase from dorf.actions a join dorf.jobs j on j.id=a.job_id where a.id=$1 for update of a,j`, actionID).Scan(&jobID, &kind, &scope, &currentRevision, &branch, &phase); err != nil {
		return err
	}
	if kind != spine.ActionRepositoryCommit || scope != observation.Parent || currentRevision != observation.Parent || branch != observation.Branch || phase != "committing" || evidence.ActionID != actionID || evidence.Revision != observation.Revision {
		return fmt.Errorf("Git Revision observation conflicts with durable parent, branch, or Action")
	}
	if err := insertEvidence(ctx, tx, jobID, evidence); err != nil {
		return err
	}
	var generation int
	if err := tx.QueryRowContext(ctx, `select coalesce(max(generation),0)+1 from dorf.revisions where job_id=$1`, jobID).Scan(&generation); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.revisions(job_id,oid,parent_oid,tree_oid,branch,generation,action_id) values($1,$2,$3,$4,$5,$6,$7) on conflict(job_id,oid) do nothing`, jobID, observation.Revision, observation.Parent, observation.Tree, observation.Branch, generation, actionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.actions set state='succeeded',external_id=$2,external_outcome=$3,updated_at=clock_timestamp() where id=$1`, actionID, observation.Revision, observation.Tree); err != nil {
		return err
	}
	var reviewRepairSource string
	if err := tx.QueryRowContext(ctx, `select coalesce(review_repair_source_run_id,'') from dorf.jobs where id=$1`, jobID).Scan(&reviewRepairSource); err != nil {
		return err
	}
	if reviewRepairSource != "" {
		if _, err := tx.ExecContext(ctx, `update dorf.review_findings set adjudication='accepted',stale=true where run_id=$1`, reviewRepairSource); err != nil {
			return err
		}
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set revision=$2,workflow_phase='checking',workflow_attention=null where id=$1 and revision=$3`, jobID, observation.Revision, observation.Parent)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) BeginCheck(ctx context.Context, jobID, revision, name, command string) (spine.Check, error) {
	id := spine.CheckID(jobID, revision, name)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Check{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `insert into dorf.checks(id,job_id,name,command,revision,state) values($1,$2,$3,$4,$5,'running') on conflict do nothing`, id, jobID, name, command, revision); err != nil {
		return spine.Check{}, err
	}
	var check spine.Check
	if err := tx.QueryRowContext(ctx, `update dorf.checks set attempts=attempts+case when state in ('passed','failed') then 0 else 1 end where id=$1 returning id,job_id,name,command,revision,state,coalesce(exit_code,0),coalesce(evidence_id,''),coalesce(started_at,'epoch'),coalesce(finished_at,'epoch')`, id).Scan(&check.ID, &check.JobID, &check.Name, &check.Command, &check.Revision, &check.State, &check.ExitCode, &check.EvidenceID, &check.StartedAt, &check.FinishedAt); err != nil {
		return spine.Check{}, err
	}
	if check.Command != command {
		return spine.Check{}, fmt.Errorf("Check %s command conflicts at Revision %s", name, revision)
	}
	if check.EvidenceID != "" {
		if err := tx.QueryRowContext(ctx, `select digest from dorf.evidence where id=$1`, check.EvidenceID).Scan(&check.EvidenceDigest); err != nil {
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
	var jobID, revision, command string
	if err := tx.QueryRowContext(ctx, `select job_id,revision,command from dorf.checks where id=$1 for update`, check.ID).Scan(&jobID, &revision, &command); err != nil {
		return err
	}
	if jobID != check.JobID || revision != check.Revision || command != observation.Command || evidence.CheckID != check.ID || evidence.Revision != revision {
		return fmt.Errorf("Check observation conflicts with its exact Revision or command")
	}
	if err := insertEvidence(ctx, tx, jobID, evidence); err != nil {
		return err
	}
	state := "passed"
	if observation.ExitCode != 0 {
		state = "failed"
	}
	if _, err := tx.ExecContext(ctx, `update dorf.checks set state=$2,exit_code=$3,evidence_id=$4,started_at=$5,finished_at=$6,attention=null where id=$1`, check.ID, state, observation.ExitCode, evidence.ID, observation.StartedAt, observation.FinishedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) AdmitRepair(ctx context.Context, check spine.Check) (spine.Message, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Message{}, false, err
	}
	defer tx.Rollback()
	var revision, phase string
	var repairCount int
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase,repair_count from dorf.jobs where id=$1 for update`, check.JobID).Scan(&revision, &phase, &repairCount); err != nil {
		return spine.Message{}, false, err
	}
	callerID := "dorf:repair:1"
	var existing spine.Message
	err = tx.QueryRowContext(ctx, `select id,job_id,caller_id,sequence,input from dorf.job_messages where job_id=$1 and caller_id=$2`, check.JobID, callerID).Scan(&existing.ID, &existing.JobID, &existing.CallerID, &existing.Sequence, &existing.Input)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return spine.Message{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spine.Message{}, false, err
	}
	if repairCount != 0 || phase != "checking" || revision != check.Revision || check.State != "failed" {
		return spine.Message{}, false, fmt.Errorf("focused repair is not admissible for the current failed Check")
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `select coalesce(max(sequence),0)+1 from dorf.job_messages where job_id=$1`, check.JobID).Scan(&sequence); err != nil {
		return spine.Message{}, false, err
	}
	input := fmt.Sprintf("Focused repair: the deterministic %s Check failed at exact Revision %s with exit %d. Its command was %q and observed Evidence digest is %s. Repair only that failure, keep the bounded change intact, do not create a commit, and return control so Dorf can commit and rerun affected verification programmatically.", check.Name, check.Revision, check.ExitCode, check.Command, check.EvidenceDigest)
	message := spine.Message{ID: spine.MessageID(check.JobID, callerID), JobID: check.JobID, CallerID: callerID, Sequence: sequence, Input: input}
	if _, err := tx.ExecContext(ctx, `insert into dorf.job_messages(id,job_id,caller_id,sequence,input) values($1,$2,$3,$4,$5)`, message.ID, message.JobID, message.CallerID, message.Sequence, message.Input); err != nil {
		return spine.Message{}, false, err
	}
	actionID, runID := spine.TurnActionID(message.ID), spine.AgentRunID(message.ID)
	if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,message_id,kind,state) values($1,$2,$3,$4,'pending')`, actionID, message.JobID, message.ID, spine.ActionTurnStart); err != nil {
		return spine.Message{}, false, err
	}
	if err := expectOne(tx.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,message_id,action_id,session_id,role,state) select $1,$2,$3,$4,native_session_id,'repair','pending' from dorf.sessions where job_id=$2`, runID, message.JobID, message.ID, actionID)); err != nil {
		return spine.Message{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.jobs set repair_count=1,workflow_phase='repairing',workflow_attention=null where id=$1`, check.JobID); err != nil {
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
	var currentRevision, phase string
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase from dorf.jobs where id=$1 for update`, jobID).Scan(&currentRevision, &phase); err != nil {
		return err
	}
	if currentRevision != revision || phase != "checking" {
		return fmt.Errorf("Revision %s cannot become ready from phase %s at Revision %s", revision, phase, currentRevision)
	}
	rows, err := tx.QueryContext(ctx, `select c.evidence_id from dorf.repository_commands r join dorf.checks c on c.job_id=r.job_id and c.name=r.name and c.command=r.command and c.revision=$2 where r.job_id=$1 and r.name in ('check','smoke') and c.state='passed' and c.exit_code=0 order by r.name`, jobID, revision)
	if err != nil {
		return err
	}
	var proving []string
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !id.Valid || id.String == "" {
			rows.Close()
			return fmt.Errorf("Revision %s has a passing Check without Evidence", revision)
		}
		proving = append(proving, id.String)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var declared int
	if err := tx.QueryRowContext(ctx, `select count(*) from dorf.repository_commands where job_id=$1 and name in ('check','smoke')`, jobID).Scan(&declared); err != nil {
		return err
	}
	sort.Strings(proving)
	verified := append([]string(nil), verifiedEvidenceIDs...)
	sort.Strings(verified)
	if declared == 0 || len(proving) != declared || !slices.Equal(proving, verified) {
		return fmt.Errorf("Revision %s is not ready: verified Evidence does not exactly match %d declared Checks", revision, declared)
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready',workflow_attention=null where id=$1 and revision=$2 and workflow_phase='checking'`, jobID, revision)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) BlockWorkflow(ctx context.Context, jobID, reason string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set workflow_phase='blocked',workflow_attention=$2 where id=$1`, jobID, reason))
}

func (s Store) CompleteAction(ctx context.Context, id string, receipt spine.Receipt) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var jobID, scope string
	var kind spine.ActionKind
	if err := tx.QueryRowContext(ctx, `update dorf.actions set state='succeeded',external_id=$2,external_outcome=nullif($3,''),updated_at=clock_timestamp() where id=$1 returning job_id,kind,scope_key`, id, receipt.ExternalID, receipt.Outcome).Scan(&jobID, &kind, &scope); err != nil {
		return err
	}
	switch kind {
	case spine.ActionSandboxCreate:
		_, err = tx.ExecContext(ctx, `insert into dorf.sandboxes(job_id,action_id,incus_name,state) values($1,$2,$3,'created') on conflict(job_id) do update set incus_name=excluded.incus_name,state='created',observed_at=clock_timestamp()`, jobID, id, receipt.ExternalID)
	case spine.ActionRouteCreate:
		_, err = tx.ExecContext(ctx, `insert into dorf.routes(job_id,action_id,route_id,state) values($1,$2,$3,'active') on conflict(job_id) do update set route_id=excluded.route_id,state='active',observed_at=clock_timestamp()`, jobID, id, receipt.ExternalID)
	case spine.ActionSessionStart:
		if scope != "" {
			if strings.TrimSpace(receipt.ExternalID) == "" {
				err = fmt.Errorf("review Session receipt is empty")
				break
			}
			var inherited bool
			if err = tx.QueryRowContext(ctx, `select exists(select 1 from dorf.sessions where native_session_id=$1)`, receipt.ExternalID).Scan(&inherited); err == nil && inherited {
				err = fmt.Errorf("review AgentRun %s cannot inherit the implementation Session", scope)
			}
			if err != nil {
				break
			}
			err = expectOne(tx.ExecContext(ctx, `update dorf.agent_runs set session_id=coalesce(session_id,$2),updated_at=clock_timestamp() where id=$1 and (session_id is null or session_id=$2)`, scope, receipt.ExternalID))
			break
		}
		var result sql.Result
		result, err = tx.ExecContext(ctx, `insert into dorf.sessions(job_id,action_id,native_session_id) values($1,$2,$3) on conflict(job_id) do update set native_session_id=excluded.native_session_id,observed_at=clock_timestamp() where dorf.sessions.native_session_id=excluded.native_session_id`, jobID, id, receipt.ExternalID)
		if err == nil {
			var affected int64
			affected, err = result.RowsAffected()
			if err == nil && affected != 1 {
				err = fmt.Errorf("native Session binding conflicts with the recorded Session for Job %s", jobID)
			}
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `update dorf.agent_runs set session_id=$2,updated_at=clock_timestamp() where job_id=$1 and role in ('implement','repair') and (session_id is null or session_id=$2)`, jobID, receipt.ExternalID)
		}
	case spine.ActionRouteRevoke:
		_, err = tx.ExecContext(ctx, `update dorf.routes set state='revoked',observed_at=clock_timestamp() where job_id=$1`, jobID)
	case spine.ActionSandboxDelete:
		_, err = tx.ExecContext(ctx, `update dorf.sandboxes set state='deleted',observed_at=clock_timestamp() where job_id=$1`, jobID)
	case spine.ActionReviewWorkspaceCreate:
		var path, revision string
		if err = tx.QueryRowContext(ctx, `select path,revision from dorf.review_workspaces where create_action_id=$1`, id).Scan(&path, &revision); err == nil && (receipt.ExternalID != path || receipt.Outcome != revision) {
			err = fmt.Errorf("review workspace receipt conflicts with its exact path or Revision")
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `update dorf.review_workspaces set state='created',created_at=coalesce(created_at,clock_timestamp()) where create_action_id=$1`, id)
		}
	case spine.ActionReviewWorkspaceDelete:
		var path string
		if err = tx.QueryRowContext(ctx, `select path from dorf.review_workspaces where delete_action_id=$1`, id).Scan(&path); err == nil && (receipt.ExternalID != path || receipt.Outcome != "deleted") {
			err = fmt.Errorf("review workspace cleanup receipt conflicts with its exact path")
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `update dorf.review_workspaces set state='deleted',deleted_at=coalesce(deleted_at,clock_timestamp()) where delete_action_id=$1`, id)
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) UncertainAction(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `update dorf.actions set state='uncertain',updated_at=clock_timestamp() where id=$1 and state<>'succeeded'`, id)
	return err
}

func (s Store) NextDelivery(ctx context.Context, jobID, sessionID string) (*spine.Delivery, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var workflowPhase string
	if err := tx.QueryRowContext(ctx, `select workflow_phase from dorf.jobs where id=$1 for update`, jobID).Scan(&workflowPhase); err != nil {
		return nil, err
	}
	var message spine.Message
	err = tx.QueryRowContext(ctx, `
		select m.id,m.job_id,m.caller_id,m.sequence,m.input
		from dorf.job_messages m
		left join dorf.agent_runs ar on ar.message_id=m.id
		where m.job_id=$1 and coalesce(ar.state,'') <> 'completed'
		order by m.sequence limit 1`, jobID).Scan(&message.ID, &message.JobID, &message.CallerID, &message.Sequence, &message.Input)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	actionID := spine.TurnActionID(message.ID)
	runID := spine.AgentRunID(message.ID)
	if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,message_id,kind,state) values($1,$2,$3,$4,'pending') on conflict do nothing`, actionID, jobID, message.ID, spine.ActionTurnStart); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,message_id,action_id,session_id,role,state) values($1,$2,$3,$4,nullif($5,''),'implement','pending') on conflict do nothing`, runID, jobID, message.ID, actionID, sessionID); err != nil {
		return nil, err
	}
	if sessionID != "" {
		if _, err := tx.ExecContext(ctx, `update dorf.agent_runs set session_id=coalesce(session_id,$2),updated_at=clock_timestamp() where message_id=$1 and (session_id is null or session_id=$2)`, message.ID, sessionID); err != nil {
			return nil, err
		}
	}
	run, err := scanAgentRun(tx.QueryRowContext(ctx, `select id,job_id,message_id,action_id,coalesce(session_id,''),state,baseline_native_turn_id is not null,coalesce(baseline_native_turn_id,''),coalesce(native_turn_id,''),coalesce(native_outcome,''),coalesce(attention,''),role from dorf.agent_runs where message_id=$1`, message.ID))
	if err != nil {
		return nil, err
	}
	if sessionID != "" && run.SessionID != sessionID {
		return nil, fmt.Errorf("AgentRun %s is bound to native Session %s, not %s", run.ID, run.SessionID, sessionID)
	}
	allowed := run.Role == "implement" && (workflowPhase == "setup" || workflowPhase == "implementing") || run.Role == "repair" && (workflowPhase == "repairing" || workflowPhase == "review-repairing")
	if !allowed {
		reason := fmt.Sprintf("preserved FIFO sequence %d as attention: %s AgentRun cannot start during workflow phase %s", message.Sequence, run.Role, workflowPhase)
		if _, err := tx.ExecContext(ctx, `update dorf.agent_runs set state='uncertain',attention=$2,updated_at=clock_timestamp() where id=$1 and state not in ('completed','failed','interrupted')`, run.ID, reason); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='blocked',workflow_attention=$2 where id=$1`, jobID, reason); err != nil {
			return nil, err
		}
		run.State, run.Attention = spine.AgentRunUncertain, reason
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &spine.Delivery{Message: message, AgentRun: run}, nil
}

func scanAgentRun(row *sql.Row) (spine.AgentRun, error) {
	var run spine.AgentRun
	err := row.Scan(&run.ID, &run.JobID, &run.MessageID, &run.ActionID, &run.SessionID, &run.State,
		&run.BaselineRecorded, &run.BaselineTurnID, &run.NativeTurnID, &run.NativeOutcome, &run.Attention, &run.Role)
	return run, err
}

func (s Store) PrepareAgentRun(ctx context.Context, runID, baselineTurnID string) error {
	result, err := s.DB.ExecContext(ctx, `update dorf.agent_runs set state='submitting',baseline_native_turn_id=$2,attention=null,started_at=coalesce(started_at,clock_timestamp()),updated_at=clock_timestamp() where id=$1 and state='pending'`, runID, baselineTurnID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	var recorded bool
	var baseline string
	if err := s.DB.QueryRowContext(ctx, `select baseline_native_turn_id is not null,coalesce(baseline_native_turn_id,'') from dorf.agent_runs where id=$1`, runID).Scan(&recorded, &baseline); err != nil {
		return err
	}
	if !recorded || baseline != baselineTurnID {
		return fmt.Errorf("AgentRun %s native baseline conflicts with durable baseline", runID)
	}
	return nil
}

func (s Store) BeginTurnSubmission(ctx context.Context, runID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.actions set state='pending',attempts=attempts+1,updated_at=clock_timestamp() where id=(select action_id from dorf.agent_runs where id=$1) and state in ('pending','uncertain')`, runID))
}

func (s Store) BindNativeTurn(ctx context.Context, runID, turnID, status string) error {
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
		attention = fmt.Sprintf("native turn %s has unsupported status %q", turnID, status)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actionID string
	if err := tx.QueryRowContext(ctx, `update dorf.agent_runs set native_turn_id=coalesce(native_turn_id,$2),state=$3,native_outcome=nullif($4,''),attention=nullif($5,''),finished_at=case when $3 in ('completed','failed','interrupted') then coalesce(finished_at,clock_timestamp()) else finished_at end,updated_at=clock_timestamp() where id=$1 and (native_turn_id is null or native_turn_id=$2) returning action_id`, runID, turnID, state, outcome, attention).Scan(&actionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.actions set state='succeeded',external_id=$2,external_outcome='submitted',updated_at=clock_timestamp() where id=$1`, actionID, turnID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) FailAgentRun(ctx context.Context, runID, reason string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actionID string
	if err := tx.QueryRowContext(ctx, `update dorf.agent_runs set state='failed',native_outcome=case when native_turn_id is null then null else 'failed' end,attention=$2,updated_at=clock_timestamp() where id=$1 returning action_id`, runID, reason).Scan(&actionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.actions set state='failed',external_outcome=$2,updated_at=clock_timestamp() where id=$1`, actionID, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) UncertainAgentRun(ctx context.Context, runID, reason string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actionID string
	if err := tx.QueryRowContext(ctx, `update dorf.agent_runs set state='uncertain',attention=$2,updated_at=clock_timestamp() where id=$1 returning action_id`, runID, reason).Scan(&actionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.actions set state='uncertain',external_outcome=$2,updated_at=clock_timestamp() where id=$1 and state<>'succeeded'`, actionID, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) AgentRunAttention(ctx context.Context, runID, reason string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.agent_runs set attention=$2,updated_at=clock_timestamp() where id=$1`, runID, reason))
}

func (s Store) Messages(ctx context.Context, jobID string) ([]spine.MessageView, error) {
	rows, err := s.DB.QueryContext(ctx, `
		select m.id,m.job_id,m.caller_id,m.sequence,m.input,
		       coalesce(ar.id,''),coalesce(ar.state,''),coalesce(ar.native_turn_id,''),
		       coalesce(ar.native_outcome,''),coalesce(ar.attention,'')
		from dorf.job_messages m left join dorf.agent_runs ar on ar.message_id=m.id
		where m.job_id=$1 order by m.sequence`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []spine.MessageView
	for rows.Next() {
		var view spine.MessageView
		if err := rows.Scan(&view.ID, &view.JobID, &view.CallerID, &view.Sequence, &view.Input,
			&view.AgentRunID, &view.State, &view.NativeTurnID, &view.NativeOutcome, &view.Attention); err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var blocker *spine.MessageView
	for i := range views {
		view := &views[i]
		if blocker != nil {
			view.BlockingSeq = blocker.Sequence
			view.BlockingReason = string(blocker.State)
			if blocker.Attention != "" {
				view.BlockingReason += ": " + blocker.Attention
			}
		}
		if blocker == nil && (view.State == spine.AgentRunFailed || view.State == spine.AgentRunInterrupted || view.State == spine.AgentRunUncertain) {
			blocker = view
		}
	}
	return views, nil
}

func (s Store) NativeMutationDelivery(ctx context.Context, jobID string) (*spine.Delivery, error) {
	var delivery spine.Delivery
	err := s.DB.QueryRowContext(ctx, `
		select m.id,m.job_id,m.caller_id,m.sequence,m.input,
		       ar.id,ar.job_id,ar.message_id,ar.action_id,coalesce(ar.session_id,''),ar.state,
		       ar.baseline_native_turn_id is not null,coalesce(ar.baseline_native_turn_id,''),
		       coalesce(ar.native_turn_id,''),coalesce(ar.native_outcome,''),coalesce(ar.attention,''),ar.role
		from dorf.job_messages m join dorf.agent_runs ar on ar.message_id=m.id
		where m.job_id=$1 and ar.state in ('submitting','active','uncertain')
		order by m.sequence limit 1`, jobID).Scan(
		&delivery.Message.ID, &delivery.Message.JobID, &delivery.Message.CallerID, &delivery.Message.Sequence, &delivery.Message.Input,
		&delivery.AgentRun.ID, &delivery.AgentRun.JobID, &delivery.AgentRun.MessageID, &delivery.AgentRun.ActionID,
		&delivery.AgentRun.SessionID, &delivery.AgentRun.State, &delivery.AgentRun.BaselineRecorded,
		&delivery.AgentRun.BaselineTurnID, &delivery.AgentRun.NativeTurnID, &delivery.AgentRun.NativeOutcome,
		&delivery.AgentRun.Attention, &delivery.AgentRun.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &delivery, nil
}

func (s Store) CompleteCleanup(ctx context.Context, jobID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var unsettled int
	if err := tx.QueryRowContext(ctx, `select count(*) from dorf.review_workspaces where job_id=$1 and state<>'deleted'`, jobID).Scan(&unsettled); err != nil {
		return err
	}
	if unsettled != 0 {
		return fmt.Errorf("cleanup cannot complete with %d retained review workspaces", unsettled)
	}
	var sandboxState, routeState string
	if err := tx.QueryRowContext(ctx, `select coalesce((select state from dorf.sandboxes where job_id=$1),''),coalesce((select state from dorf.routes where job_id=$1),'')`, jobID).Scan(&sandboxState, &routeState); err != nil {
		return err
	}
	if sandboxState != "deleted" || routeState != "revoked" {
		return fmt.Errorf("cleanup cannot complete before exact Sandbox deletion and route revocation are observed")
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set cleanup_state='complete',cleaned_at=clock_timestamp() where id=$1`, jobID)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) CancelRun(ctx context.Context, jobID string) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var taskID string
	if err := tx.QueryRowContext(ctx, `select coalesce(task_id,'') from dorf.jobs where id=$1 for update`, jobID).Scan(&taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if taskID == "" {
		return "", fmt.Errorf("Job %s has no Absurd run task to cancel", jobID)
	}
	if _, err := tx.ExecContext(ctx, `select absurd.cancel_task('dorf_jobs',$1::uuid)`, taskID); err != nil {
		return "", fmt.Errorf("cancel Absurd run task: %w", err)
	}
	var state string
	if err := tx.QueryRowContext(ctx, `select state from absurd.t_dorf_jobs where task_id=$1::uuid`, taskID).Scan(&state); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.jobs set run_terminal_state=case when $2 in ('failed','cancelled') then coalesce(run_terminal_state,$2) else run_terminal_state end,run_terminal_at=case when $2 in ('failed','cancelled') then coalesce(run_terminal_at,clock_timestamp()) else run_terminal_at end where id=$1`, jobID, state); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return state, nil
}

func (s Store) Actions(ctx context.Context, jobID string) ([]ActionView, error) {
	rows, err := s.DB.QueryContext(ctx, `select a.id,coalesce(a.message_id,''),a.kind,a.state,coalesce(a.external_id,''),a.attempts,a.scope_key,coalesce(e.digest,'') from dorf.actions a left join dorf.evidence e on e.action_id=a.id where a.job_id=$1 order by a.created_at,a.id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var actions []ActionView
	for rows.Next() {
		var action ActionView
		if err := rows.Scan(&action.ID, &action.MessageID, &action.Kind, &action.State, &action.ExternalID, &action.Attempts, &action.Scope, &action.EvidenceDigest); err != nil {
			return nil, err
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

func (s Store) Checks(ctx context.Context, jobID string) ([]spine.Check, error) {
	rows, err := s.DB.QueryContext(ctx, `select c.id,c.job_id,c.name,c.command,c.revision,c.state,coalesce(c.exit_code,0),coalesce(c.evidence_id,''),coalesce(e.digest,''),coalesce(c.started_at,'epoch'),coalesce(c.finished_at,'epoch') from dorf.checks c left join dorf.evidence e on e.id=c.evidence_id where c.job_id=$1 order by c.started_at nulls last,c.id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checks []spine.Check
	for rows.Next() {
		var check spine.Check
		if err := rows.Scan(&check.ID, &check.JobID, &check.Name, &check.Command, &check.Revision, &check.State, &check.ExitCode, &check.EvidenceID, &check.EvidenceDigest, &check.StartedAt, &check.FinishedAt); err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	return checks, rows.Err()
}

func (s Store) Evidence(ctx context.Context, jobID string) ([]spine.Evidence, error) {
	rows, err := s.DB.QueryContext(ctx, `select id,digest,byte_size,media_type,producer,provenance,kind,coalesce(action_id,''),coalesce(check_id,''),coalesce(revision,''),coalesce(started_at,'epoch'),coalesce(finished_at,'epoch') from dorf.evidence where job_id=$1 order by created_at,id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []spine.Evidence
	for rows.Next() {
		var record spine.Evidence
		if err := rows.Scan(&record.ID, &record.Digest, &record.ByteSize, &record.MediaType, &record.Producer, &record.Provenance, &record.Kind, &record.ActionID, &record.CheckID, &record.Revision, &record.StartedAt, &record.FinishedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s Store) NextWakeSequence(ctx context.Context, jobID string) (int64, error) {
	var sequence int64
	err := s.DB.QueryRowContext(ctx, `
		select coalesce(
			(
				select m.sequence
				from dorf.jobs j
				join dorf.actions a on a.id=j.setup_action_id
				join dorf.job_messages m on m.job_id=j.id and m.caller_id='dorf:setup-retry:'||a.scope_key
				where j.id=$1 and j.workflow_phase='setup' and a.kind='repository-setup'
				  and a.scope_key<>'' and a.state in ('pending','uncertain')
			),
			(
				select min(m.sequence)
				from dorf.job_messages m
				join dorf.agent_runs ar on ar.message_id=m.id
				where m.job_id=$1 and m.sequence>1 and ar.state='pending'
				  and not exists (
					select 1
					from dorf.job_messages earlier
					join dorf.agent_runs earlier_run on earlier_run.message_id=earlier.id
					where earlier.job_id=m.job_id and earlier.sequence<m.sequence
					  and earlier_run.state<>'completed'
				  )
			),
			(select coalesce(max(sequence),0)+1 from dorf.job_messages where job_id=$1)
		)`, jobID).Scan(&sequence)
	return sequence, err
}

func (s Store) TaskEvidence(ctx context.Context, taskID string) (TaskEvidence, error) {
	if taskID == "" {
		return TaskEvidence{}, nil
	}
	var evidence TaskEvidence
	err := s.DB.QueryRowContext(ctx, `select t.task_id::text,t.state,t.attempts,count(c.task_id) from absurd.t_dorf_jobs t left join absurd.c_dorf_jobs c on c.task_id=t.task_id where t.task_id=$1::uuid group by t.task_id,t.state,t.attempts`, taskID).Scan(&evidence.TaskID, &evidence.State, &evidence.Attempts, &evidence.Checkpoints)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskEvidence{TaskID: taskID, State: "missing"}, nil
	}
	return evidence, err
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
