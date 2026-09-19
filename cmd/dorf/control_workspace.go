package main

import (
	"context"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
)

func (a controlAPISessions) ReadWorkspace(ctx context.Context, sessionID string) (persistence.Workspace, error) {
	if _, err := a.loadSession(ctx, sessionID); err != nil {
		return persistence.Workspace{}, err
	}
	reader, ok := a.reader.(controlapi.WorkspaceReader)
	if !ok {
		return persistence.Workspace{}, core.ErrNativeUnavailable
	}
	return reader.ReadWorkspace(ctx, sessionID)
}
