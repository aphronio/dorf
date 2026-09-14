package controlreader

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

const CoherentObservationPath = "/v1/messages/observation"
const ObservationStreamPath = "/v1/messages/observation/stream"
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

type MessageObservation struct {
	Attention          *ObservationAttention `json:"attention"`
	JobID              string                `json:"job_id"`
	MessageID          string                `json:"message_id"`
	Intent             string                `json:"intent"`
	InterruptRequested bool                  `json:"interrupt_requested"`
	Delivery           struct {
		State string `json:"state"`
	} `json:"delivery"`
	Outcome             *string                        `json:"outcome"`
	Binding             *ObservationBinding            `json:"binding"`
	Items               []core.HarnessConversationItem `json:"items"`
	Cursor              *string                        `json:"cursor"`
	FromIndex           int                            `json:"from_index"`
	NextIndex           int                            `json:"next_index"`
	CompletionWatermark *int                           `json:"completion_watermark"`
	State               string                         `json:"state"`
}

type observationRequest struct {
	JobID     string `json:"job_id"`
	MessageID string `json:"message_id"`
	Cursor    string `json:"cursor,omitempty"`
}

type observationCursor struct {
	JobID     string             `json:"j"`
	MessageID string             `json:"m"`
	Binding   ObservationBinding `json:"b"`
	Next      int                `json:"n"`
	Digest    string             `json:"h"`
}

func parseObservationCursor(value, jobID, messageID string) (*observationCursor, error) {
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
	if json.Unmarshal(raw, &cursor) != nil || cursor.JobID != jobID || cursor.MessageID != messageID || cursor.Next < 0 || len(cursor.Digest) != 64 || !validIdentity(cursor.Binding.Harness) || !validIdentity(cursor.Binding.ThreadID) || !validIdentity(cursor.Binding.TurnID) {
		return nil, ErrInvalidRequest
	}
	canonical, _ := json.Marshal(cursor)
	if base64.RawURLEncoding.EncodeToString(canonical) != value {
		return nil, ErrInvalidRequest
	}
	return &cursor, nil
}

func (s Service) ReadMessageObservation(ctx context.Context, jobID, messageID, cursor string) (MessageObservation, error) {
	// Explicit snapshots remain available without a worker event source.
	if s.Replies == nil {
		s.Replies = codex.NewReplyFeed()
	}
	result, _, err := s.messageObservation(ctx, observationRequest{JobID: jobID, MessageID: messageID, Cursor: cursor}, true)
	return result, err
}

func (s Service) messageObservation(ctx context.Context, input observationRequest, hydrate bool) (MessageObservation, <-chan struct{}, error) {
	result := MessageObservation{JobID: input.JobID, MessageID: input.MessageID, Items: []core.HarnessConversationItem{}, State: "pending"}
	cursor, err := parseObservationCursor(input.Cursor, input.JobID, input.MessageID)
	if err != nil || !validIdentity(input.JobID) || !validIdentity(input.MessageID) {
		return result, nil, ErrInvalidRequest
	}
	execution, err := s.observationExecution(ctx, input)
	if err != nil {
		return result, nil, err
	}
	projectObservationStatus(&result, execution)
	if err := s.projectObservationAttention(ctx, &result, execution); err != nil {
		return result, nil, err
	}
	run := execution.AgentRun
	if run.ThreadID == "" || run.TurnID == "" {
		if cursor != nil {
			return result, nil, ErrInvalidRequest
		}
		if result.Outcome != nil {
			zero := 0
			result.CompletionWatermark = &zero
			result.State = "complete"
		}
		return result, nil, nil
	}
	if err := validateMessageTimelineBinding(execution, input.JobID, input.MessageID); err != nil {
		return result, nil, err
	}
	result.Binding = &ObservationBinding{Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID}
	if cursor != nil {
		if cursor.Binding != *result.Binding {
			return result, nil, ErrInvalidRequest
		}
		result.FromIndex, result.NextIndex, result.Cursor = cursor.Next, cursor.Next, &input.Cursor
	}
	return s.readObservationFeed(ctx, result, cursor, execution, hydrate)
}

func (s Service) observationExecution(ctx context.Context, input observationRequest) (core.AgentMessageExecution, error) {
	execution, err := s.Store.AgentMessageExecution(ctx, input.MessageID)
	if errors.Is(err, postgres.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return execution, ErrJobNotFound
	}
	if err != nil {
		return execution, err
	}
	if execution.Job.ID != input.JobID || execution.Message.JobID != input.JobID || execution.Message.ID != input.MessageID || execution.AgentRun.MessageID != input.MessageID {
		return execution, ErrJobNotFound
	}
	return execution, nil
}

func (s Service) readObservationFeed(ctx context.Context, result MessageObservation, cursor *observationCursor, execution core.AgentMessageExecution, hydrate bool) (MessageObservation, <-chan struct{}, error) {
	run := execution.AgentRun
	binding := codex.ReplyBinding{JobID: result.JobID, SandboxID: execution.Sandbox.ID, OwnershipNonce: execution.Sandbox.OwnershipNonce, Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID}
	if s.Replies == nil {
		result.State = "resync_deferred"
		return result, nil, nil
	}
	snapshot, changed, found := s.Replies.Read(binding)
	if hydrate && (!found || snapshot.Gap || result.Outcome != nil && !snapshot.Complete) {
		_, readErr := s.ReadMessageTimeline(ctx, result.JobID, result.MessageID)
		if readErr != nil {
			return result, changed, readErr
		}
		snapshot, changed, found = s.Replies.Read(binding)
	}
	if !found {
		result.State = "resync_deferred"
		return result, changed, nil
	}
	if snapshot.Gap || result.FromIndex > len(snapshot.Items) {
		result.State = "gap"
		return result, changed, nil
	}
	return s.projectObservationItems(ctx, result, cursor, run, snapshot, changed)
}

func (s Service) projectObservationItems(ctx context.Context, result MessageObservation, cursor *observationCursor, run core.AgentRun, snapshot codex.ReplySnapshot, changed <-chan struct{}) (MessageObservation, <-chan struct{}, error) {
	if cursor != nil && cursor.Digest != observationPrefixDigest(snapshot.Items[:result.FromIndex]) {
		result.State = "gap"
		return result, changed, nil
	}
	digest := observationPrefixDigest(snapshot.Items)
	timeline := core.HarnessTimeline{CompletedItems: snapshot.Items}
	deliveries, err := s.observationInputSources(ctx, result.JobID, snapshot.Items[result.FromIndex:])
	if err != nil {
		return result, changed, err
	}
	if err := bindTimelineInputs(&timeline, deliveries, run); err != nil {
		return result, changed, err
	}
	result.Items = append(result.Items, timeline.CompletedItems[result.FromIndex:]...)
	result.NextIndex = len(timeline.CompletedItems)
	raw, _ := json.Marshal(observationCursor{JobID: result.JobID, MessageID: result.MessageID, Binding: *result.Binding, Next: result.NextIndex, Digest: digest})
	next := base64.RawURLEncoding.EncodeToString(raw)
	result.Cursor, result.State = &next, "observing"
	if snapshot.Complete && result.Outcome != nil {
		watermark := result.NextIndex
		result.CompletionWatermark, result.State = &watermark, "complete"
	}
	return result, changed, nil
}

// Once the input prefix is acknowledged, status polls and reply-only deltas
// need no Job delivery history. Input attribution is resolved only when sent.
func (s Service) observationInputSources(ctx context.Context, jobID string, items []core.HarnessConversationItem) ([]core.Delivery, error) {
	for _, item := range items {
		if item.Kind != "input" {
			continue
		}
		store, ok := s.Store.(timelineStore)
		if !ok {
			return nil, ErrUnavailable
		}
		return store.Deliveries(ctx, jobID)
	}
	return nil, nil
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

func projectObservationStatus(result *MessageObservation, execution core.AgentMessageExecution) {
	run := execution.AgentRun
	result.Intent, result.InterruptRequested = string(execution.Message.Intent), run.InterruptRequested
	switch run.State {
	case core.AgentRunPending, core.AgentRunSubmitting:
		result.Delivery.State = "accepted"
	case core.AgentRunActive, core.AgentRunUncertain:
		result.Delivery.State = "running"
	case core.AgentRunCompleted:
		result.Delivery.State = "completed"
	case core.AgentRunFailed, core.AgentRunInterrupted:
		result.Delivery.State = "failed"
	}
	if run.State != core.AgentRunCompleted && run.State != core.AgentRunFailed && run.State != core.AgentRunInterrupted {
		return
	}
	outcome := run.TurnOutcome
	if outcome == "" && (run.State == core.AgentRunFailed || run.State == core.AgentRunInterrupted) {
		outcome = string(run.State)
	}
	if terminalMessageOutcome(outcome) {
		result.Outcome = &outcome
	}
}

// StreamMessageObservation waits on native projection changes. Its compact
// custody poll settles durable outcome and never calls a Harness or activity API.
func (s Service) StreamMessageObservation(ctx context.Context, jobID, messageID, cursor string, emit func(MessageObservation) error) error {
	if s.Replies == nil {
		return core.ErrTimelineUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, ObservationStreamTimeout)
	defer cancel()
	input := observationRequest{JobID: jobID, MessageID: messageID, Cursor: cursor}
	ticker := time.NewTicker(observationStatusInterval)
	defer ticker.Stop()
	var previous *MessageObservation
	for {
		result, changed, err := s.messageObservation(ctx, input, false)
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

func (s Service) projectObservationAttention(ctx context.Context, result *MessageObservation, execution core.AgentMessageExecution) error {
	if result.Outcome != nil {
		return nil
	}
	code := ""
	switch {
	case !execution.Job.AdmissionOpen || execution.Job.CleanupState != core.CleanupPending:
		code = "job_closed"
	case execution.Job.WorkflowAttention != "":
		code = "job_attention"
	}
	if code == "" && s.ObservationAttention != nil {
		var err error
		code, err = s.ObservationAttention(ctx, execution.Job)
		if err != nil {
			return err
		}
	}
	if code == "" && (execution.AgentRun.Attention != "" || execution.AgentRun.State == core.AgentRunUncertain) {
		code = "agent_delivery_attention"
	}
	if code != "" {
		result.Attention = &ObservationAttention{Code: code, Detail: "Message execution needs operator attention; inspect the deployment service logs."}
	}
	return nil
}
