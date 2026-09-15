package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) JobExecutionWakeRevision(ctx context.Context, jobID string) (int64, error) {
	revision, err := dbsql.New(s.DB).GetJobExecutionWakeRevision(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return revision, err
}

// SignalJobExecutionWake serializes one idempotent cause under the Job's
// dedicated revision row and emits its immutable Absurd event atomically.
func (s Store) SignalJobExecutionWake(ctx context.Context, queue, jobID, causeKey string) (int64, error) {
	if err := validJobExecutionWakeInput(jobID, causeKey); err != nil {
		return 0, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	revision, err := signalJobExecutionWakeTx(ctx, tx, queue, jobID, causeKey)
	if err != nil {
		return 0, err
	}
	return revision, tx.Commit()
}

// SignalNativeTerminalWake validates the observer's exact durable ownership
// when present. A very fast accepted Turn may signal before Thread/Turn binding
// commits; any later nonempty binding must match exactly.
func (s Store) SignalNativeTerminalWake(ctx context.Context, queue string, target core.NativeTerminalWakeTarget) (bool, error) {
	if err := validNativeTerminalWakeTarget(target); err != nil {
		return false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	q := dbsql.New(tx)
	binding, err := q.GetNativeTerminalWakeBinding(ctx, target.AgentRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("native terminal wake AgentRun %s was not found", target.AgentRunID)
	}
	if err != nil {
		return false, err
	}
	if binding.JobID != target.JobID || binding.SandboxID != target.SandboxID {
		return false, fmt.Errorf("native terminal wake has a foreign Job or Sandbox binding")
	}
	if binding.ThreadID != "" && binding.ThreadID != target.ThreadID || binding.TurnID != "" && binding.TurnID != target.TurnID {
		return false, fmt.Errorf("native terminal wake conflicts with the durable Thread or Turn binding")
	}
	if !binding.AdmissionOpen || core.CleanupState(binding.CleanupState) != core.CleanupPending {
		return false, nil
	}
	causeKey := "native-terminal:" + target.AgentRunID + ":" + target.TurnID
	if _, err := signalJobExecutionWakeTx(ctx, tx, queue, target.JobID, causeKey); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func signalJobExecutionWakeTx(ctx context.Context, tx *sql.Tx, queue, jobID, causeKey string) (int64, error) {
	q := dbsql.New(tx)
	if err := q.EnsureJobExecutionWake(ctx, jobID); err != nil {
		return 0, err
	}
	current, err := q.LockJobExecutionWake(ctx, jobID)
	if err != nil {
		return 0, err
	}
	revision, err := q.GetJobExecutionWakeCause(ctx, dbsql.GetJobExecutionWakeCauseParams{JobID: jobID, CauseKey: causeKey})
	if errors.Is(err, sql.ErrNoRows) {
		if current == math.MaxInt64 {
			return 0, fmt.Errorf("Job %s execution wake revision is exhausted", jobID)
		}
		revision = current + 1
		if err := expectOneRows(q.SetJobExecutionWakeRevision(ctx, dbsql.SetJobExecutionWakeRevisionParams{Revision: revision, JobID: jobID})); err != nil {
			return 0, err
		}
		if err := q.InsertJobExecutionWakeCause(ctx, dbsql.InsertJobExecutionWakeCauseParams{JobID: jobID, CauseKey: causeKey, Revision: revision}); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(core.JobExecutionWakeV1{JobID: jobID, Revision: revision, CauseKey: causeKey})
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `select absurd.emit_event($1,$2,$3)`, queue, core.JobExecutionWakeEvent(jobID, revision), string(payload)); err != nil {
		return 0, fmt.Errorf("emit Job %s execution wake revision %d: %w", jobID, revision, err)
	}
	return revision, nil
}

func validJobExecutionWakeInput(jobID, causeKey string) error {
	if jobID == "" || jobID != strings.TrimSpace(jobID) || len(jobID) > 256 {
		return fmt.Errorf("Job execution wake requires a bounded exact Job ID")
	}
	if causeKey == "" || causeKey != strings.TrimSpace(causeKey) || len(causeKey) > 512 {
		return fmt.Errorf("Job execution wake requires a bounded cause key")
	}
	return nil
}

func validNativeTerminalWakeTarget(target core.NativeTerminalWakeTarget) error {
	for name, value := range map[string]string{
		"Job": target.JobID, "Sandbox": target.SandboxID, "AgentRun": target.AgentRunID,
		"Thread": target.ThreadID, "Turn": target.TurnID,
	} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 256 {
			return fmt.Errorf("native terminal wake requires a bounded exact %s ID", name)
		}
	}
	return nil
}
