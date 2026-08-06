package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/spine"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var ErrNotFound = errors.New("Dorf Job not found")

const (
	// AbsurdReleaseCommit is the commit behind the SDK's v0.5.0 module tag. The
	// schema URL uses the immutable commit rather than trusting a mutable tag.
	AbsurdReleaseCommit = "550d3b9e6f9382d96178de6ab8c90c7f8edf2227"
	AbsurdSchemaURL     = "https://raw.githubusercontent.com/earendil-works/absurd/" + AbsurdReleaseCommit + "/sql/absurd.sql"
	AbsurdSchemaSHA256  = "d34309370c539f3a51f2b36b69b1f77551f8e4a14480a1c8def8bb8f40fd9aab"
)

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

type ActionView struct {
	ID         string            `json:"id"`
	Kind       spine.ActionKind  `json:"kind"`
	State      spine.ActionState `json:"state"`
	ExternalID string            `json:"external_id,omitempty"`
	Attempts   int               `json:"attempts"`
}

type TaskEvidence struct {
	TaskID      string `json:"task_id"`
	State       string `json:"state"`
	Attempts    int    `json:"attempts"`
	Checkpoints int    `json:"checkpoints"`
}

func (s Store) Migrate(ctx context.Context) error {
	var version string
	if err := s.DB.QueryRowContext(ctx, `select absurd.get_schema_version()`).Scan(&version); err != nil {
		return fmt.Errorf("Absurd schema is not ready: %w (initialize pinned Absurd 0.5.0 first)", err)
	}
	if version != "0.5.0" {
		return fmt.Errorf("Absurd schema version is %q; Dorf requires 0.5.0", version)
	}
	contents, err := migrationFiles.ReadFile("migrations/001_dorf.sql")
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
		return fmt.Errorf("apply Dorf migration 001_dorf.sql: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.schema_migrations(name) values ('001_dorf.sql') on conflict do nothing`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `select absurd.create_queue('dorf_jobs')`); err != nil {
		return fmt.Errorf("create Absurd queue dorf_jobs: %w", err)
	}
	return tx.Commit()
}

func (s Store) Admit(ctx context.Context, input NewJob) (spine.Job, bool, error) {
	input.AdmissionKey = strings.TrimSpace(input.AdmissionKey)
	input.Goal = strings.TrimSpace(input.Goal)
	input.Repository = strings.TrimSpace(input.Repository)
	input.Revision = strings.TrimSpace(input.Revision)
	input.Branch = strings.TrimSpace(input.Branch)
	input.ProviderConnection = strings.TrimSpace(input.ProviderConnection)
	input.Model = strings.TrimSpace(input.Model)
	input.ReasoningEffort = strings.TrimSpace(input.ReasoningEffort)
	if input.AdmissionKey == "" || input.Goal == "" || input.Repository == "" || input.Revision == "" || input.Branch == "" || input.ProviderConnection == "" || input.Model == "" || input.ReasoningEffort == "" {
		return spine.Job{}, false, fmt.Errorf("admission requires key, complete goal, repository, revision, branch, provider connection, model, and reasoning effort")
	}
	id := spine.JobID(input.AdmissionKey)
	result, err := s.DB.ExecContext(ctx, `
		insert into dorf.jobs (
			id, admission_key, goal, repository, revision, branch,
			provider_connection, model, reasoning_effort
		) values ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		on conflict (admission_key) do nothing`,
		id, input.AdmissionKey, input.Goal, input.Repository, input.Revision, input.Branch,
		input.ProviderConnection, input.Model, input.ReasoningEffort,
	)
	if err != nil {
		return spine.Job{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return spine.Job{}, false, err
	}
	job, err := s.Job(ctx, id)
	if err != nil {
		return spine.Job{}, false, err
	}
	if job.Goal != input.Goal || job.Repository != input.Repository || job.Revision != input.Revision || job.Branch != input.Branch || job.ProviderConnection != input.ProviderConnection || job.Model != input.Model || job.ReasoningEffort != input.ReasoningEffort {
		return spine.Job{}, false, fmt.Errorf("admission key %q is already bound to different complete Job input", input.AdmissionKey)
	}
	return job, rows == 1, nil
}

func (s Store) Job(ctx context.Context, id string) (spine.Job, error) {
	var job spine.Job
	err := s.DB.QueryRowContext(ctx, `
		select j.id, j.admission_key, j.goal, j.repository, j.revision, j.branch,
		       j.provider_connection, j.model, j.reasoning_effort, j.state,
		       j.cleanup_state, coalesce(j.task_id,''), coalesce(j.cleanup_task_id,''),
		       coalesce(sb.incus_name,''), coalesce(r.route_id,''),
		       coalesce(se.native_session_id,''), coalesce(ar.id,''),
		       coalesce(ar.native_turn_id,''), coalesce(j.native_outcome,'')
		from dorf.jobs j
		left join dorf.sandboxes sb on sb.job_id=j.id
		left join dorf.routes r on r.job_id=j.id
		left join dorf.sessions se on se.job_id=j.id
		left join dorf.agent_runs ar on ar.job_id=j.id
		where j.id=$1`, id).Scan(
		&job.ID, &job.AdmissionKey, &job.Goal, &job.Repository, &job.Revision, &job.Branch,
		&job.ProviderConnection, &job.Model, &job.ReasoningEffort, &job.State,
		&job.CleanupState, &job.TaskID, &job.CleanupTaskID, &job.SandboxID, &job.RouteID,
		&job.SessionID, &job.AgentRunID, &job.NativeTurnID, &job.NativeOutcome,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return spine.Job{}, ErrNotFound
	}
	return job, err
}

func (s Store) SetTaskID(ctx context.Context, jobID, taskID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set task_id=coalesce(task_id,$2) where id=$1 and (task_id is null or task_id=$2)`, jobID, taskID))
}

func (s Store) StartRun(ctx context.Context, jobID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set state=case when state='observed' then state else 'running' end where id=$1`, jobID))
}

func (s Store) SetCleanupTaskID(ctx context.Context, jobID, taskID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set cleanup_task_id=coalesce(cleanup_task_id,$2), cleanup_state=case when cleanup_state='complete' or cleaned_at is not null then 'complete' else 'scheduled' end where id=$1 and (cleanup_task_id is null or cleanup_task_id=$2)`, jobID, taskID))
}

func (s Store) BeginAction(ctx context.Context, jobID string, kind spine.ActionKind) (spine.Action, error) {
	id := spine.ActionID(jobID, kind)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Action{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state) values($1,$2,$3,'pending') on conflict(job_id,kind) do nothing`, id, jobID, kind); err != nil {
		return spine.Action{}, err
	}
	var action spine.Action
	if err := tx.QueryRowContext(ctx, `update dorf.actions set attempts=attempts+case when state='succeeded' then 0 else 1 end, updated_at=clock_timestamp() where id=$1 returning id,job_id,kind,state,coalesce(external_id,''),coalesce(external_outcome,'')`, id).Scan(&action.ID, &action.JobID, &action.Kind, &action.State, &action.ExternalID, &action.Outcome); err != nil {
		return spine.Action{}, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Action{}, err
	}
	return action, nil
}

func (s Store) CompleteAction(ctx context.Context, id string, receipt spine.Receipt) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var jobID string
	var kind spine.ActionKind
	if err := tx.QueryRowContext(ctx, `update dorf.actions set state='succeeded',external_id=$2,external_outcome=nullif($3,''),updated_at=clock_timestamp() where id=$1 returning job_id,kind`, id, receipt.ExternalID, receipt.Outcome).Scan(&jobID, &kind); err != nil {
		return err
	}
	switch kind {
	case spine.ActionSandboxCreate:
		_, err = tx.ExecContext(ctx, `insert into dorf.sandboxes(job_id,action_id,incus_name,state) values($1,$2,$3,'created') on conflict(job_id) do update set incus_name=excluded.incus_name,state='created',observed_at=clock_timestamp()`, jobID, id, receipt.ExternalID)
	case spine.ActionRouteCreate:
		_, err = tx.ExecContext(ctx, `insert into dorf.routes(job_id,action_id,route_id,state) values($1,$2,$3,'active') on conflict(job_id) do update set route_id=excluded.route_id,state='active',observed_at=clock_timestamp()`, jobID, id, receipt.ExternalID)
	case spine.ActionSessionStart:
		_, err = tx.ExecContext(ctx, `insert into dorf.sessions(job_id,action_id,native_session_id) values($1,$2,$3) on conflict(job_id) do update set native_session_id=excluded.native_session_id,observed_at=clock_timestamp()`, jobID, id, receipt.ExternalID)
	case spine.ActionRouteRevoke:
		_, err = tx.ExecContext(ctx, `update dorf.routes set state='revoked',observed_at=clock_timestamp() where job_id=$1`, jobID)
	case spine.ActionSandboxDelete:
		_, err = tx.ExecContext(ctx, `update dorf.sandboxes set state='deleted',observed_at=clock_timestamp() where job_id=$1`, jobID)
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

func (s Store) ObserveRun(ctx context.Context, jobID string, observed spine.Observation) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actionID := spine.ActionID(jobID, spine.ActionTurnStart)
	if _, err := tx.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,action_id,session_id,native_turn_id,role,native_outcome) values($1,$2,$3,$4,$5,'implement',$6) on conflict(job_id) do update set native_turn_id=excluded.native_turn_id,native_outcome=excluded.native_outcome,observed_at=clock_timestamp()`, observed.AgentRunID, jobID, actionID, observed.SessionID, observed.TurnID, observed.Outcome); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.jobs set state='observed',native_outcome=$2,observed_at=clock_timestamp() where id=$1`, jobID, observed.Outcome); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) CompleteCleanup(ctx context.Context, jobID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set cleanup_state='complete',cleaned_at=clock_timestamp() where id=$1`, jobID))
}

func (s Store) Actions(ctx context.Context, jobID string) ([]ActionView, error) {
	rows, err := s.DB.QueryContext(ctx, `select id,kind,state,coalesce(external_id,''),attempts from dorf.actions where job_id=$1 order by created_at,id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var actions []ActionView
	for rows.Next() {
		var action ActionView
		if err := rows.Scan(&action.ID, &action.Kind, &action.State, &action.ExternalID, &action.Attempts); err != nil {
			return nil, err
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
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
