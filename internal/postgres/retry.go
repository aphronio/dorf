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

// RetryFailedSession atomically binds one caller request to the exact retry
// attempt scheduled by Absurd. Replaying the request returns the committed
// receipt even if the Session has since advanced.
func (s Store) RetryFailedSession(ctx context.Context, queueName, sessionID, requestKey string) (core.RetryReceipt, error) {
	queueName, sessionID, requestKey = strings.TrimSpace(queueName), strings.TrimSpace(sessionID), strings.TrimSpace(requestKey)
	if queueName == "" || len(queueName) > 57 || sessionID == "" || requestKey == "" || len(requestKey) > 255 {
		return core.RetryReceipt{}, fmt.Errorf("retry requires a valid queue, Session ID, and caller-retained request key")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.RetryReceipt{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	if err := queries.LockSessionRetryRequest(ctx, requestKey); err != nil {
		return core.RetryReceipt{}, err
	}
	stored, err := queries.GetSessionRetryRequest(ctx, requestKey)
	if err == nil {
		if stored.SessionID != sessionID {
			return core.RetryReceipt{}, fmt.Errorf("%w: %q", core.ErrRetryReplayConflict, requestKey)
		}
		return retryReceipt(stored.RequestKey, stored.SessionID, stored.TaskID, stored.RunID, int(stored.Attempt), false), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.RetryReceipt{}, err
	}
	target, err := queries.GetCurrentSessionTaskForUpdate(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.RetryReceipt{}, ErrNotFound
	}
	if err != nil {
		return core.RetryReceipt{}, err
	}
	if target.TaskID == "" {
		return core.RetryReceipt{}, fmt.Errorf("%w: Session %s has no attached execution task", core.ErrRetryNotEligible, sessionID)
	}
	var taskID, runID string
	var attempt int
	var taskCreated bool
	err = tx.QueryRowContext(ctx, `select task_id::text,run_id::text,attempt,created from absurd.retry_task($1,$2::uuid,'{}'::jsonb)`, queueName, target.TaskID).
		Scan(&taskID, &runID, &attempt, &taskCreated)
	if err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "P0001" {
			return core.RetryReceipt{}, fmt.Errorf("%w: Session %s attached task %s", core.ErrRetryNotEligible, sessionID, target.TaskID)
		}
		return core.RetryReceipt{}, fmt.Errorf("retry Session %s attached task %s: %w", sessionID, target.TaskID, err)
	}
	if taskID != target.TaskID || runID == "" || attempt <= 0 || taskCreated {
		return core.RetryReceipt{}, fmt.Errorf("Absurd retry returned a conflicting receipt for Session %s", sessionID)
	}
	if err := queries.InsertSessionRetryRequest(ctx, dbsql.InsertSessionRetryRequestParams{
		RequestKey: requestKey, SessionID: sessionID, TaskID: taskID, RunID: runID, Attempt: int32(attempt),
	}); err != nil {
		return core.RetryReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.RetryReceipt{}, err
	}
	return retryReceipt(requestKey, sessionID, taskID, runID, attempt, true), nil
}

func retryReceipt(requestKey, sessionID, taskID, runID string, attempt int, created bool) core.RetryReceipt {
	return core.RetryReceipt{
		RequestKey: requestKey, SessionID: sessionID, TaskID: taskID,
		Retry: "scheduled", RunID: runID, Attempt: attempt, Created: created,
	}
}
