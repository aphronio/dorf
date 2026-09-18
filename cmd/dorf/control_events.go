package main

import (
	"context"

	"github.com/aphronio/dorf/internal/core"
)

type nativeSessionReader interface {
	SubmitEvent(context.Context, string, core.NativeEvent) (core.NativeAcknowledgement, error)
	ReadNativeTurns(context.Context, string) (core.HarnessHistory, error)
}

func (a controlAPISessions) SubmitEvent(ctx context.Context, sessionID string, event core.NativeEvent) (core.NativeAcknowledgement, error) {
	if _, err := a.loadSession(ctx, sessionID); err != nil {
		return core.NativeAcknowledgement{}, err
	}
	if err := validateInputAttachments(event.Attachments); err != nil {
		return core.NativeAcknowledgement{}, err
	}
	reader, ok := a.reader.(nativeSessionReader)
	if !ok {
		return core.NativeAcknowledgement{}, core.ErrNativeUnavailable
	}
	return reader.SubmitEvent(ctx, sessionID, event)
}

func (a controlAPISessions) ReadNativeTurns(ctx context.Context, sessionID string) (core.HarnessHistory, error) {
	if _, err := a.loadSession(ctx, sessionID); err != nil {
		return core.HarnessHistory{}, err
	}
	reader, ok := a.reader.(nativeSessionReader)
	if !ok {
		return core.HarnessHistory{}, core.ErrNativeUnavailable
	}
	return reader.ReadNativeTurns(ctx, sessionID)
}
