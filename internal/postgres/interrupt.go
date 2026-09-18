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
func (s Store) RequestMessageInterrupt(ctx context.Context, sessionID, messageID string) (core.MessageInterruptTarget, error) {
	var selected core.MessageInterruptTarget
	err := s.WithSessionFence(ctx, sessionID, func() error {
		q := dbsql.New(s.DB)
		target, err := q.GetMessageInterruptTarget(ctx, dbsql.GetMessageInterruptTargetParams{SessionID: sessionID, MessageID: messageID})
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrMessageInterruptUnavailable
		}
		if err != nil {
			return err
		}
		selected = core.MessageInterruptTarget{AgentRunID: target.ID, SessionID: target.SessionID, InterruptRequested: target.InterruptRequested}
		if target.InterruptRequested || target.State == core.AgentRunCompleted || target.State == core.AgentRunFailed || target.State == core.AgentRunInterrupted {
			return nil
		}
		session, err := s.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		if !session.AdmissionOpen || session.CleanupState != core.CleanupPending {
			return core.ErrMessageAdmissionClosed
		}
		if err := expectOneRows(q.RequestAgentRunInterrupt(ctx, target.ID)); err != nil {
			return err
		}
		selected.InterruptRequested = true
		return nil
	})
	return selected, err
}
