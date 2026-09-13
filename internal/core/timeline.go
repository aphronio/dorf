package core

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrTimelineUnavailable = errors.New("native timeline is unavailable")
	ErrTurnNotFound        = errors.New("native turn not found")
)

type HarnessTimeline struct {
	Harness  string            `json:"harness"`
	ThreadID string            `json:"thread_id"`
	TurnID   string            `json:"turn_id"`
	Status   string            `json:"status"`
	Items    []json.RawMessage `json:"items"`
}

type SandboxTimelineReader interface {
	ReadTimeline(context.Context, Job, Sandbox, string, string) (HarnessTimeline, error)
}
