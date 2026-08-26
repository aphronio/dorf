package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/jackc/pgx/v5/pgconn"
)

// RetryFailedJob atomically binds one caller request to the exact retry
// attempt scheduled by Absurd. Replaying the request returns the committed
// receipt even if the Job has since advanced.
func (s Store) RetryFailedJob(ctx context.Context, queueName, jobID, requestKey string) (core.RetryReceipt, error) {
	queueName, jobID, requestKey = strings.TrimSpace(queueName), strings.TrimSpace(jobID), strings.TrimSpace(requestKey)
	if queueName == "" || len(queueName) > 57 || jobID == "" || requestKey == "" || len(requestKey) > 255 {
		return core.RetryReceipt{}, fmt.Errorf("retry requires a valid queue, Job ID, and caller-retained request key")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.RetryReceipt{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	if err := queries.LockJobRetryRequest(ctx, requestKey); err != nil {
		return core.RetryReceipt{}, err
	}
	stored, err := queries.GetJobRetryRequest(ctx, requestKey)
	if err == nil {
		if stored.JobID != jobID {
			return core.RetryReceipt{}, fmt.Errorf("%w: %q", core.ErrRetryReplayConflict, requestKey)
		}
		return retryReceipt(stored.RequestKey, stored.JobID, stored.TaskID, stored.RunID, int(stored.Attempt), false), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.RetryReceipt{}, err
	}
	target, err := queries.GetCurrentJobTaskForUpdate(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.RetryReceipt{}, ErrNotFound
	}
	if err != nil {
		return core.RetryReceipt{}, err
	}
	if target.TaskID == "" {
		return core.RetryReceipt{}, fmt.Errorf("%w: Job %s has no attached execution task", core.ErrRetryNotEligible, jobID)
	}
	var taskID, runID string
	var attempt int
	var taskCreated bool
	err = tx.QueryRowContext(ctx, `select task_id::text,run_id::text,attempt,created from absurd.retry_task($1,$2::uuid,'{}'::jsonb)`, queueName, target.TaskID).
		Scan(&taskID, &runID, &attempt, &taskCreated)
	if err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "P0001" {
			return core.RetryReceipt{}, fmt.Errorf("%w: Job %s attached task %s", core.ErrRetryNotEligible, jobID, target.TaskID)
		}
		return core.RetryReceipt{}, fmt.Errorf("retry Job %s attached task %s: %w", jobID, target.TaskID, err)
	}
	if taskID != target.TaskID || runID == "" || attempt <= 0 || taskCreated {
		return core.RetryReceipt{}, fmt.Errorf("Absurd retry returned a conflicting receipt for Job %s", jobID)
	}
	if err := queries.InsertJobRetryRequest(ctx, dbsql.InsertJobRetryRequestParams{
		RequestKey: requestKey, JobID: jobID, TaskID: taskID, RunID: runID, Attempt: int32(attempt),
	}); err != nil {
		return core.RetryReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.RetryReceipt{}, err
	}
	return retryReceipt(requestKey, jobID, taskID, runID, attempt, true), nil
}

func retryReceipt(requestKey, jobID, taskID, runID string, attempt int, created bool) core.RetryReceipt {
	return core.RetryReceipt{
		RequestKey: requestKey, JobID: jobID, TaskID: taskID,
		Retry: "scheduled", RunID: runID, Attempt: attempt, Created: created,
	}
}
