package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
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
	err := a.Store.WithJobFence(ctx, job.ID, func() error {
		current, err := a.Store.Job(ctx, job.ID)
		if err != nil {
			return err
		}
		if !current.AdmissionOpen || current.CleanupState != CleanupPending {
			return fmt.Errorf("Job %s cannot schedule ordinary work after cleanup begins", job.ID)
		}
		spawned, err := a.Tasks.Spawn(ctx, taskName, JobTaskParams{JobID: job.ID, PreviousTaskID: current.CurrentTaskID}, absurdruntime.TaskSpawnOptions(a.Tasks.QueueName(), taskKey))
		if err != nil {
			return fmt.Errorf("schedule admitted Job in Absurd: %w", err)
		}
		if err := a.Store.AttachJobTask(ctx, job.ID, current.CurrentTaskID, spawned.TaskID, taskName); err != nil {
			return fmt.Errorf("attach Job task: %w", err)
		}
		return nil
	})
	if err != nil {
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
	JobID   string `json:"job_id"`
	TaskID  string `json:"task_id"`
	Retry   string `json:"retry"`
	RunID   string `json:"run_id"`
	Attempt int    `json:"attempt"`
}

// RetryFailedJob schedules one additional bounded attempt on the Job's current
// attached execution task. Absurd remains the task, checkpoint, and retry authority.
func (a Application) RetryFailedJob(ctx context.Context, jobID string) (RetryReceipt, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return RetryReceipt{}, fmt.Errorf("retry requires one Job ID")
	}
	job, err := a.Store.Job(ctx, jobID)
	if err != nil {
		return RetryReceipt{}, err
	}
	if job.CurrentTaskID == "" {
		return RetryReceipt{}, fmt.Errorf("Job %s has no attached execution task", job.ID)
	}
	scheduled, err := a.Tasks.RetryTask(ctx, a.Tasks.QueueName(), job.CurrentTaskID)
	if err != nil {
		return RetryReceipt{}, fmt.Errorf("retry Job %s attached task %s: %w", job.ID, job.CurrentTaskID, err)
	}
	return RetryReceipt{JobID: job.ID, TaskID: scheduled.TaskID, Retry: "scheduled", RunID: scheduled.RunID, Attempt: scheduled.Attempt}, nil
}
