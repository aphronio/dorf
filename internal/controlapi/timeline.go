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
	SessionID string            `json:"session_id"`
	Harness   string            `json:"harness"`
	ThreadID  string            `json:"thread_id"`
	TurnID    string            `json:"turn_id"`
	Status    string            `json:"status"`
	Items     []json.RawMessage `json:"items"`
}

type TimelineSessions interface {
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
	reader, ok := h.sessions.(TimelineSessions)
	if !ok {
		h.fail(w, problem("timeline_unavailable"))
		return
	}
	timeline, err := reader.ReadTimeline(r.Context(), r.PathValue("session"), turnID)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, timeline)
}
