package workflow

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	RunTaskName     = "dorf-job-messages-v2"
	CleanupTaskName = "dorf-job-cleanup-v2"
)

type Params struct {
	JobID string `json:"job_id"`
}
type Result struct {
	JobID   string `json:"job_id"`
	Outcome string `json:"outcome"`
}
type Wake struct {
	JobID string `json:"job_id"`
}

func WakeEvent(jobID string) string { return "dorf.job-message:" + jobID }

func Register(client *absurd.Client, service spine.Service) {
	client.MustRegister(absurd.Task(RunTaskName, func(ctx context.Context, params Params) (Result, error) {
		for wake := 1; ; wake++ {
			disposition, err := service.RunUntilIdle(ctx, params.JobID)
			if err != nil {
				return Result{}, err
			}
			if disposition == spine.RunClosed {
				return Result{JobID: params.JobID, Outcome: "admission-closed"}, nil
			}
			_, err = absurd.AwaitEvent[Wake](ctx, WakeEvent(params.JobID), absurd.AwaitEventOptions{StepName: fmt.Sprintf("message-wake-%06d", wake)})
			if err != nil {
				return Result{}, err
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
	client.MustRegister(absurd.Task(CleanupTaskName, func(ctx context.Context, params Params) (Result, error) {
		if err := service.Cleanup(ctx, params.JobID); err != nil {
			return Result{}, err
		}
		return Result{JobID: params.JobID, Outcome: "cleanup-complete"}, nil
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
}

func Admit(ctx context.Context, store postgres.Store, client *absurd.Client, input postgres.NewJob) (spine.Job, bool, error) {
	job, created, err := store.Admit(ctx, input)
	if err != nil {
		return spine.Job{}, false, err
	}
	spawned, err := client.Spawn(ctx, RunTaskName, Params{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: "run:" + job.ID})
	if err != nil {
		return spine.Job{}, false, fmt.Errorf("schedule admitted Job in Absurd: %w", err)
	}
	if err := store.SetTaskID(ctx, job.ID, spawned.TaskID); err != nil {
		return spine.Job{}, false, err
	}
	job, err = store.Job(ctx, job.ID)
	return job, created, err
}

func AdmitMessage(ctx context.Context, store postgres.Store, client *absurd.Client, input postgres.NewMessage) (spine.Message, bool, error) {
	message, created, err := store.AdmitMessage(ctx, input)
	if err != nil {
		return spine.Message{}, false, err
	}
	// Events carry no delivery truth. Re-emitting on an idempotent client retry
	// repairs a crash after PostgreSQL admission but before this wake hint.
	if err := client.EmitEvent(ctx, config.QueueName, WakeEvent(message.JobID), Wake{JobID: message.JobID}); err != nil {
		return message, created, fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same caller ID and input: %w", message.ID, message.Sequence, err)
	}
	return message, created, nil
}

func ScheduleCleanup(ctx context.Context, store postgres.Store, client *absurd.Client, jobID string) (spine.Job, error) {
	var result spine.Job
	err := store.WithJobFence(ctx, jobID, func() error {
		job, err := store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if job.CleanupState == spine.CleanupComplete {
			result = job
			return nil
		}
		blocker, err := store.NativeMutationBlocker(ctx, jobID)
		if err != nil {
			return err
		}
		if blocker != nil {
			reason := blocker.Attention
			if reason == "" {
				reason = string(blocker.State)
			}
			return fmt.Errorf("cleanup blocked by message sequence %d (%s): recover and inspect the native turn before deleting its Sandbox", blocker.Sequence, reason)
		}
		if err := store.CloseAdmission(ctx, jobID); err != nil {
			return err
		}
		if err := client.EmitEvent(ctx, config.QueueName, WakeEvent(jobID), Wake{JobID: jobID}); err != nil {
			return fmt.Errorf("wake delivery task for cleanup: %w", err)
		}
		if _, err := store.CancelRun(ctx, jobID); err != nil {
			return err
		}
		spawned, err := client.Spawn(ctx, CleanupTaskName, Params{JobID: jobID}, absurd.SpawnOptions{IdempotencyKey: "cleanup:" + jobID})
		if err != nil {
			return fmt.Errorf("schedule cleanup in Absurd: %w", err)
		}
		if err := store.SetCleanupTaskID(ctx, jobID, spawned.TaskID); err != nil {
			return err
		}
		result, err = store.Job(ctx, jobID)
		return err
	})
	return result, err
}
