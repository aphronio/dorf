package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/aphronio/dorf/internal/controlauth"
)

var (
	ErrTimelineUnavailable = errors.New("control API native timeline is unavailable")
	ErrTurnNotFound        = errors.New("control API native turn not found")
)

type Timeline struct {
	JobID    string            `json:"job_id"`
	Harness  string            `json:"harness"`
	ThreadID string            `json:"thread_id"`
	TurnID   string            `json:"turn_id"`
	Status   string            `json:"status"`
	Items    []json.RawMessage `json:"items"`
}

type TimelineJobs interface {
	ReadTimeline(context.Context, string, string) (Timeline, error)
}

func (h *handler) timelineRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.fail(w, problem("method_not_allowed"))
		return
	}
	if len(r.Header.Values("Content-Type")) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		h.fail(w, problem("body_not_allowed"))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		h.fail(w, problem("invalid_query"))
		return
	}
	turnID := query.Get("turn_id")
	if len(query) > 1 || len(query["turn_id"]) > 1 || (len(query) == 1 && (turnID == "" || len(turnID) > 255 || strings.TrimSpace(turnID) != turnID || strings.ContainsRune(turnID, 0))) {
		h.fail(w, problem("invalid_query"))
		return
	}
	reader, ok := h.jobs.(TimelineJobs)
	if !ok {
		h.fail(w, problem("timeline_unavailable"))
		return
	}
	timeline, err := reader.ReadTimeline(r.Context(), r.PathValue("job"), turnID)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, timeline)
}

type MessageTimelineItem struct {
	Index        int     `json:"index"`
	NativeItemID string  `json:"native_item_id"`
	Kind         string  `json:"kind"`
	Text         *string `json:"text,omitempty"`
	MessageID    string  `json:"message_id,omitempty"`
}

type MessageTimeline struct {
	JobID     string                `json:"job_id"`
	MessageID string                `json:"message_id"`
	Harness   string                `json:"harness"`
	ThreadID  string                `json:"thread_id"`
	TurnID    string                `json:"turn_id"`
	Status    string                `json:"status"`
	Items     []MessageTimelineItem `json:"items"`
}

type MessageTimelineJobs interface {
	ReadMessageTimeline(context.Context, string, string) (MessageTimeline, error)
}

func (h *handler) messageTimelineRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.fail(w, problem("method_not_allowed"))
		return
	}
	if len(r.Header.Values("Content-Type")) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		h.fail(w, problem("body_not_allowed"))
		return
	}
	if r.URL.RawQuery != "" {
		h.fail(w, problem("invalid_query"))
		return
	}
	reader, ok := h.jobs.(MessageTimelineJobs)
	if !ok {
		h.fail(w, problem("timeline_unavailable"))
		return
	}
	timeline, err := reader.ReadMessageTimeline(r.Context(), r.PathValue("job"), r.PathValue("message"))
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, timeline)
}
