package publication

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const TaskName = postgres.PublicationTaskName

type Params struct {
	JobID    string `json:"job_id"`
	Revision string `json:"revision"`
	Attempt  int    `json:"attempt"`
}

type Result struct {
	JobID    string `json:"job_id"`
	Revision string `json:"revision"`
	Outcome  string `json:"outcome"`
}

func Register(client *absurd.Client, service Service) {
	client.MustRegister(absurd.Task(TaskName, func(ctx context.Context, params Params) (Result, error) {
		if err := service.Publish(ctx, params.JobID, params.Revision, params.Attempt); err != nil {
			return Result{}, err
		}
		job, err := service.Store.Job(ctx, params.JobID)
		if err != nil {
			return Result{}, err
		}
		return Result{JobID: job.ID, Revision: params.Revision, Outcome: job.WorkflowPhase}, nil
	}, absurd.TaskOptions{DefaultMaxAttempts: postgres.PublicationTaskMaxAttempts}))
}

func Schedule(ctx context.Context, store postgres.Store, client *absurd.Client, barrier any, jobID, revision string) (Params, string, bool, error) {
	var params Params
	var taskID string
	var created bool
	err := store.WithJobFence(ctx, jobID, func() error {
		job, _, _, spawn, err := store.BeginPublication(ctx, jobID, revision)
		if err != nil {
			return err
		}
		params = Params{JobID: job.ID, Revision: job.Revision, Attempt: job.PublicationAttempt}
		if job.WorkflowPhase == "published" || !spawn {
			taskID = job.PublicationTaskID
			return nil
		}
		key := postgres.PublicationTaskKey(job.ID, job.Revision, job.PublicationAttempt)
		if err := reachScheduleBarrier(ctx, barrier, spine.BarrierPublicationBegin, job.ID, key); err != nil {
			return err
		}
		spawned, err := client.Spawn(ctx, TaskName, params, absurd.SpawnOptions{IdempotencyKey: key})
		if err != nil {
			return fmt.Errorf("schedule exact-Revision publication: %w", err)
		}
		taskID, created = spawned.TaskID, spawned.Created
		if err := reachScheduleBarrier(ctx, barrier, spine.BarrierPublicationSpawn, job.ID, spawned.TaskID); err != nil {
			return err
		}
		if err := store.AttachPublicationTask(ctx, job.ID, job.Revision, job.PublicationAttempt, spawned.TaskID); err != nil {
			if spawned.Created {
				if cancelErr := client.CancelTask(ctx, config.QueueName, spawned.TaskID); cancelErr != nil {
					return fmt.Errorf("attach exact-Revision publication task: %w; cancel unattached task %s: %v", err, spawned.TaskID, cancelErr)
				}
			}
			return fmt.Errorf("attach exact-Revision publication task: %w", err)
		}
		return nil
	})
	if err != nil {
		return params, taskID, created, err
	}
	return params, taskID, created, nil
}

func reachScheduleBarrier(ctx context.Context, barrier any, point, jobID, identity string) error {
	reacher, ok := barrier.(interface {
		ReachWorkflow(context.Context, string, string, string) error
	})
	if !ok {
		return nil
	}
	return reacher.ReachWorkflow(ctx, point, jobID, identity)
}
