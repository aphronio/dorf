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

// MessageWakeV1 is persisted by Absurd under one immutable Job-local FIFO event.
type MessageWakeV1 struct {
	JobID    string `json:"job_id"`
	Sequence int64  `json:"sequence"`
}

// JobExecutionWakeV1 is a disposable hint that asks a Job's current task to
// reload authoritative workflow and Harness state.
type JobExecutionWakeV1 struct {
	JobID    string `json:"job_id"`
	Revision int64  `json:"revision"`
	CauseKey string `json:"cause_key"`
}

// NativeTerminalWakeTarget carries the exact native coordinates already
// authenticated by a Harness observer. It authorizes only a wake hint.
type NativeTerminalWakeTarget struct {
	JobID      string
	SandboxID  string
	AgentRunID string
	ThreadID   string
	TurnID     string
}

type jobExecutionWakeRevisionStore interface {
	JobExecutionWakeRevision(context.Context, string) (int64, error)
}

type jobExecutionWakeSignalStore interface {
	SignalJobExecutionWake(context.Context, string, string, string) (int64, error)
}

type nativeTerminalWakeStore interface {
	SignalNativeTerminalWake(context.Context, string, NativeTerminalWakeTarget) (bool, error)
}

func MessageWakeEvent(jobID string, sequence int64) string {
	return fmt.Sprintf("dorf.job-message:%s:%020d", jobID, sequence)
}

func JobExecutionWakeEvent(jobID string, revision int64) string {
	return fmt.Sprintf("dorf.job-execution:v1:%s:%020d", jobID, revision)
}

// ScheduleJobTask reconciles one consumer-owned task with the Job's durable
// current attachment. The concrete consumer owns the task name and idempotency key.
func (a Application) ScheduleJobTask(ctx context.Context, job Job, taskName, taskKey string) (Job, error) {
	if a.Tasks == nil {
		return Job{}, fmt.Errorf("Job task scheduling is not configured")
	}
	if err := a.Store.ScheduleJobTask(ctx, a.Tasks.QueueName(), job.ID, taskName, taskKey); err != nil {
		return Job{}, err
	}
	return a.Store.Job(ctx, job.ID)
}

// EmitMessageWake emits a disposable wake hint for one durably accepted FIFO
// Message. Re-emission is safe because the event identity is deterministic.
func (a Application) EmitMessageWake(ctx context.Context, message Message) error {
	if err := a.Tasks.EmitEvent(ctx, a.Tasks.QueueName(), MessageWakeEvent(message.JobID, message.Sequence), MessageWakeV1{JobID: message.JobID, Sequence: message.Sequence}); err != nil {
		return fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same send key and complete Message request: %w", message.ID, message.Sequence, err)
	}
	if _, err := a.signalJobExecutionWake(ctx, message.JobID, "message:"+message.ID); err != nil {
		return fmt.Errorf("message %s sequence %d was accepted and its FIFO wake emitted, but its execution wake hint failed; retry the same send key and complete Message request: %w", message.ID, message.Sequence, err)
	}
	return nil
}

func (a Application) JobExecutionWakeRevision(ctx context.Context, jobID string) (int64, error) {
	wakes, ok := a.Store.(jobExecutionWakeRevisionStore)
	if !ok {
		return 0, fmt.Errorf("Job execution wake storage is not configured")
	}
	revision, err := wakes.JobExecutionWakeRevision(ctx, jobID)
	if err != nil {
		return 0, err
	}
	if revision < 0 || revision == math.MaxInt64 {
		return 0, fmt.Errorf("Job %s execution wake revision cannot advance", jobID)
	}
	return revision, nil
}

func (a Application) signalJobExecutionWake(ctx context.Context, jobID, causeKey string) (int64, error) {
	wakes, ok := a.Store.(jobExecutionWakeSignalStore)
	if !ok || a.Tasks == nil {
		return 0, fmt.Errorf("Job execution wake is not configured")
	}
	return wakes.SignalJobExecutionWake(ctx, a.Tasks.QueueName(), jobID, causeKey)
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

// AwaitJobExecutionWake waits for one fresh per-Job revision. Timeout asks the
// consumer to reload authority without checkpointing a stale emitted event.
func (a Application) AwaitJobExecutionWake(ctx context.Context, jobID string, expectedRevision int64, stepName string, timeout time.Duration) error {
	wake, err := absurd.AwaitEvent[JobExecutionWakeV1](ctx, JobExecutionWakeEvent(jobID, expectedRevision), absurd.AwaitEventOptions{StepName: stepName, Timeout: timeout})
	return resolveJobExecutionWake(jobID, expectedRevision, wake, err)
}

func resolveJobExecutionWake(jobID string, revision int64, wake JobExecutionWakeV1, err error) error {
	if err != nil {
		var timeout *absurd.TimeoutError
		if errors.As(err, &timeout) {
			return nil
		}
		return err
	}
	if wake.JobID != jobID || wake.Revision != revision || wake.CauseKey == "" {
		return fmt.Errorf("execution wake payload conflicts with Job %s revision %d", jobID, revision)
	}
	return nil
}

// AwaitMessageWake waits for one Job's exact next FIFO Message hint. A timeout
// asks the consumer to reload durable facts; only an event with the expected
// Job and sequence is accepted as a wake.
func (a Application) AwaitMessageWake(ctx context.Context, jobID string, sequence int64, stepName string, timeout time.Duration) error {
	wake, err := absurd.AwaitEvent[MessageWakeV1](ctx, MessageWakeEvent(jobID, sequence), absurd.AwaitEventOptions{StepName: stepName, Timeout: timeout})
	return resolveMessageWake(jobID, sequence, wake, err)
}

func resolveMessageWake(jobID string, sequence int64, wake MessageWakeV1, err error) error {
	if err != nil {
		var timeout *absurd.TimeoutError
		if errors.As(err, &timeout) {
			return nil
		}
		return err
	}
	if wake.JobID != jobID || wake.Sequence != sequence {
		return fmt.Errorf("message wake payload conflicts with Job %s sequence %d", jobID, sequence)
	}
	return nil
}

// RetryReceipt reports only facts committed by Absurd. It is not a claim that
// a worker has resumed or completed the Job.
type RetryReceipt struct {
	RequestKey string `json:"request_key"`
	JobID      string `json:"job_id"`
	TaskID     string `json:"task_id"`
	Retry      string `json:"retry"`
	RunID      string `json:"run_id"`
	Attempt    int    `json:"attempt"`
	Created    bool   `json:"created"`
}

type atomicJobRetry interface {
	RetryFailedJob(context.Context, string, string, string) (RetryReceipt, error)
}

// RetryFailedJob schedules one additional bounded attempt on the Job's current
// attached execution task. The caller-retained request key and Absurd retry are
// committed atomically by the durable Store.
func (a Application) RetryFailedJob(ctx context.Context, jobID, requestKey string) (RetryReceipt, error) {
	jobID = strings.TrimSpace(jobID)
	requestKey = strings.TrimSpace(requestKey)
	if jobID == "" || requestKey == "" {
		return RetryReceipt{}, fmt.Errorf("retry requires one Job ID and caller-retained request key")
	}
	if len(requestKey) > 255 {
		return RetryReceipt{}, fmt.Errorf("retry request key must be at most 255 characters")
	}
	retries, ok := a.Store.(atomicJobRetry)
	if !ok || a.Tasks == nil {
		return RetryReceipt{}, fmt.Errorf("atomic Job retry is not configured")
	}
	return retries.RetryFailedJob(ctx, a.Tasks.QueueName(), jobID, requestKey)
}
