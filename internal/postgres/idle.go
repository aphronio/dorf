package postgres

import (
	"context"
	"time"

	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) BeginSandboxActivity(ctx context.Context, jobID string) error {
	affected, err := dbsql.New(s.DB).BeginSandboxActivity(ctx, jobID)
	return expectOneRows(affected, err)
}

func (s Store) FinishSandboxActivity(ctx context.Context, jobID string) error {
	affected, err := dbsql.New(s.DB).FinishSandboxActivity(ctx, jobID)
	return expectOneRows(affected, err)
}

func (s Store) SandboxIdleFor(ctx context.Context, jobID string, duration time.Duration) (bool, error) {
	return dbsql.New(s.DB).SandboxIdleFor(ctx, dbsql.SandboxIdleForParams{JobID: jobID, Seconds: duration.Seconds()})
}
