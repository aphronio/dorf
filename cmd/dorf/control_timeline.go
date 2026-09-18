package main

import (
	"context"

	"errors"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
)

func (a controlAPISessions) ReadTimeline(ctx context.Context, sessionID, turnID string) (controlapi.Timeline, error) {
	session, err := a.loadSession(ctx, sessionID)
	if err != nil {
		return controlapi.Timeline{}, err
	}
	reader, ok := a.reader.(interface {
		ReadTimeline(context.Context, string, string) (core.HarnessTimeline, error)
	})
	if !ok {
		return controlapi.Timeline{}, controlapi.ErrTimelineUnavailable
	}
	timeline, err := reader.ReadTimeline(ctx, session.ID, turnID)
	if err != nil {
		if errors.Is(err, core.ErrTurnNotFound) {
			return controlapi.Timeline{}, controlapi.ErrTurnNotFound
		}
		if errors.Is(err, controlreader.ErrSessionNotFound) {
			return controlapi.Timeline{}, controlapi.ErrSessionNotFound
		}
		return controlapi.Timeline{}, controlapi.ErrTimelineUnavailable
	}
	return controlapi.Timeline{SessionID: session.ID, Harness: timeline.Harness, ThreadID: timeline.ThreadID, TurnID: timeline.TurnID, Status: timeline.Status, Items: timeline.Items}, nil
}
