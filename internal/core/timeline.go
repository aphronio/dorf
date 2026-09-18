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

type HarnessConversationItem struct {
	Index        int    `json:"index"`
	NativeItemID string `json:"native_item_id"`
	Kind         string `json:"kind"`
	Text         string `json:"text,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
}

type HarnessTimeline struct {
	CompletedItems []HarnessConversationItem `json:"completed_items"`
	Harness        string                    `json:"harness"`
	ThreadID       string                    `json:"thread_id"`
	TurnID         string                    `json:"turn_id"`
	Status         string                    `json:"status"`
	Items          []json.RawMessage         `json:"items"`
}

type SandboxTimelineReader interface {
	ReadTimeline(context.Context, Session, Sandbox, string, string) (HarnessTimeline, error)
}
