package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// HoldSandboxDelivery serializes against native effects and Message admission.
// The operation ID is immutable: replaying a released hold never reopens it.
// Only direct Sessions are supported until workflow mutations also honor this gate.
func (s Store) HoldSandboxDelivery(ctx context.Context, queue, sessionID, sandboxID, operationID string) (core.SandboxDeliveryHold, error) {
	var hold core.SandboxDeliveryHold
	err := s.withDeliveryHold(ctx, sessionID, sandboxID, operationID, func(tx *sql.Tx, q *dbsql.Queries, session dbsql.GetSessionAdmissionForUpdateRow) error {
		row, err := q.GetSandboxDeliveryHold(ctx, operationID)
		if err == nil {
			if row.SandboxID != sandboxID {
				return fmt.Errorf("delivery hold belongs to a different Sandbox")
			}
			hold = deliveryHold(row)
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !session.AdmissionOpen || session.CleanupState != core.CleanupPending {
			return fmt.Errorf("delivery hold requires an open direct Session")
		}
		if err := q.InsertSandboxDeliveryHold(ctx, dbsql.InsertSandboxDeliveryHoldParams{ID: operationID, SandboxID: sandboxID}); err != nil {
			return err
		}
		row, err = q.GetSandboxDeliveryHold(ctx, operationID)
		if err != nil {
			return err
		}
		hold = deliveryHold(row)
		_, err = signalSessionExecutionWakeTx(ctx, tx, queue, sessionID, "hold:"+operationID)
		return err
	})
	return hold, err
}

// ReleaseSandboxDelivery requires the exact retained operation ID, so a stale
// release cannot clear a newer hold. The coordinating operation owns the proof
// that it is safe to resume; this method never infers success from empty output.
func (s Store) ReleaseSandboxDelivery(ctx context.Context, queue, sessionID, sandboxID, operationID string) error {
	return s.withDeliveryHold(ctx, sessionID, sandboxID, operationID, func(tx *sql.Tx, q *dbsql.Queries, _ dbsql.GetSessionAdmissionForUpdateRow) error {
		if _, err := q.GetSandboxUpgrade(ctx, operationID); err == nil {
			return fmt.Errorf("package upgrade holds require verified atomic completion")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := q.GetCheckpointRecovery(ctx, operationID); err == nil {
			return fmt.Errorf("checkpoint recovery holds require verified atomic completion")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := expectOneRows(q.ReleaseSandboxDeliveryHold(ctx, dbsql.ReleaseSandboxDeliveryHoldParams{ID: operationID, SandboxID: sandboxID})); err != nil {
			return err
		}
		_, err := signalSessionExecutionWakeTx(ctx, tx, queue, sessionID, "release-hold:"+operationID)
		return err
	})
}

func (s Store) withDeliveryHold(ctx context.Context, sessionID, sandboxID, operationID string, fn func(*sql.Tx, *dbsql.Queries, dbsql.GetSessionAdmissionForUpdateRow) error) error {
	for _, id := range []string{sessionID, sandboxID, operationID} {
		if id == "" || id != strings.TrimSpace(id) || len(id) > 256 {
			return fmt.Errorf("delivery hold requires exact bounded identities")
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := acquireSessionFenceTx(ctx, tx, sessionID); err != nil {
		return err
	}
	q := dbsql.New(tx)
	session, err := q.GetSessionAdmissionForUpdate(ctx, sessionID)
	if err != nil {
		return err
	}
	owned, err := q.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if owned.SessionID != sessionID {
		return fmt.Errorf("delivery hold belongs to a different Session")
	}
	if err := fn(tx, q, session); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) SandboxDeliveryHeld(ctx context.Context, sandboxID string) (bool, error) {
	return dbsql.New(s.DB).SandboxDeliveryHeld(ctx, sandboxID)
}

func (s Store) SessionDeliveryHolds(ctx context.Context, sessionID string) ([]core.SandboxDeliveryHold, error) {
	rows, err := dbsql.New(s.DB).ListSessionDeliveryHolds(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	holds := make([]core.SandboxDeliveryHold, 0, len(rows))
	for _, row := range rows {
		holds = append(holds, deliveryHold(row))
	}
	return holds, nil
}

func deliveryHold(row dbsql.DorfSandboxDeliveryHold) core.SandboxDeliveryHold {
	return core.SandboxDeliveryHold{ID: row.ID, SandboxID: row.SandboxID, Reason: row.Reason, RequestedAt: row.RequestedAt, ReleasedAt: timeValue(row.ReleasedAt)}
}

// Typed maintenance releases its hold only at atomic completion or after Session
// admission closes for cleanup. The hold is the existing exclusion authority.
func requireSandboxDeliveryUnheld(ctx context.Context, q *dbsql.Queries, sandboxID string) error {
	held, err := q.SandboxDeliveryHeld(ctx, sandboxID)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("Sandbox already has an unfinished maintenance operation")
	}
	return nil
}
