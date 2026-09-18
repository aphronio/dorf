package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// MessageWakeV1 is persisted by Absurd under one immutable Session-local FIFO event.
type MessageWakeV1 struct {
	SessionID string `json:"job_id"`
	Sequence  int64  `json:"sequence"`
}

// SessionExecutionWakeV1 is a disposable hint that asks a Session's current task to
// reload authoritative workflow and Harness state.
type SessionExecutionWakeV1 struct {
	SessionID string `json:"job_id"`
	Revision  int64  `json:"revision"`
	CauseKey  string `json:"cause_key"`
}

// NativeTerminalWakeTarget carries the exact native coordinates already
// authenticated by a Harness observer. It authorizes only a wake hint.
type NativeTerminalWakeTarget struct {
	SessionID  string
	SandboxID  string
	AgentRunID string
	ThreadID   string
	TurnID     string
}

type sessionExecutionWakeRevisionStore interface {
	SessionExecutionWakeRevision(context.Context, string) (int64, error)
}

type sessionExecutionWakeSignalStore interface {
	SignalSessionExecutionWake(context.Context, string, string, string) (int64, error)
}

type nativeTerminalWakeStore interface {
	SignalNativeTerminalWake(context.Context, string, NativeTerminalWakeTarget) (bool, error)
}

func MessageWakeEvent(sessionID string, sequence int64) string {
	return fmt.Sprintf("dorf.job-message:%s:%020d", sessionID, sequence)
}

func SessionExecutionWakeEvent(sessionID string, revision int64) string {
	return fmt.Sprintf("dorf.job-execution:v1:%s:%020d", sessionID, revision)
}

// ScheduleSessionTask reconciles one consumer-owned task with the Session's durable
// current attachment. The concrete consumer owns the task name and idempotency key.
func (a Application) ScheduleSessionTask(ctx context.Context, session Session, taskName, taskKey string) (Session, error) {
	if a.Tasks == nil {
		return Session{}, fmt.Errorf("Session task scheduling is not configured")
	}
	if err := a.Store.ScheduleSessionTask(ctx, a.Tasks.QueueName(), session.ID, taskName, taskKey); err != nil {
		return Session{}, err
	}
	return a.Store.Session(ctx, session.ID)
}

// EmitMessageWake emits a disposable wake hint for one durably accepted FIFO
// Message. Re-emission is safe because the event identity is deterministic.
func (a Application) EmitMessageWake(ctx context.Context, message Message) error {
	if err := a.Tasks.EmitEvent(ctx, a.Tasks.QueueName(), MessageWakeEvent(message.SessionID, message.Sequence), MessageWakeV1{SessionID: message.SessionID, Sequence: message.Sequence}); err != nil {
		return fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same send key and complete Message request: %w", message.ID, message.Sequence, err)
	}
	if _, err := a.signalSessionExecutionWake(ctx, message.SessionID, "message:"+message.ID); err != nil {
		return fmt.Errorf("message %s sequence %d was accepted and its FIFO wake emitted, but its execution wake hint failed; retry the same send key and complete Message request: %w", message.ID, message.Sequence, err)
	}
	return nil
}

func (a Application) SessionExecutionWakeRevision(ctx context.Context, sessionID string) (int64, error) {
	wakes, ok := a.Store.(sessionExecutionWakeRevisionStore)
	if !ok {
		return 0, fmt.Errorf("Session execution wake storage is not configured")
	}
	revision, err := wakes.SessionExecutionWakeRevision(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if revision < 0 || revision == math.MaxInt64 {
		return 0, fmt.Errorf("Session %s execution wake revision cannot advance", sessionID)
	}
	return revision, nil
}

func (a Application) signalSessionExecutionWake(ctx context.Context, sessionID, causeKey string) (int64, error) {
	wakes, ok := a.Store.(sessionExecutionWakeSignalStore)
	if !ok || a.Tasks == nil {
		return 0, fmt.Errorf("Session execution wake is not configured")
	}
	return wakes.SignalSessionExecutionWake(ctx, a.Tasks.QueueName(), sessionID, causeKey)
}

// SignalNativeTerminalWake turns one exact observer binding into a wake hint.
// PostgreSQL and the Harness remain authoritative for AgentRun settlement.
func (a Application) SignalNativeTerminalWake(ctx context.Context, target NativeTerminalWakeTarget) error {
	wakes, ok := a.Store.(nativeTerminalWakeStore)
	if !ok || a.Tasks == nil {
		return fmt.Errorf("native terminal wake is not configured")
	}
	_, err := wakes.SignalNativeTerminalWake(ctx, a.Tasks.QueueName(), target)
	return err
}

// AwaitSessionExecutionWake waits for one fresh per-Session revision. Timeout asks the
// consumer to reload authority without checkpointing a stale emitted event.
func (a Application) AwaitSessionExecutionWake(ctx context.Context, sessionID string, expectedRevision int64, stepName string, timeout time.Duration) error {
	wake, err := absurd.AwaitEvent[SessionExecutionWakeV1](ctx, SessionExecutionWakeEvent(sessionID, expectedRevision), absurd.AwaitEventOptions{StepName: stepName, Timeout: timeout})
	return resolveSessionExecutionWake(sessionID, expectedRevision, wake, err)
}

func resolveSessionExecutionWake(sessionID string, revision int64, wake SessionExecutionWakeV1, err error) error {
	if err != nil {
		var timeout *absurd.TimeoutError
		if errors.As(err, &timeout) {
			return nil
		}
		return err
	}
	if wake.SessionID != sessionID || wake.Revision != revision || wake.CauseKey == "" {
		return fmt.Errorf("execution wake payload conflicts with Session %s revision %d", sessionID, revision)
	}
	return nil
}

// AwaitMessageWake waits for one Session's exact next FIFO Message hint. A timeout
// asks the consumer to reload durable facts; only an event with the expected
// Session and sequence is accepted as a wake.
func (a Application) AwaitMessageWake(ctx context.Context, sessionID string, sequence int64, stepName string, timeout time.Duration) error {
	wake, err := absurd.AwaitEvent[MessageWakeV1](ctx, MessageWakeEvent(sessionID, sequence), absurd.AwaitEventOptions{StepName: stepName, Timeout: timeout})
	return resolveMessageWake(sessionID, sequence, wake, err)
}

func resolveMessageWake(sessionID string, sequence int64, wake MessageWakeV1, err error) error {
	if err != nil {
		var timeout *absurd.TimeoutError
		if errors.As(err, &timeout) {
			return nil
		}
		return err
	}
	if wake.SessionID != sessionID || wake.Sequence != sequence {
		return fmt.Errorf("message wake payload conflicts with Session %s sequence %d", sessionID, sequence)
	}
	return nil
}

// RetryReceipt reports only facts committed by Absurd. It is not a claim that
// a worker has resumed or completed the Session.
type RetryReceipt struct {
	RequestKey string `json:"request_key"`
	SessionID  string `json:"job_id"`
	TaskID     string `json:"task_id"`
	Retry      string `json:"retry"`
	RunID      string `json:"run_id"`
	Attempt    int    `json:"attempt"`
	Created    bool   `json:"created"`
}

type atomicSessionRetry interface {
	RetryFailedSession(context.Context, string, string, string) (RetryReceipt, error)
}

// RetryFailedSession schedules one additional bounded attempt on the Session's current
// attached execution task. The caller-retained request key and Absurd retry are
// committed atomically by the durable Store.
func (a Application) RetryFailedSession(ctx context.Context, sessionID, requestKey string) (RetryReceipt, error) {
	sessionID = strings.TrimSpace(sessionID)
	requestKey = strings.TrimSpace(requestKey)
	if sessionID == "" || requestKey == "" {
		return RetryReceipt{}, fmt.Errorf("retry requires one Session ID and caller-retained request key")
	}
	if len(requestKey) > 255 {
		return RetryReceipt{}, fmt.Errorf("retry request key must be at most 255 characters")
	}
	retries, ok := a.Store.(atomicSessionRetry)
	if !ok || a.Tasks == nil {
		return RetryReceipt{}, fmt.Errorf("atomic Session retry is not configured")
	}
	return retries.RetryFailedSession(ctx, a.Tasks.QueueName(), sessionID, requestKey)
}
