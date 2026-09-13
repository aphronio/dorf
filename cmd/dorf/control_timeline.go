package main

import (
	"context"
	"errors"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
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
