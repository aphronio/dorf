package workflow

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	RunTaskName     = "dorf-job-spine-v1"
	CleanupTaskName = "dorf-job-cleanup-v1"
)

type Params struct {
	JobID string `json:"job_id"`
}
type Result struct {
	JobID   string `json:"job_id"`
	Outcome string `json:"outcome"`
}

func Register(client *absurd.Client, service spine.Service) {
	client.MustRegister(absurd.Task(RunTaskName, func(ctx context.Context, params Params) (Result, error) {
		return absurd.Step(ctx, "real-incus-codex-terminal", func(ctx context.Context) (Result, error) {
			if err := service.Run(ctx, params.JobID); err != nil {
				return Result{}, err
			}
			return Result{JobID: params.JobID, Outcome: "observed"}, nil
		})
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
	client.MustRegister(absurd.Task(CleanupTaskName, func(ctx context.Context, params Params) (Result, error) {
		return absurd.Step(ctx, "revoke-route-and-delete-sandbox", func(ctx context.Context) (Result, error) {
			if err := service.Cleanup(ctx, params.JobID); err != nil {
				return Result{}, err
			}
			return Result{JobID: params.JobID, Outcome: "cleanup-complete"}, nil
		})
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

func ScheduleCleanup(ctx context.Context, store postgres.Store, client *absurd.Client, jobID string) (spine.Job, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return spine.Job{}, err
	}
	if job.State != spine.JobObserved {
		return spine.Job{}, fmt.Errorf("Job %s has not recorded a native outcome; cleanup cannot race its durable run", jobID)
	}
	if job.CleanupState == spine.CleanupComplete {
		return job, nil
	}
	spawned, err := client.Spawn(ctx, CleanupTaskName, Params{JobID: jobID}, absurd.SpawnOptions{IdempotencyKey: "cleanup:" + jobID})
	if err != nil {
		return spine.Job{}, fmt.Errorf("schedule cleanup in Absurd: %w", err)
	}
	if err := store.SetCleanupTaskID(ctx, jobID, spawned.TaskID); err != nil {
		return spine.Job{}, err
	}
	return store.Job(ctx, jobID)
}
