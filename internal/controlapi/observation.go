package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/controlauth"
)

type ObservationBinding struct {
	Harness  string `json:"harness"`
	ThreadID string `json:"thread_id"`
	TurnID   string `json:"turn_id"`
}

type MessageObservation struct {
	Attention           *Attention            `json:"attention"`
	SessionID           string                `json:"session_id"`
	MessageID           string                `json:"message_id"`
	Intent              string                `json:"intent"`
	InterruptRequested  bool                  `json:"interrupt_requested"`
	Delivery            State                 `json:"delivery"`
	Outcome             *string               `json:"outcome"`
	Binding             *ObservationBinding   `json:"binding"`
	Items               []MessageTimelineItem `json:"items"`
	Cursor              *string               `json:"cursor"`
	FromIndex           int                   `json:"from_index"`
	NextIndex           int                   `json:"next_index"`
	CompletionWatermark *int                  `json:"completion_watermark"`
	State               string                `json:"state"`
}

type MessageObservationSessions interface {
	ReadMessageObservation(context.Context, string, string, string) (MessageObservation, error)
	StreamMessageObservation(context.Context, string, string, string, func(MessageObservation) error) error
}

func observationQuery(r *http.Request, stream bool) (string, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) > 1 || len(query["cursor"]) > 1 {
		return "", false
	}
	cursor := query.Get("cursor")
	if len(query) == 1 && cursor == "" {
		return "", false
	}
	ids := r.Header.Values("Last-Event-ID")
	if len(ids) > 1 || !stream && len(ids) > 0 {
		return "", false
	}
	if len(ids) == 1 {
		if ids[0] == "" || cursor != "" && cursor != ids[0] {
			return "", false
		}
		cursor = ids[0]
	}
	return cursor, len(cursor) <= 4096 && strings.TrimSpace(cursor) == cursor
}

func (h *handler) messageObservationRoute(w http.ResponseWriter, r *http.Request, client controlauth.Client) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.fail(w, problem("method_not_allowed"))
		return
	}
	if len(r.Header.Values("Content-Type")) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		h.fail(w, problem("body_not_allowed"))
		return
	}
	stream := strings.HasSuffix(r.URL.Path, "/stream")
	cursor, ok := observationQuery(r, stream)
	if !ok {
		h.fail(w, problem("invalid_cursor"))
		return
	}
	reader, ok := h.sessions.(MessageObservationSessions)
	if !ok {
		h.fail(w, problem("timeline_unavailable"))
		return
	}
	if !stream {
		value, err := reader.ReadMessageObservation(r.Context(), r.PathValue("session"), r.PathValue("message"), cursor)
		if err != nil {
			h.serviceError(w, r, err)
			return
		}
		h.reply(w, http.StatusOK, value)
		return
	}
	accept := r.Header.Values("Accept")
	if len(accept) != 1 || accept[0] != "text/event-stream" {
		h.fail(w, problem("not_acceptable"))
		return
	}
	h.streamMessageObservation(w, r, client, reader, cursor)
}

func (h *handler) streamMessageObservation(w http.ResponseWriter, r *http.Request, client controlauth.Client, reader MessageObservationSessions, cursor string) {
	deadline := streamAuthenticationDeadline(client)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	stop := context.AfterFunc(h.shutdown, cancel)
	defer stop()
	frames := observationFrames(ctx, reader, r.PathValue("session"), r.PathValue("message"), cursor)
	controller := http.NewResponseController(w)
	started := false
	heartbeat := time.NewTicker(watchKeepaliveInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			if !started {
				h.observationStreamError(w, r, ctx, ctx.Err())
			}
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			if frame.err != nil {
				if !started {
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
						h.fail(w, problem("unauthenticated"))
					} else {
						h.serviceError(w, r, frame.err)
					}
				}
				return
			}
			if !started {
				w.Header().Set("Content-Type", "text/event-stream")
				started = true
			}
			if err := writeObservationFrame(w, controller, frame.value); err != nil {
				return
			}
		case <-heartbeat.C:
			if started && writeObservationHeartbeat(w, controller) != nil {
				return
			}
		}
	}
}

type observationFrame struct {
	value MessageObservation
	err   error
}

// One pending frame bounds a slow consumer. Cancellation propagates through
// the private worker request and ordered closure never drops its last frame.
func observationFrames(ctx context.Context, reader MessageObservationSessions, sessionID, messageID, cursor string) <-chan observationFrame {
	frames := make(chan observationFrame, 1)
	go func() {
		defer close(frames)
		err := reader.StreamMessageObservation(ctx, sessionID, messageID, cursor, func(value MessageObservation) error {
			select {
			case frames <- observationFrame{value: value}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil {
			select {
			case frames <- observationFrame{err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return frames
}

func writeObservationFrame(w io.Writer, controller *http.ResponseController, value MessageObservation) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return streamWrite(controller, func() error {
		prefix := "event: observation\n"
		if value.Cursor != nil {
			prefix += "id: " + *value.Cursor + "\n"
		}
		if _, err := io.WriteString(w, prefix+"data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		return controller.Flush()
	})
}

func writeObservationHeartbeat(w io.Writer, controller *http.ResponseController) error {
	return streamWrite(controller, func() error {
		if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
			return err
		}
		return controller.Flush()
	})
}

func (h *handler) observationStreamError(w http.ResponseWriter, r *http.Request, ctx context.Context, err error) {
	if r.Context().Err() != nil || errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		h.fail(w, problem("unauthenticated"))
		return
	}
	h.serviceError(w, r, err)
}
