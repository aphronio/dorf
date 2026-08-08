package workflow

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/publication"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	RunTaskName     = postgres.MessageTaskName
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
	JobID    string `json:"job_id"`
	Sequence int64  `json:"sequence"`
}

func WakeEvent(jobID string, sequence int64) string {
	return fmt.Sprintf("dorf.job-message:%s:%020d", jobID, sequence)
}

func Register(client *absurd.Client, service spine.Service, store postgres.Store) {
	client.MustRegister(absurd.Task(RunTaskName, func(ctx context.Context, params Params) (Result, error) {
		// Sequence 1 is present before this task is spawned. Every later FIFO
		// position owns one immutable Absurd event identity, starting at 2.
		for {
			disposition, err := service.RunUntilIdle(ctx, params.JobID)
			if err != nil {
				return Result{}, err
			}
			if disposition == spine.RunClosed {
				return Result{JobID: params.JobID, Outcome: "admission-closed"}, nil
			}
			job, err := store.Job(ctx, params.JobID)
			if err != nil {
				return Result{}, err
			}
			if err := continuePublication(ctx, store, client, service.Barrier, job); err != nil {
				return Result{}, err
			}
			sequence, err := store.NextWakeSequence(ctx, params.JobID)
			if err != nil {
				return Result{}, err
			}
			_, err = absurd.AwaitEvent[Wake](ctx, WakeEvent(params.JobID, sequence), absurd.AwaitEventOptions{StepName: fmt.Sprintf("message-wake-%020d", sequence)})
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

func continuePublication(ctx context.Context, store postgres.Store, client *absurd.Client, barrier any, job spine.Job) error {
	needsSchedule := job.WorkflowPhase == "ready" ||
		(job.WorkflowPhase == "publishing" || job.WorkflowPhase == "published") && job.PublicationTaskID == ""
	if !needsSchedule {
		return nil
	}
	if _, _, _, err := publication.Schedule(ctx, store, client, barrier, job.ID, job.Revision); err != nil {
		return fmt.Errorf("continue exact-Revision publication: %w", err)
	}
	return nil
}

func Admit(ctx context.Context, store postgres.Store, client *absurd.Client, input postgres.NewJob) (spine.Job, bool, error) {
	job, created, err := store.Admit(ctx, input)
	if err != nil {
		return spine.Job{}, false, err
	}
	if !job.AdmissionOpen {
		return job, created, nil
	}
	if err := store.CheckMessageTaskAttachment(ctx, job.ID); err != nil {
		return spine.Job{}, false, fmt.Errorf("validate Job run task before scheduling: %w", err)
	}
	spawned, err := client.Spawn(ctx, RunTaskName, Params{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: postgres.MessageTaskKey(job.ID)})
	if err != nil {
		return spine.Job{}, false, fmt.Errorf("schedule admitted Job in Absurd: %w", err)
	}
	if err := store.AttachMessageTask(ctx, job.ID, spawned.TaskID); err != nil {
		if spawned.Created {
			if cancelErr := client.CancelTask(ctx, config.QueueName, spawned.TaskID); cancelErr != nil {
				return spine.Job{}, false, fmt.Errorf("attach Job message task: %w; cancel unattached task %s: %v", err, spawned.TaskID, cancelErr)
			}
		}
		return spine.Job{}, false, fmt.Errorf("attach Job message task: %w", err)
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
	if err := client.EmitEvent(ctx, config.QueueName, WakeEvent(message.JobID, message.Sequence), Wake{JobID: message.JobID, Sequence: message.Sequence}); err != nil {
		return message, created, fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same caller ID and input: %w", message.ID, message.Sequence, err)
	}
	return message, created, nil
}

// RetrySetup atomically records a new setup Action generation and its FIFO
// wake, then emits the recoverable Absurd event hint.
func RetrySetup(ctx context.Context, store postgres.Store, client *absurd.Client, jobID, retryID, input string) (spine.Action, spine.Message, bool, error) {
	action, message, created, err := store.RetrySetup(ctx, jobID, retryID, input)
	if err != nil {
		return action, message, created, err
	}
	if action.State == spine.ActionFailed {
		return action, message, false, nil
	}
	// Events carry no delivery truth. Re-emission is the recovery path for a
	// crash after the Action/message transaction and before this wake hint.
	if err := client.EmitEvent(ctx, config.QueueName, WakeEvent(message.JobID, message.Sequence), Wake{JobID: message.JobID, Sequence: message.Sequence}); err != nil {
		return action, message, created, fmt.Errorf("setup retry message %s sequence %d was accepted, but its wake hint failed; retry the same setup identity and input: %w", message.ID, message.Sequence, err)
	}
	return action, message, created, nil
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
		if job.WorkflowPhase == "publishing" || job.WorkflowPhase == "publication-blocked" || job.WorkflowPhase == "published" {
			proposal, err := store.Proposal(ctx, job.ID)
			if err != nil {
				return err
			}
			if proposal != nil {
				outcome, err := store.Outcome(ctx, job.ID)
				if err != nil {
					return err
				}
				if outcome == nil {
					return fmt.Errorf("cleanup cannot cancel or remove a stored GitHub proposal without a recorded accepted, rejected, or explicitly abandoned outcome")
				}
			}
		}
		if err := store.CloseAdmission(ctx, jobID); err != nil {
			return err
		}
		if _, err := store.CancelRun(ctx, jobID); err != nil {
			return err
		}
		if _, err := store.CancelPublication(ctx, jobID); err != nil {
			return fmt.Errorf("settle attached publication task: %w", err)
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
