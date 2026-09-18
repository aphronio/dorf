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

func (s Store) SessionExecutionWakeRevision(ctx context.Context, sessionID string) (int64, error) {
	revision, err := dbsql.New(s.DB).GetSessionExecutionWakeRevision(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return revision, err
}

// SignalSessionExecutionWake serializes one idempotent cause under the Session's
// dedicated revision row and emits its immutable Absurd event atomically.
func (s Store) SignalSessionExecutionWake(ctx context.Context, queue, sessionID, causeKey string) (int64, error) {
	if err := validSessionExecutionWakeInput(sessionID, causeKey); err != nil {
		return 0, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	revision, err := signalSessionExecutionWakeTx(ctx, tx, queue, sessionID, causeKey)
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
	binding, err := q.GetNativeTerminalWakeBinding(ctx, dbsql.GetNativeTerminalWakeBindingParams{SessionID: target.SessionID, SandboxID: target.SandboxID})
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("native terminal wake Thread %s was not found", target.ThreadID)
	}
	if err != nil {
		return false, err
	}
	if binding.SessionID != target.SessionID || binding.SandboxID != target.SandboxID {
		return false, fmt.Errorf("native terminal wake has a foreign Session or Sandbox binding")
	}
	if binding.ThreadID != "" && binding.ThreadID != target.ThreadID {
		return false, fmt.Errorf("native terminal wake conflicts with the durable Thread or Turn binding")
	}
	if !binding.AdmissionOpen || core.CleanupState(binding.CleanupState) != core.CleanupPending {
		return false, nil
	}
	causeKey := "native-terminal:" + target.ThreadID + ":" + target.TurnID
	if _, err := signalSessionExecutionWakeTx(ctx, tx, queue, target.SessionID, causeKey); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func signalSessionExecutionWakeTx(ctx context.Context, tx *sql.Tx, queue, sessionID, causeKey string) (int64, error) {
	q := dbsql.New(tx)
	if err := q.EnsureSessionExecutionWake(ctx, sessionID); err != nil {
		return 0, err
	}
	current, err := q.LockSessionExecutionWake(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	revision, err := q.GetSessionExecutionWakeCause(ctx, dbsql.GetSessionExecutionWakeCauseParams{SessionID: sessionID, CauseKey: causeKey})
	if errors.Is(err, sql.ErrNoRows) {
		if current == math.MaxInt64 {
			return 0, fmt.Errorf("Session %s execution wake revision is exhausted", sessionID)
		}
		revision = current + 1
		if err := expectOneRows(q.SetSessionExecutionWakeRevision(ctx, dbsql.SetSessionExecutionWakeRevisionParams{Revision: revision, SessionID: sessionID})); err != nil {
			return 0, err
		}
		if err := q.InsertSessionExecutionWakeCause(ctx, dbsql.InsertSessionExecutionWakeCauseParams{SessionID: sessionID, CauseKey: causeKey, Revision: revision}); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(core.SessionExecutionWakeV1{SessionID: sessionID, Revision: revision, CauseKey: causeKey})
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `select absurd.emit_event($1,$2,$3)`, queue, core.SessionExecutionWakeEvent(sessionID, revision), string(payload)); err != nil {
		return 0, fmt.Errorf("emit Session %s execution wake revision %d: %w", sessionID, revision, err)
	}
	return revision, nil
}

func validSessionExecutionWakeInput(sessionID, causeKey string) error {
	if sessionID == "" || sessionID != strings.TrimSpace(sessionID) || len(sessionID) > 256 {
		return fmt.Errorf("Session execution wake requires a bounded exact Session ID")
	}
	if causeKey == "" || causeKey != strings.TrimSpace(causeKey) || len(causeKey) > 512 {
		return fmt.Errorf("Session execution wake requires a bounded cause key")
	}
	return nil
}

func validNativeTerminalWakeTarget(target core.NativeTerminalWakeTarget) error {
	for name, value := range map[string]string{
		"Session": target.SessionID, "Sandbox": target.SandboxID,
		"Thread": target.ThreadID, "Turn": target.TurnID,
	} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 256 {
			return fmt.Errorf("native terminal wake requires a bounded exact %s ID", name)
		}
	}
	return nil
}
