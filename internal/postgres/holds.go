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
// Only direct Jobs are supported until workflow mutations also honor this gate.
func (s Store) HoldSandboxDelivery(ctx context.Context, queue, jobID, sandboxID, operationID string) (core.SandboxDeliveryHold, error) {
	var hold core.SandboxDeliveryHold
	err := s.withDeliveryHold(ctx, jobID, sandboxID, operationID, func(tx *sql.Tx, q *dbsql.Queries, job dbsql.GetJobAdmissionForUpdateRow) error {
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
		if !job.AdmissionOpen || job.CleanupState != core.CleanupPending || job.WorkflowName != "" || job.WorkflowRevision != "" {
			return fmt.Errorf("delivery hold requires an open direct Job")
		}
		if err := q.InsertSandboxDeliveryHold(ctx, dbsql.InsertSandboxDeliveryHoldParams{ID: operationID, SandboxID: sandboxID}); err != nil {
			return err
		}
		row, err = q.GetSandboxDeliveryHold(ctx, operationID)
		if err != nil {
			return err
		}
		hold = deliveryHold(row)
		_, err = signalJobExecutionWakeTx(ctx, tx, queue, jobID, "hold:"+operationID)
		return err
	})
	return hold, err
}

// ReleaseSandboxDelivery requires the exact retained operation ID, so a stale
// release cannot clear a newer hold. The coordinating operation owns the proof
// that it is safe to resume; this method never infers success from empty output.
func (s Store) ReleaseSandboxDelivery(ctx context.Context, queue, jobID, sandboxID, operationID string) error {
	return s.withDeliveryHold(ctx, jobID, sandboxID, operationID, func(tx *sql.Tx, q *dbsql.Queries, _ dbsql.GetJobAdmissionForUpdateRow) error {
		if err := expectOneRows(q.ReleaseSandboxDeliveryHold(ctx, dbsql.ReleaseSandboxDeliveryHoldParams{ID: operationID, SandboxID: sandboxID})); err != nil {
			return err
		}
		_, err := signalJobExecutionWakeTx(ctx, tx, queue, jobID, "release-hold:"+operationID)
		return err
	})
}

func (s Store) withDeliveryHold(ctx context.Context, jobID, sandboxID, operationID string, fn func(*sql.Tx, *dbsql.Queries, dbsql.GetJobAdmissionForUpdateRow) error) error {
	for _, id := range []string{jobID, sandboxID, operationID} {
		if id == "" || id != strings.TrimSpace(id) || len(id) > 256 {
			return fmt.Errorf("delivery hold requires exact bounded identities")
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := acquireJobFenceTx(ctx, tx, jobID); err != nil {
		return err
	}
	q := dbsql.New(tx)
	job, err := q.GetJobAdmissionForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	owned, err := q.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if owned.JobID != jobID {
		return fmt.Errorf("delivery hold belongs to a different Job")
	}
	if err := fn(tx, q, job); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) SandboxDeliveryHeld(ctx context.Context, sandboxID string) (bool, error) {
	return dbsql.New(s.DB).SandboxDeliveryHeld(ctx, sandboxID)
}

func (s Store) JobDeliveryHolds(ctx context.Context, jobID string) ([]core.SandboxDeliveryHold, error) {
	rows, err := dbsql.New(s.DB).ListJobDeliveryHolds(ctx, jobID)
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
