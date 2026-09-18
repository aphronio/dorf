package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// ListedSession is the narrow durable identity needed by the remote Session index.
// Full Session state remains available through the canonical single-Session read.
type ListedSession struct {
	CreatedByClientID   string
	CreatedByClientName string
	ClientReference     string
	ID                  string
	AdmittedAt          time.Time
}

// ListSessions returns current public Session identities strictly before the
// optional immutable (admitted_at,id) position.
func (s Store) ListSessions(ctx context.Context, limit int, cursorAt time.Time, cursorID string) ([]ListedSession, error) {
	if limit < 1 || limit > 101 {
		return nil, fmt.Errorf("Session list limit must be between 1 and 101")
	}
	if (cursorID == "") != cursorAt.IsZero() {
		return nil, fmt.Errorf("Session list cursor requires both admitted time and Session ID")
	}
	rows, err := dbsql.New(s.DB).ListSessions(ctx, dbsql.ListSessionsParams{
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
			ID: row.ID, AdmittedAt: row.AdmittedAt,
		})
	}
	return sessions, nil
}
