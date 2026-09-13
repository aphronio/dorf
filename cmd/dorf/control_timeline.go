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

func (a controlAPIJobs) ReadTimeline(ctx context.Context, jobID, turnID string) (controlapi.Timeline, error) {
	job, err := a.supportedJob(ctx, jobID)
	if err != nil {
		return controlapi.Timeline{}, err
	}
	reader, ok := a.reader.(interface {
		ReadTimeline(context.Context, string, string) (core.HarnessTimeline, error)
	})
	if !ok {
		return controlapi.Timeline{}, controlapi.ErrTimelineUnavailable
	}
	timeline, err := reader.ReadTimeline(ctx, job.ID, turnID)
	if err != nil {
		if errors.Is(err, core.ErrTurnNotFound) {
			return controlapi.Timeline{}, controlapi.ErrTurnNotFound
		}
		if errors.Is(err, controlreader.ErrJobNotFound) {
			return controlapi.Timeline{}, controlapi.ErrJobNotFound
		}
		return controlapi.Timeline{}, controlapi.ErrTimelineUnavailable
	}
	return controlapi.Timeline{JobID: job.ID, Harness: timeline.Harness, ThreadID: timeline.ThreadID, TurnID: timeline.TurnID, Status: timeline.Status, Items: timeline.Items}, nil
}

func (a controlAPIJobs) ReadMessageTimeline(ctx context.Context, jobID, messageID string) (controlapi.MessageTimeline, error) {
	job, err := a.supportedJob(ctx, jobID)
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
	if execution.Message.JobID != job.ID {
		return controlapi.MessageTimeline{}, controlapi.ErrMessageNotFound
	}
	reader, ok := a.reader.(interface {
		ReadMessageTimeline(context.Context, string, string) (core.HarnessTimeline, error)
	})
	if !ok {
		return controlapi.MessageTimeline{}, controlapi.ErrTimelineUnavailable
	}
	timeline, err := reader.ReadMessageTimeline(ctx, job.ID, messageID)
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
	return controlapi.MessageTimeline{JobID: job.ID, MessageID: messageID, Harness: timeline.Harness, ThreadID: timeline.ThreadID, TurnID: timeline.TurnID, Status: timeline.Status, Items: items}, nil
}
