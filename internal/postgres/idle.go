package postgres

import (
	"context"
	"time"

	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) BeginSandboxActivity(ctx context.Context, sessionID string) error {
	affected, err := dbsql.New(s.DB).BeginSandboxActivity(ctx, sessionID)
	return expectOneRows(affected, err)
}

func (s Store) FinishSandboxActivity(ctx context.Context, sessionID string) error {
	affected, err := dbsql.New(s.DB).FinishSandboxActivity(ctx, sessionID)
	return expectOneRows(affected, err)
}

func (s Store) SandboxIdleFor(ctx context.Context, sessionID string, duration time.Duration) (bool, error) {
	return dbsql.New(s.DB).SandboxIdleFor(ctx, dbsql.SandboxIdleForParams{SessionID: sessionID, Seconds: duration.Seconds()})
}
