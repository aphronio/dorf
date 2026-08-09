package publication

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const TaskName = postgres.PublicationTaskName

type Params struct {
	JobID    string `json:"job_id"`
	Revision string `json:"revision"`
}

type Result struct {
	JobID    string `json:"job_id"`
	Revision string `json:"revision"`
	Outcome  string `json:"outcome"`
}

func Register(client *absurd.Client, service Service) {
	client.MustRegister(absurd.Task(TaskName, func(ctx context.Context, params Params) (Result, error) {
		if err := service.Store.WithJobFence(ctx, params.JobID, func() error {
			job, err := service.Store.Job(ctx, params.JobID)
			if err != nil {
				return err
			}
			attachedID := job.PublicationTaskID
			if attachedID == "" {
				task, ok := absurd.TaskFromContext(ctx)
				if !ok {
					return absurd.ErrNoTaskContext
				}
				if err := service.Store.AttachPublicationTask(ctx, params.JobID, params.Revision, task.TaskID()); err != nil {
					return fmt.Errorf("recover public publication Spawn attachment: %w", err)
				}
				attachedID = task.TaskID()
			}
			return verifyTaskContext(ctx, attachedID, TaskName)
		}); err != nil {
			return Result{}, err
		}
		// The checkpoint is a replay cursor, not proof that Git or GitHub
		// mutated exactly once. Stable Dorf Actions remain the authority for
		// reconciling the effect/checkpoint commit gap.
		result, err := absurd.Step(ctx, "dorf/publication/v1", func(stepCtx context.Context) (Result, error) {
			return absurdruntime.WithHeartbeat(stepCtx, func(workCtx context.Context) (Result, error) {
				if err := service.Publish(workCtx, params.JobID, params.Revision); err != nil {
					return Result{}, err
				}
				current, err := service.Store.Job(workCtx, params.JobID)
				if err != nil {
					return Result{}, err
				}
				return Result{JobID: current.ID, Revision: params.Revision, Outcome: current.WorkflowPhase}, nil
			})
		})
		if err != nil {
			return Result{}, err
		}
		return result, nil
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
		params = Params{JobID: job.ID, Revision: job.Revision}
		if !spawn {
			taskID = job.PublicationTaskID
			return nil
		}
		key := postgres.PublicationTaskKey(job.ID, job.Revision)
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
		if err := store.AttachPublicationTask(ctx, job.ID, job.Revision, spawned.TaskID); err != nil {
			return fmt.Errorf("attach exact-Revision publication task: %w", err)
		}
		return nil
	})
	if err != nil {
		return params, taskID, created, err
	}
	return params, taskID, created, nil
}

func Retry(ctx context.Context, store postgres.Store, client *absurd.Client, jobID, revision string) (Params, absurd.SpawnResult, error) {
	params := Params{JobID: jobID, Revision: revision}
	var retried absurd.SpawnResult
	err := store.WithJobFence(ctx, jobID, func() error {
		job, err := store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if job.Revision != revision || job.PublicationTaskID == "" {
			return fmt.Errorf("publication retry is not attached to exact Revision %s", revision)
		}
		snapshot, err := client.FetchTaskResult(ctx, client.QueueName(), job.PublicationTaskID)
		if err != nil {
			return err
		}
		if snapshot == nil || snapshot.State != absurd.TaskFailed {
			state := absurd.TaskResultState("missing")
			if snapshot != nil {
				state = snapshot.State
			}
			return fmt.Errorf("publication task %s is %s, not failed", job.PublicationTaskID, state)
		}
		// Make the product projection runnable before Absurd exposes the new
		// run to a worker. A crash here leaves one failed attached task and is
		// repaired by repeating this public retry command.
		if err := store.ResumePublication(ctx, job.ID, revision); err != nil {
			return err
		}
		retried, err = client.RetryTask(ctx, client.QueueName(), job.PublicationTaskID)
		return err
	})
	return params, retried, err
}

func verifyTaskContext(ctx context.Context, attachedID, taskName string) error {
	task, ok := absurd.TaskFromContext(ctx)
	if !ok {
		return absurd.ErrNoTaskContext
	}
	if attachedID == "" {
		return fmt.Errorf("%s task %s ran before its public Spawn result was attached", taskName, task.TaskID())
	}
	if task.TaskID() != attachedID || task.TaskName() != taskName {
		return fmt.Errorf("%s task context %s conflicts with attached task %s", taskName, task.TaskID(), attachedID)
	}
	return nil
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
