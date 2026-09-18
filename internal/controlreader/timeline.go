package controlreader

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

const TimelinePath = "/v1/sessions/timeline"

var ErrSessionNotFound = errors.New("control reader Session not found")

type timelineStore interface {
	Sandboxes(context.Context, string) ([]core.Sandbox, error)
	Deliveries(context.Context, string) ([]core.Delivery, error)
}

type timelineRequest struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

func (s Service) ReadTimeline(ctx context.Context, sessionID, turnID string) (core.HarnessTimeline, error) {
	if !validIdentity(sessionID) {
		return core.HarnessTimeline{}, ErrSessionNotFound
	}
	if turnID != "" && !validIdentity(turnID) {
		return core.HarnessTimeline{}, ErrInvalidRequest
	}
	store, ok := s.Store.(timelineStore)
	if !ok || s.Runtimes == nil {
		return core.HarnessTimeline{}, core.ErrTimelineUnavailable
	}
	var result core.HarnessTimeline
	var idleRuntime core.Execution
	defer func() { core.ReconcileIdle(ctx, idleRuntime, sessionID) }()
	err := s.Store.WithSessionFence(ctx, sessionID, func() error {
		session, err := s.Store.Session(ctx, sessionID)
		if errors.Is(err, postgres.ErrNotFound) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		if session.ID != sessionID || session.CleanupState != core.CleanupPending {
			return core.ErrTimelineUnavailable
		}
		owned, harness, threadID, err := timelineBinding(ctx, store, session)
		if err != nil {
			return err
		}
		runtime, session, err := s.sandboxAuthority(ctx, owned)
		if err != nil || runtime.Timeline == nil {
			return core.ErrTimelineUnavailable
		}
		idleRuntime = runtime.Execution
		err = core.WithSandboxActivity(ctx, s.Store, session.ID, func() error {
			result, err = runtime.Timeline.ReadTimeline(ctx, session, owned, threadID, turnID)
			return err
		})
		if err != nil {
			return errors.Join(core.ErrTimelineUnavailable, err)
		}
		result.CompletedItems = nil
		return validateTimeline(result, harness, threadID, turnID)
	})
	if err != nil {
		return core.HarnessTimeline{}, err
	}
	return result, nil
}

func timelineBinding(ctx context.Context, store timelineStore, session core.Session) (core.Sandbox, string, string, error) {
	if !validIdentity(session.Harness) || !validIdentity(session.ThreadID) {
		return core.Sandbox{}, "", "", core.ErrTimelineUnavailable
	}
	sandboxes, err := store.Sandboxes(ctx, session.ID)
	if err != nil {
		return core.Sandbox{}, "", "", err
	}
	owned, err := defaultTimelineSandbox(sandboxes, session.ID)
	return owned, session.Harness, session.ThreadID, err
}

func (c Client) ReadTimeline(ctx context.Context, sessionID, turnID string) (core.HarnessTimeline, error) {
	response, err := c.request(ctx, TimelinePath, timelineRequest{SessionID: sessionID, TurnID: turnID})
	if err != nil {
		return core.HarnessTimeline{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return core.HarnessTimeline{}, decodeProblem(response)
	}
	var result core.HarnessTimeline
	if err := decodeJSONResponse(response, &result, MaxObservationBytes, "timeline"); err != nil {
		return core.HarnessTimeline{}, err
	}
	return result, nil
}

func validateTimeline(result core.HarnessTimeline, harness, threadID, turnID string) error {
	if result.Harness != harness || result.ThreadID != threadID || result.TurnID == "" || turnID != "" && result.TurnID != turnID || len(result.Items) == 0 {
		return core.ErrTimelineUnavailable
	}
	if result.Status != "inProgress" && !terminalMessageOutcome(result.Status) {
		return core.ErrTimelineUnavailable
	}
	return nil
}

func defaultTimelineSandbox(sandboxes []core.Sandbox, sessionID string) (core.Sandbox, error) {
	var owned core.Sandbox
	for _, candidate := range sandboxes {
		if candidate.Name != core.DefaultSandbox {
			continue
		}
		if owned.ID != "" || candidate.SessionID != sessionID || !validIdentity(candidate.ID) || !validIdentity(candidate.OwnershipNonce) {
			return core.Sandbox{}, core.ErrTimelineUnavailable
		}
		owned = candidate
	}
	if owned.ID == "" {
		return core.Sandbox{}, core.ErrTimelineUnavailable
	}
	return owned, nil
}

func (s Service) ReadMessageTimeline(ctx context.Context, sessionID, messageID string) (core.HarnessTimeline, error) {
	store, ok := s.Store.(timelineStore)
	if !ok || s.Runtimes == nil {
		return core.HarnessTimeline{}, core.ErrTimelineUnavailable
	}
	var result core.HarnessTimeline
	var idleRuntime core.Execution
	defer func() { core.ReconcileIdle(ctx, idleRuntime, sessionID) }()
	err := s.Store.WithSessionFence(ctx, sessionID, func() error {
		execution, err := s.Store.AgentMessageExecution(ctx, messageID)
		if errors.Is(err, postgres.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return core.ErrTimelineUnavailable
		}
		if err != nil {
			return err
		}
		session, run, owned := execution.Session, execution.AgentRun, execution.Sandbox
		if err := validateMessageTimelineBinding(execution, sessionID, messageID); err != nil {
			return err
		}
		runtime, session, err := s.sandboxAuthority(ctx, owned)
		if err != nil || runtime.Timeline == nil {
			return core.ErrTimelineUnavailable
		}
		idleRuntime = runtime.Execution
		err = core.WithSandboxActivity(ctx, s.Store, session.ID, func() error {
			result, err = runtime.Timeline.ReadTimeline(ctx, session, owned, run.ThreadID, run.TurnID)
			return err
		})
		if err != nil {
			return errors.Join(core.ErrTimelineUnavailable, err)
		}
		if err := validateTimeline(result, run.Harness, run.ThreadID, run.TurnID); err != nil {
			return err
		}
		if s.Replies != nil {
			s.Replies.Seed(codex.ReplyBinding{SessionID: sessionID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce, Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID}, result.CompletedItems, terminalMessageOutcome(result.Status))
		}
		deliveries, err := store.Deliveries(ctx, sessionID)
		if err != nil {
			return err
		}
		if err := bindTimelineInputs(&result, deliveries, run); err != nil {
			return err
		}
		result.Items = nil
		return nil
	})
	if err != nil {
		return core.HarnessTimeline{}, err
	}
	return result, nil
}

func (c Client) ReadMessageTimeline(ctx context.Context, sessionID, messageID string) (core.HarnessTimeline, error) {
	response, err := c.request(ctx, TimelinePath, timelineRequest{SessionID: sessionID, MessageID: messageID})
	if err != nil {
		return core.HarnessTimeline{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return core.HarnessTimeline{}, decodeProblem(response)
	}
	var result core.HarnessTimeline
	if err := decodeJSONResponse(response, &result, MaxObservationBytes, "message timeline"); err != nil {
		return core.HarnessTimeline{}, err
	}
	return result, nil
}

func validateMessageTimelineBinding(execution core.AgentMessageExecution, sessionID, messageID string) error {
	session, run, owned := execution.Session, execution.AgentRun, execution.Sandbox
	if session.ID != sessionID || session.CleanupState != core.CleanupPending || execution.Message.ID != messageID || execution.Message.SessionID != sessionID || run.SessionID != sessionID || run.MessageID != messageID || run.SandboxID != owned.ID || owned.SessionID != sessionID {
		return core.ErrTimelineUnavailable
	}
	for _, identity := range []string{owned.ID, owned.OwnershipNonce, run.ID, run.Harness, run.ThreadID, run.TurnID} {
		if !validIdentity(identity) {
			return core.ErrTimelineUnavailable
		}
	}
	return nil
}

func bindTimelineInputs(result *core.HarnessTimeline, deliveries []core.Delivery, run core.AgentRun) error {
	if result.CompletedItems == nil {
		return core.ErrTimelineUnavailable
	}
	sources := make(map[string]string)
	for _, delivery := range deliveries {
		source := delivery.AgentRun
		if source.SessionID == run.SessionID && delivery.Message.SessionID == run.SessionID && source.MessageID == delivery.Message.ID && source.SandboxID == run.SandboxID && source.Harness == run.Harness && source.ThreadID == run.ThreadID && source.TurnID == run.TurnID {
			sources[source.ID] = delivery.Message.ID
		}
	}
	for i := range result.CompletedItems {
		item := &result.CompletedItems[i]
		if item.Index != i || !validIdentity(item.NativeItemID) {
			return core.ErrTimelineUnavailable
		}
		item.MessageID = ""
		switch item.Kind {
		case "input":
			item.MessageID = sources[item.ClientID]
		case "reply":
		default:
			return core.ErrTimelineUnavailable
		}
		item.ClientID = ""
	}
	return nil
}
