package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
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

func (a controlAPISessions) ReadMessageTimeline(ctx context.Context, sessionID, messageID string) (controlapi.MessageTimeline, error) {
	session, err := a.loadSession(ctx, sessionID)
	if err != nil {
		return controlapi.MessageTimeline{}, err
	}
	execution, err := a.store.AgentMessageExecution(ctx, messageID)
	if errors.Is(err, postgres.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return controlapi.MessageTimeline{}, controlapi.ErrMessageNotFound
	}
	if err != nil {
		return controlapi.MessageTimeline{}, err
	}
	if execution.Message.SessionID != session.ID {
		return controlapi.MessageTimeline{}, controlapi.ErrMessageNotFound
	}
	reader, ok := a.reader.(interface {
		ReadMessageTimeline(context.Context, string, string) (core.HarnessTimeline, error)
	})
	if !ok {
		return controlapi.MessageTimeline{}, controlapi.ErrTimelineUnavailable
	}
	timeline, err := reader.ReadMessageTimeline(ctx, session.ID, messageID)
	if err != nil {
		return controlapi.MessageTimeline{}, controlapi.ErrTimelineUnavailable
	}
	items := make([]controlapi.MessageTimelineItem, 0, len(timeline.CompletedItems))
	for _, item := range timeline.CompletedItems {
		projected := controlapi.MessageTimelineItem{Index: item.Index, NativeItemID: item.NativeItemID, Kind: item.Kind, MessageID: item.MessageID}
		if item.Kind == "reply" {
			projected.Text = &item.Text
		}
		items = append(items, projected)
	}
	return controlapi.MessageTimeline{SessionID: session.ID, MessageID: messageID, Harness: timeline.Harness, ThreadID: timeline.ThreadID, TurnID: timeline.TurnID, Status: timeline.Status, Items: items}, nil
}
