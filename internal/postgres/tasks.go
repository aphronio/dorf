package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// spawnJobTaskTx uses Absurd's public SQL API because its Go client owns a
// separate connection. Task eligibility and Dorf attachment commit together.
func spawnJobTaskTx(ctx context.Context, tx *sql.Tx, queue, jobID, previousTaskID, name, key string) (string, error) {
	params, err := json.Marshal(core.JobTaskParams{JobID: jobID, PreviousTaskID: previousTaskID})
	if err != nil {
		return "", err
	}
	policy := absurdruntime.TaskSpawnOptions(queue, key)
	options, err := json.Marshal(map[string]any{
		"max_attempts": policy.MaxAttempts, "idempotency_key": key,
		"retry_strategy": map[string]any{
			"kind": policy.RetryStrategy.Kind, "base_seconds": policy.RetryStrategy.BaseSeconds,
			"factor": policy.RetryStrategy.Factor, "max_seconds": policy.RetryStrategy.MaxSeconds,
		},
	})
	if err != nil {
		return "", err
	}
	var taskID string
	err = tx.QueryRowContext(ctx, `select task_id::text from absurd.spawn_task($1,$2,$3::jsonb,$4::jsonb)`, queue, name, string(params), string(options)).Scan(&taskID)
	if err != nil {
		return "", fmt.Errorf("schedule Job task in Absurd: %w", err)
	}
	return taskID, nil
}

func scheduleJobTaskTx(ctx context.Context, tx *sql.Tx, queue, jobID, name, key string, admission bool) error {
	queries := dbsql.New(tx)
	current, err := queries.GetCurrentJobTaskForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	// Admission replay must preserve an existing handoff or closed Job.
	if admission && (current.TaskID != "" || !current.AdmissionOpen) {
		return nil
	}
	if !current.AdmissionOpen || current.CleanupState != core.CleanupPending {
		return fmt.Errorf("Job %s cannot schedule ordinary work after cleanup begins", jobID)
	}
	taskID, err := spawnJobTaskTx(ctx, tx, queue, jobID, current.TaskID, name, key)
	if err != nil {
		return err
	}
	return attachJobTaskTx(ctx, queries, jobID, current.TaskID, taskID, name, false)
}

func (s Store) ScheduleJobTask(ctx context.Context, queue, jobID, name, key string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := acquireJobFenceTx(ctx, tx, jobID); err != nil {
		return err
	}
	if err := scheduleJobTaskTx(ctx, tx, queue, jobID, name, key, false); err != nil {
		return err
	}
	return tx.Commit()
}

// ScheduleCleanup closes admission, cancels the previous task, and attaches
// cleanup under the same transaction and external-effect fence.
func (s Store) ScheduleCleanup(ctx context.Context, queue, jobID, callerTaskID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := acquireJobFenceTx(ctx, tx, jobID); err != nil {
		return err
	}
	queries := dbsql.New(tx)
	current, err := queries.GetCurrentJobTaskForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if current.CleanupState == core.CleanupScheduled || current.CleanupState == core.CleanupComplete {
		return nil
	}
	if err := expectOneRows(queries.RequestCleanup(ctx, jobID)); err != nil {
		return err
	}
	if current.TaskID != "" && current.TaskID != callerTaskID {
		if _, err := tx.ExecContext(ctx, `select absurd.cancel_task($1,$2::uuid)`, queue, current.TaskID); err != nil {
			return fmt.Errorf("cancel attached Absurd task: %w", err)
		}
	}
	taskID, err := spawnJobTaskTx(ctx, tx, queue, jobID, current.TaskID, core.CleanupTaskName, "cleanup:v3:"+jobID)
	if err != nil {
		return err
	}
	if err := attachJobTaskTx(ctx, queries, jobID, current.TaskID, taskID, core.CleanupTaskName, true); err != nil {
		return err
	}
	return tx.Commit()
}
