package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// ListedSession is the narrow durable identity needed by the remote Session index.
// Full Session state remains available through the canonical single-Session read.
type ListedSession struct {
	CreatedByClientID   string
	CreatedByClientName string
	ClientReference     string
	ID                  string
	Workflow            core.WorkflowName
	WorkflowRevision    string
	AdmittedAt          time.Time
}

// ListSupportedSessions returns current public Session identities strictly before the
// optional immutable (admitted_at,id) position.
func (s Store) ListSupportedSessions(ctx context.Context, limit int, cursorAt time.Time, cursorID string) ([]ListedSession, error) {
	if limit < 1 || limit > 101 {
		return nil, fmt.Errorf("Session list limit must be between 1 and 101")
	}
	if (cursorID == "") != cursorAt.IsZero() {
		return nil, fmt.Errorf("Session list cursor requires both admitted time and Session ID")
	}
	rows, err := dbsql.New(s.DB).ListSupportedSessions(ctx, dbsql.ListSupportedSessionsParams{
		HasCursor:        cursorID != "",
		CursorAdmittedAt: cursorAt,
		CursorID:         cursorID,
		PageSize:         int32(limit),
	})
	if err != nil {
		return nil, err
	}
	sessions := make([]ListedSession, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, ListedSession{
			CreatedByClientID: row.CreatedByClientID, CreatedByClientName: row.CreatedByClientName, ClientReference: row.ClientReference,
			ID: row.ID, Workflow: row.WorkflowName, WorkflowRevision: row.WorkflowRevision, AdmittedAt: row.AdmittedAt,
		})
	}
	return sessions, nil
}
