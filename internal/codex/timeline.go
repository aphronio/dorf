package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/coder/websocket"
)

const timelinePageSize = 1
const timelineMaxBytes = 16 << 20

func (a Agent) ReadTimeline(ctx context.Context, owner provider.Ownership, threadID, turnID string) (core.HarnessTimeline, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	a.Observations = nil
	var timeline core.HarnessTimeline
	err := a.withServer(ctx, owner, func(p *protocol) error {
		var err error
		timeline, err = p.readTimeline(ctx, threadID, turnID)
		return err
	})
	if errors.Is(err, core.ErrTurnNotFound) {
		return core.HarnessTimeline{}, core.ErrTurnNotFound
	}
	if err != nil {
		return core.HarnessTimeline{}, fmt.Errorf("%w: %w", core.ErrTimelineUnavailable, err)
	}
	return timeline, nil
}

type timelinePage struct {
	Data       []json.RawMessage `json:"data"`
	NextCursor *string           `json:"nextCursor"`
}

type timelinePagination struct {
	cursors map[string]bool
	bytes   int
}

func (p *protocol) timelinePage(ctx context.Context, params map[string]any, state *timelinePagination) (timelinePage, error) {
	id := p.nextID
	p.nextID++
	if err := p.send(ctx, map[string]any{"method": "thread/turns/list", "id": id, "params": params}); err != nil {
		return timelinePage{}, err
	}
	for {
		kind, raw, err := p.connection.Read(ctx)
		if err != nil {
			return timelinePage{}, err
		}
		state.bytes += len(raw)
		if kind != websocket.MessageText || state.bytes > timelineMaxBytes {
			return timelinePage{}, core.ErrTimelineUnavailable
		}
		var response struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return timelinePage{}, err
		}
		if response.ID == nil {
			continue
		}
		if *response.ID != id || len(response.Error) > 0 && string(response.Error) != "null" {
			return timelinePage{}, core.ErrTimelineUnavailable
		}
		return state.decodePage(response.Result, params)
	}
}

func (state *timelinePagination) decodePage(raw json.RawMessage, params map[string]any) (timelinePage, error) {
	var page timelinePage
	if err := json.Unmarshal(raw, &page); err != nil {
		return timelinePage{}, err
	}
	if page.Data == nil || len(page.Data) > timelinePageSize {
		return timelinePage{}, core.ErrTimelineUnavailable
	}
	if page.NextCursor != nil {
		cursor := *page.NextCursor
		if cursor == "" || len(cursor) > 4096 || state.cursors[cursor] || len(page.Data) == 0 {
			return timelinePage{}, core.ErrTimelineUnavailable
		}
		state.cursors[cursor] = true
		params["cursor"] = cursor
	}
	return page, nil
}

func (p *protocol) readTimeline(ctx context.Context, threadID, turnID string) (core.HarnessTimeline, error) {
	if threadID == "" {
		return core.HarnessTimeline{}, core.ErrTimelineUnavailable
	}
	p.connection.SetReadLimit(timelineMaxBytes)
	state := timelinePagination{cursors: make(map[string]bool)}
	params := map[string]any{"threadId": threadID, "sortDirection": "desc", "itemsView": "full", "limit": timelinePageSize}
	seen := make(map[string]bool)
	for {
		page, err := p.timelinePage(ctx, params, &state)
		if err != nil {
			return core.HarnessTimeline{}, fmt.Errorf("%w: %w", core.ErrTimelineUnavailable, err)
		}
		for _, raw := range page.Data {
			turn, err := decodeTimelineTurn(raw)
			if err != nil || seen[turn.ID] {
				return core.HarnessTimeline{}, core.ErrTimelineUnavailable
			}
			seen[turn.ID] = true
			if turnID == "" || turn.ID == turnID {
				items, err := conversationItems(turn.Items)
				if err != nil {
					return core.HarnessTimeline{}, err
				}
				completed, _ := completedConversationItems(turn.Items)
				return core.HarnessTimeline{Harness: Harness, ThreadID: threadID, TurnID: turn.ID, Status: turn.Status, Items: items, CompletedItems: completed}, nil
			}
		}
		if page.NextCursor == nil {
			if turnID != "" {
				return core.HarnessTimeline{}, core.ErrTurnNotFound
			}
			return core.HarnessTimeline{}, core.ErrTimelineUnavailable
		}
	}
}

type nativeTimelineTurn struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	ItemsView string            `json:"itemsView"`
	Items     []json.RawMessage `json:"items"`
}

func decodeTimelineTurn(raw json.RawMessage) (nativeTimelineTurn, error) {
	var turn nativeTimelineTurn
	if err := json.Unmarshal(raw, &turn); err != nil {
		return nativeTimelineTurn{}, err
	}
	if turn.ID == "" || turn.ItemsView != "full" || turn.Items == nil || turn.Status != "inProgress" && !terminal(turn.Status) {
		return nativeTimelineTurn{}, core.ErrTimelineUnavailable
	}
	return turn, nil
}

func conversationItems(native []json.RawMessage) ([]json.RawMessage, error) {
	items := make([]json.RawMessage, 0, len(native))
	seen := make(map[string]bool)
	hasObservation := false
	for _, raw := range native {
		var item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || item.ID == "" || item.Type == "" || seen[item.ID] {
			return nil, core.ErrTimelineUnavailable
		}
		seen[item.ID] = true
		if item.Type == "functionCallOutput" {
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				return nil, core.ErrTimelineUnavailable
			}
			hasObservation = hasObservation || observationDeliveryID(fields) != ""
		}
		if item.Type == "userMessage" || item.Type == "agentMessage" {
			items = append(items, raw)
		}
	}
	if len(items) == 0 && !hasObservation {
		return nil, core.ErrTimelineUnavailable
	}
	return items, nil
}

func completedConversationItems(native []json.RawMessage) ([]core.HarnessConversationItem, error) {
	items := make([]core.HarnessConversationItem, 0, len(native))
	for _, raw := range native {
		var fields struct {
			ID       string  `json:"id"`
			Type     string  `json:"type"`
			Phase    *string `json:"phase"`
			ClientID string  `json:"clientId"`
			Text     *string `json:"text"`
			Content  []*struct {
				Text *string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, core.ErrTimelineUnavailable
		}
		for _, block := range fields.Content {
			if block == nil {
				return nil, core.ErrTimelineUnavailable
			}
		}
		entry := core.HarnessConversationItem{Index: len(items), NativeItemID: fields.ID}
		switch fields.Type {
		case "userMessage":
			entry.Kind, entry.ClientID = "input", fields.ClientID
		case "functionCallOutput":
			var item map[string]any
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, core.ErrTimelineUnavailable
			}
			entry.Kind, entry.ClientID = "input", observationDeliveryID(item)
			if entry.ClientID == "" {
				continue
			}
		case "agentMessage":
			if fields.Phase != nil && *fields.Phase != "final_answer" {
				continue
			}
			var item map[string]any
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, core.ErrTimelineUnavailable
			}
			entry.Kind, entry.Text = "reply", agentMessageText(item)
			if entry.Text == "" {
				continue
			}
		default:
			continue
		}
		items = append(items, entry)
	}
	return items, nil
}
