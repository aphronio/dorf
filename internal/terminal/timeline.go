package terminal

import (
	"context"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type TimelineHarness interface {
	ReadTimeline(context.Context, provider.Ownership, string, string) (core.HarnessTimeline, error)
}

func (e Externals) ReadTimeline(ctx context.Context, session core.Session, owned core.Sandbox, threadID, turnID string) (core.HarnessTimeline, error) {
	reader, ok := e.Agent.(TimelineHarness)
	if !ok || owned.SessionID != session.ID {
		return core.HarnessTimeline{}, core.ErrTimelineUnavailable
	}
	return reader.ReadTimeline(ctx, ownershipMetadata(owned), threadID, turnID)
}
