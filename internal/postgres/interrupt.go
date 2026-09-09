package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// RequestMessageInterrupt records a stop against the original Turn-starting run,
// including when the caller addresses a Steer Message attached to that Turn.
func (s Store) RequestMessageInterrupt(ctx context.Context, jobID, messageID string) error {
	return s.WithJobFence(ctx, jobID, func() error {
		q := dbsql.New(s.DB)
		target, err := q.GetMessageInterruptTarget(ctx, dbsql.GetMessageInterruptTargetParams{JobID: jobID, MessageID: messageID})
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrMessageInterruptUnavailable
		}
		if err != nil {
			return err
		}
		if target.InterruptRequested || target.State == core.AgentRunCompleted || target.State == core.AgentRunFailed || target.State == core.AgentRunInterrupted {
			return nil
		}
		job, err := s.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
			return core.ErrMessageAdmissionClosed
		}
		return expectOneRows(q.RequestAgentRunInterrupt(ctx, target.ID))
	})
}
