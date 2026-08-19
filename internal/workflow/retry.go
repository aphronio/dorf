package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

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
// attached execution task. The current task is simply the latest durable
// attachment, regardless of which workflow phase or task name produced it.
// Absurd retains task, run, checkpoint, and retry authority; Dorf does not
// copy or reinterpret that execution state.
func RetryFailedJob(ctx context.Context, store postgres.Store, client *absurd.Client, jobID string) (RetryReceipt, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return RetryReceipt{}, fmt.Errorf("retry requires one Job ID")
	}
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return RetryReceipt{}, err
	}
	taskID := job.CurrentTaskID
	if taskID == "" {
		return RetryReceipt{}, fmt.Errorf("Job %s has no attached execution task", job.ID)
	}

	// With no max-attempt override, Absurd atomically extends the same failed
	// task's existing bounded ceiling by exactly one attempt. It also owns the
	// atomic failed-state check, so Dorf does not pre-read or mirror task state.
	scheduled, err := client.RetryTask(ctx, client.QueueName(), taskID)
	if err != nil {
		return RetryReceipt{}, fmt.Errorf("retry Job %s attached task %s: %w", job.ID, taskID, err)
	}
	return RetryReceipt{
		JobID: job.ID, TaskID: scheduled.TaskID, Retry: "scheduled",
		RunID: scheduled.RunID, Attempt: scheduled.Attempt,
	}, nil
}
