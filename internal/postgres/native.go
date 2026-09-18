package postgres

import (
	"context"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) NativeState(ctx context.Context, sessionID string) (core.NativeState, error) {
	r, err := dbsql.New(s.DB).GetNativeState(ctx, sessionID)
	return core.NativeState{Revision: r.NativeRevision, PendingInputID: r.PendingInputID, PendingTurnID: r.PendingTurnID}, err
}

func (s Store) BindNativeThread(ctx context.Context, sessionID, threadID string) error {
	n, err := dbsql.New(s.DB).BindNativeThread(ctx, dbsql.BindNativeThreadParams{SessionID: sessionID, ThreadID: nullableString(threadID)})
	return expectOneRows(n, err)
}

func (s Store) BeginNativeMutation(ctx context.Context, sessionID, threadID, inputID, turnID string) (int64, error) {
	return dbsql.New(s.DB).BeginNativeMutation(ctx, dbsql.BeginNativeMutationParams{SessionID: sessionID, ThreadID: nullableString(threadID), InputID: inputID, TurnID: turnID})
}

func (s Store) FinishNativeMutation(ctx context.Context, sessionID string, revision int64) error {
	n, err := dbsql.New(s.DB).FinishNativeMutation(ctx, dbsql.FinishNativeMutationParams{SessionID: sessionID, Revision: revision})
	return expectOneRows(n, err)
}
