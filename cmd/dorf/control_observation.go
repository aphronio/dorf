package main

import (
	"context"
	"errors"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
)

type turnObservationReader interface {
	ReadTurnObservation(context.Context, string, string, string) (controlreader.TurnObservation, error)
	StreamTurnObservation(context.Context, string, string, string, func(controlreader.TurnObservation) error) error
}

func (a controlAPISessions) ReadTurnObservation(ctx context.Context, sessionID, turnID, cursor string) (controlapi.TurnObservation, error) {
	if _, err := a.loadSession(ctx, sessionID); err != nil {
		return controlapi.TurnObservation{}, err
	}
	reader, ok := a.reader.(turnObservationReader)
	if !ok {
		return controlapi.TurnObservation{}, controlapi.ErrTimelineUnavailable
	}
	result, err := reader.ReadTurnObservation(ctx, sessionID, turnID, cursor)
	if err != nil {
		return controlapi.TurnObservation{}, observationError(err)
	}
	return publicTurnObservation(result), nil
}

func (a controlAPISessions) StreamTurnObservation(ctx context.Context, sessionID, turnID, cursor string, emit func(controlapi.TurnObservation) error) error {
	if _, err := a.loadSession(ctx, sessionID); err != nil {
		return err
	}
	reader, ok := a.reader.(turnObservationReader)
	if !ok {
		return controlapi.ErrTimelineUnavailable
	}
	return observationError(reader.StreamTurnObservation(ctx, sessionID, turnID, cursor, func(value controlreader.TurnObservation) error { return emit(publicTurnObservation(value)) }))
}

func observationError(err error) error {
	switch {
	case errors.Is(err, core.ErrTimelineUnavailable):
		return controlapi.ErrTimelineUnavailable
	case errors.Is(err, controlreader.ErrInvalidRequest):
		return controlapi.ErrInvalidCursor
	case errors.Is(err, controlreader.ErrSessionNotFound):
		return controlapi.ErrTurnNotFound
	default:
		return err
	}
}

func publicTurnObservation(value controlreader.TurnObservation) controlapi.TurnObservation {
	result := controlapi.TurnObservation{Type: value.Type, SessionID: value.SessionID, TurnID: value.TurnID, Status: value.Status, Items: value.Items, Cursor: value.Cursor, FromIndex: value.FromIndex, NextIndex: value.NextIndex, CompletionWatermark: value.CompletionWatermark, State: value.State}
	if value.Binding != nil {
		result.Binding = &controlapi.ObservationBinding{Harness: value.Binding.Harness, ThreadID: value.Binding.ThreadID, TurnID: value.Binding.TurnID}
	}
	return result
}
