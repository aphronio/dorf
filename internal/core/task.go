package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// MessageWakeV1 is persisted by Absurd under one immutable Job-local FIFO event.
type MessageWakeV1 struct {
	JobID    string `json:"job_id"`
	Sequence int64  `json:"sequence"`
}

func MessageWakeEvent(jobID string, sequence int64) string {
	return fmt.Sprintf("dorf.job-message:%s:%020d", jobID, sequence)
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
		return fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same send key and text: %w", message.ID, message.Sequence, err)
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
