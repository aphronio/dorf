package controlreader

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"reflect"
	"strings"
	"time"
)

const CoherentObservationPath = "/v1/sessions/turn-observation"
const ObservationStreamPath = "/v1/sessions/events/stream"
const ObservationStreamTimeout = time.Minute
const observationStatusInterval = time.Second

type ObservationBinding struct {
	Harness  string `json:"harness"`
	ThreadID string `json:"thread_id"`
	TurnID   string `json:"turn_id"`
}
type ObservationAttention struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}
type TurnObservation struct {
	Type                string                         `json:"type"`
	SessionID           string                         `json:"session_id"`
	TurnID              string                         `json:"turn_id"`
	Status              string                         `json:"status"`
	Binding             *ObservationBinding            `json:"binding"`
	Items               []core.HarnessConversationItem `json:"items"`
	Cursor              *string                        `json:"cursor"`
	FromIndex           int                            `json:"from_index"`
	NextIndex           int                            `json:"next_index"`
	CompletionWatermark *int                           `json:"completion_watermark"`
	State               string                         `json:"state"`
}
type observationRequest struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Cursor    string `json:"cursor,omitempty"`
}
type observationCursor struct {
	SessionID string             `json:"j"`
	TurnID    string             `json:"t"`
	Binding   ObservationBinding `json:"b"`
	Next      int                `json:"n"`
	Digest    string             `json:"h"`
}

func parseObservationCursor(value, sessionID, turnID string) (*observationCursor, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > 4096 {
		return nil, ErrInvalidRequest
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	var cursor observationCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.SessionID != sessionID || cursor.TurnID != turnID || cursor.Next < 0 || len(cursor.Digest) != 64 || !validIdentity(cursor.Binding.Harness) || !validIdentity(cursor.Binding.ThreadID) || !validIdentity(cursor.Binding.TurnID) {
		return nil, ErrInvalidRequest
	}
	canonical, _ := json.Marshal(cursor)
	if base64.RawURLEncoding.EncodeToString(canonical) != value {
		return nil, ErrInvalidRequest
	}
	return &cursor, nil
}

func (s Service) ReadTurnObservation(ctx context.Context, sessionID, turnID, cursor string) (TurnObservation, error) {
	if s.Replies == nil {
		s.Replies = codex.NewReplyFeed()
	}
	result, _, err := s.turnObservation(ctx, observationRequest{SessionID: sessionID, TurnID: turnID, Cursor: cursor}, true)
	return result, err
}

func (s Service) turnObservation(ctx context.Context, input observationRequest, hydrate bool) (TurnObservation, <-chan struct{}, error) {
	result := TurnObservation{Type: "turn.updated", SessionID: input.SessionID, TurnID: input.TurnID, Items: []core.HarnessConversationItem{}, State: "observing"}
	cursor, err := parseObservationCursor(input.Cursor, input.SessionID, input.TurnID)
	if err != nil || !validIdentity(input.SessionID) || !validIdentity(input.TurnID) {
		return result, nil, ErrInvalidRequest
	}
	session, err := s.Store.Session(ctx, input.SessionID)
	if err != nil {
		return result, nil, err
	}
	if !observableSession(session) {
		return result, nil, core.ErrTimelineUnavailable
	}
	owned, err := s.Store.Sandbox(ctx, core.MainSandboxName(session.ID))
	if err != nil {
		return result, nil, err
	}
	if owned.SessionID != session.ID {
		return result, nil, core.ErrTimelineUnavailable
	}
	result.Binding = &ObservationBinding{Harness: session.Harness, ThreadID: session.ThreadID, TurnID: input.TurnID}
	if cursor != nil {
		if cursor.Binding != *result.Binding {
			return result, nil, ErrInvalidRequest
		}
		result.FromIndex, result.NextIndex, result.Cursor = cursor.Next, cursor.Next, &input.Cursor
	}
	binding := codex.ReplyBinding{SessionID: session.ID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce, Harness: session.Harness, ThreadID: session.ThreadID, TurnID: input.TurnID}
	snapshot, changed, found := s.Replies.Read(binding)
	if hydrate && (!found || snapshot.Gap) {
		timeline, err := s.ReadTimeline(ctx, session.ID, input.TurnID)
		if err != nil {
			return result, changed, err
		}
		s.Replies.Seed(binding, timeline.CompletedItems, terminalTurnStatus(timeline.Status))
		s.Replies.Status(binding, timeline.Status)
		snapshot, changed, found = s.Replies.Read(binding)
	}
	return projectTurnObservation(result, input, cursor, snapshot, changed, found)
}

func projectTurnObservation(result TurnObservation, input observationRequest, cursor *observationCursor, snapshot codex.ReplySnapshot, changed <-chan struct{}, found bool) (TurnObservation, <-chan struct{}, error) {
	if !found {
		result.State = "resync_deferred"
		return result, changed, nil
	}
	result.Status = snapshot.Status
	if snapshot.Gap || result.FromIndex > len(snapshot.Items) {
		result.State = "gap"
		return result, changed, nil
	}
	if cursor != nil && cursor.Digest != observationPrefixDigest(snapshot.Items[:result.FromIndex]) {
		result.State = "gap"
		return result, changed, nil
	}
	result.Items = append(result.Items, snapshot.Items[result.FromIndex:]...)
	for i := range result.Items {
		result.Items[i].ClientID, _, _ = strings.Cut(result.Items[i].ClientID, "/")
	}
	result.NextIndex = len(snapshot.Items)
	raw, _ := json.Marshal(observationCursor{SessionID: input.SessionID, TurnID: input.TurnID, Binding: *result.Binding, Next: result.NextIndex, Digest: observationPrefixDigest(snapshot.Items)})
	next := base64.RawURLEncoding.EncodeToString(raw)
	result.Cursor = &next
	if snapshot.Complete && terminalTurnStatus(result.Status) {
		result.Type, result.State = "turn.completed", "complete"
		watermark := result.NextIndex
		result.CompletionWatermark = &watermark
	}
	return result, changed, nil
}

func (s Service) StreamTurnObservation(ctx context.Context, sessionID, turnID, cursor string, emit func(TurnObservation) error) error {
	if s.Replies == nil {
		return core.ErrTimelineUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, ObservationStreamTimeout)
	defer cancel()
	input := observationRequest{SessionID: sessionID, TurnID: turnID, Cursor: cursor}
	ticker := time.NewTicker(observationStatusInterval)
	defer ticker.Stop()
	var previous *TurnObservation
	for {
		result, changed, err := s.turnObservation(ctx, input, false)
		if err != nil {
			return err
		}
		stamp := result
		stamp.Items = nil
		stamp.FromIndex = 0
		if previous == nil || !reflect.DeepEqual(*previous, stamp) || len(result.Items) != 0 {
			if err := emit(result); err != nil {
				return err
			}
			previous = &stamp
			if result.Cursor != nil {
				input.Cursor = *result.Cursor
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-ticker.C:
		}
	}
}
func observationPrefixDigest(items []core.HarnessConversationItem) string {
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for _, item := range items {
		item.NativeItemID = ""
		_ = encoder.Encode(item)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func terminalTurnStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}

func observableSession(session core.Session) bool {
	return session.AdmissionOpen && session.CleanupState == core.CleanupPending && session.ThreadID != ""
}
