package controlclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
)

// SubmitEvent sends once. ClientID is native attribution, never a replay key.
func (c *Client) SubmitEvent(ctx context.Context, sessionID string, event core.NativeEvent) (core.NativeAcknowledgement, error) {
	if sessionID == "" {
		return core.NativeAcknowledgement{}, fmt.Errorf("Session ID is empty")
	}
	if err := event.Validate(); err != nil {
		return core.NativeAcknowledgement{}, err
	}
	var result core.NativeAcknowledgement
	err := c.do(ctx, http.MethodPost, []string{"v1", "sessions", sessionID, "events"}, event, true, "", &result)
	return result, err
}
func (c *Client) Turns(ctx context.Context, sessionID string) (core.HarnessHistory, error) {
	var result core.HarnessHistory
	err := c.do(ctx, http.MethodGet, []string{"v1", "sessions", sessionID, "turns"}, nil, true, "", &result)
	return result, err
}
func (c *Client) History(ctx context.Context, sessionID string) (controlapi.Timeline, error) {
	var result controlapi.Timeline
	err := c.do(ctx, http.MethodGet, []string{"v1", "sessions", sessionID, "history"}, nil, true, "", &result)
	return result, err
}
