// Package controlplane exposes the in-process Dorf Core application boundary.
// Native workflows and transport adapters consume this boundary; provider,
// scheduler, and persistence authorities remain behind it.
package controlplane

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const CleanupTaskName = "dorf-job-cleanup-v3"

type JobTaskParams struct {
	JobID string `json:"job_id"`
}

type TaskResultV1 struct {
	JobID   string `json:"job_id"`
	Outcome string `json:"outcome"`
}

type Application struct {
	Store postgres.Store
	Tasks *absurd.Client
}

// RequestCleanup closes further admission, settles the currently attached
// task, and durably hands the Job to Core cleanup. Calling it again converges
// on the same cleanup task or completed receipt.
func (a Application) RequestCleanup(ctx context.Context, jobID string) (spine.Job, error) {
	job, err := a.Store.Job(ctx, jobID)
	if err != nil {
		return spine.Job{}, err
	}
	if job.CleanupState == spine.CleanupComplete {
		return job, nil
	}
	if err := a.Store.CloseAdmissionForCleanup(ctx, jobID); err != nil {
		return spine.Job{}, err
	}
	job, err = a.Store.Job(ctx, jobID)
	if err != nil {
		return spine.Job{}, err
	}
	skipTaskID := currentTaskID(ctx)
	if err := a.cancelAttachedTask(ctx, job, skipTaskID); err != nil {
		return spine.Job{}, err
	}

	var result spine.Job
	err = a.Store.WithJobFence(ctx, jobID, func() error {
		current, err := a.Store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if err := a.cancelAttachedTask(ctx, current, skipTaskID); err != nil {
			return err
		}
		spawned, err := a.Tasks.Spawn(ctx, CleanupTaskName, JobTaskParams{JobID: jobID}, absurdruntime.TaskSpawnOptions("cleanup:v3:"+jobID))
		if err != nil {
			return fmt.Errorf("schedule cleanup in Absurd: %w", err)
		}
		if err := a.Store.AttachCleanupTask(ctx, jobID, current.CurrentTaskID, spawned.TaskID, CleanupTaskName); err != nil {
			return err
		}
		result, err = a.Store.Job(ctx, jobID)
		return err
	})
	return result, err
}

func (a Application) cancelAttachedTask(ctx context.Context, job spine.Job, skipTaskID string) error {
	taskID := job.CurrentTaskID
	if taskID == "" || taskID == skipTaskID {
		return nil
	}
	if err := a.Tasks.CancelTask(ctx, a.Tasks.QueueName(), taskID); err != nil {
		return fmt.Errorf("cancel attached Absurd task %s: %w", taskID, err)
	}
	snapshot, err := a.Tasks.FetchTaskResult(ctx, a.Tasks.QueueName(), taskID)
	if err != nil {
		return err
	}
	if snapshot == nil || !snapshot.IsTerminal() {
		return fmt.Errorf("attached Absurd task %s did not reach a public terminal result", taskID)
	}
	return nil
}

func currentTaskID(ctx context.Context) string {
	task, ok := absurd.TaskFromContext(ctx)
	if !ok {
		return ""
	}
	return task.TaskID()
}
