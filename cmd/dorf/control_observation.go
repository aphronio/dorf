package main

import (
	"context"
	"errors"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
)

type messageObservationReader interface {
	ReadMessageObservation(context.Context, string, string, string) (controlreader.MessageObservation, error)
	StreamMessageObservation(context.Context, string, string, string, func(controlreader.MessageObservation) error) error
}

func (a controlAPIJobs) ReadMessageObservation(ctx context.Context, jobID, messageID, cursor string) (controlapi.MessageObservation, error) {
	if _, err := a.supportedJob(ctx, jobID); err != nil {
		return controlapi.MessageObservation{}, err
	}
	reader, ok := a.reader.(messageObservationReader)
	if !ok {
		return controlapi.MessageObservation{}, controlapi.ErrTimelineUnavailable
	}
	result, err := reader.ReadMessageObservation(ctx, jobID, messageID, cursor)
	if err != nil {
		return controlapi.MessageObservation{}, observationError(err)
	}
	return publicMessageObservation(result), nil
}

func (a controlAPIJobs) StreamMessageObservation(ctx context.Context, jobID, messageID, cursor string, emit func(controlapi.MessageObservation) error) error {
	if _, err := a.supportedJob(ctx, jobID); err != nil {
		return err
	}
	reader, ok := a.reader.(messageObservationReader)
	if !ok {
		return controlapi.ErrTimelineUnavailable
	}
	return observationError(reader.StreamMessageObservation(ctx, jobID, messageID, cursor, func(value controlreader.MessageObservation) error { return emit(publicMessageObservation(value)) }))
}

func observationError(err error) error {
	switch {
	case errors.Is(err, core.ErrTimelineUnavailable):
		return controlapi.ErrTimelineUnavailable
	case errors.Is(err, controlreader.ErrInvalidRequest):
		return controlapi.ErrInvalidCursor
	case errors.Is(err, controlreader.ErrJobNotFound):
		return controlapi.ErrMessageNotFound
	default:
		return err
	}
}

func publicMessageObservation(value controlreader.MessageObservation) controlapi.MessageObservation {
	result := controlapi.MessageObservation{JobID: value.JobID, MessageID: value.MessageID, Intent: value.Intent, InterruptRequested: value.InterruptRequested, Delivery: controlapi.State{State: value.Delivery.State}, Outcome: value.Outcome, Cursor: value.Cursor, FromIndex: value.FromIndex, NextIndex: value.NextIndex, CompletionWatermark: value.CompletionWatermark, State: value.State, Items: []controlapi.MessageTimelineItem{}}
	if value.Attention != nil {
		result.Attention = &controlapi.Attention{Code: value.Attention.Code, Detail: value.Attention.Detail}
	}
	if value.Binding != nil {
		result.Binding = &controlapi.ObservationBinding{Harness: value.Binding.Harness, ThreadID: value.Binding.ThreadID, TurnID: value.Binding.TurnID}
	}
	for _, item := range value.Items {
		projected := controlapi.MessageTimelineItem{Index: item.Index, NativeItemID: item.NativeItemID, Kind: item.Kind, MessageID: item.MessageID}
		if item.Kind == "reply" {
			projected.Text = &item.Text
		}
		result.Items = append(result.Items, projected)
	}
	return result
}
