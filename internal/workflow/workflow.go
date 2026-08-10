package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	RunTaskName     = postgres.MessageTaskName
	CleanupTaskName = "dorf-job-cleanup-v3"
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

func Register(client *absurd.Client, service spine.Service, store postgres.Store, proposal ProposalRuntime) {
	service.ClaimCheck = absurdruntime.RequireClaim
	proposal.Publication.ClaimCheck = absurdruntime.RequireClaim
	proposal.Outcome.ClaimCheck = absurdruntime.RequireClaim
	if proposal.PollInterval <= 0 {
		proposal.PollInterval = 30 * time.Second
	}
	client.MustRegister(absurd.Task(RunTaskName, func(ctx context.Context, params Params) (Result, error) {
		if err := verifyAttachedTask(ctx, store, params.JobID, RunTaskName); err != nil {
			return Result{}, err
		}
		// Sequence 1 is present before this task is spawned. Every later FIFO
		// position owns one immutable Absurd event identity, starting at 2.
		for {
			disposition, err := RunJob(ctx, service, store, proposal, params.JobID)
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
			if job.WorkflowPhase == "published" {
				observation, err := observeProposal(ctx, proposal, job.ID, job.Revision)
				if err != nil {
					return Result{}, err
				}
				if observation.Outcome != "" {
					task := absurd.MustTaskContext(ctx)
					if _, err := scheduleCleanup(ctx, store, client, params.JobID, task.TaskID()); err != nil {
						return Result{}, err
					}
					return Result{JobID: params.JobID, Outcome: string(observation.Outcome)}, nil
				}
				if observation.NewMessages > 0 {
					continue
				}
			}
			sequence, err := store.NextWakeSequence(ctx, params.JobID)
			if err != nil {
				return Result{}, err
			}
			options := absurd.AwaitEventOptions{StepName: fmt.Sprintf("dorf/message-wake/v1/%020d", sequence)}
			if job.WorkflowPhase == "published" {
				options.StepName = fmt.Sprintf("dorf/proposal-wake/v2/%s/%020d", job.Revision, sequence)
				options.Timeout = proposal.PollInterval
			}
			wake, err := absurd.AwaitEvent[Wake](ctx, WakeEvent(params.JobID, sequence), options)
			if err != nil {
				var timeout *absurd.TimeoutError
				if job.WorkflowPhase == "published" && errors.As(err, &timeout) {
					continue
				}
				return Result{}, err
			}
			if wake.JobID != params.JobID || wake.Sequence != sequence {
				return Result{}, fmt.Errorf("message wake payload conflicts with Job %s sequence %d", params.JobID, sequence)
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
	client.MustRegister(absurd.Task(CleanupTaskName, func(ctx context.Context, params Params) (Result, error) {
		if err := verifyAttachedTask(ctx, store, params.JobID, CleanupTaskName); err != nil {
			return Result{}, err
		}
		return absurd.Step(ctx, "dorf/cleanup/v1", func(stepCtx context.Context) (Result, error) {
			return absurdruntime.WithHeartbeat(stepCtx, func(workCtx context.Context) (Result, error) {
				if err := service.Cleanup(workCtx, params.JobID); err != nil {
					return Result{}, err
				}
				return Result{JobID: params.JobID, Outcome: "cleanup-complete"}, nil
			})
		})
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
}

func observeProposal(ctx context.Context, proposal ProposalRuntime, jobID, revision string) (ProposalObservationResultV1, error) {
	result, err := absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (ProposalObservationResultV1, error) {
		return proposal.Observe(workCtx, jobID, revision)
	})
	if err != nil {
		return ProposalObservationResultV1{}, err
	}
	if result.Revision != revision {
		return ProposalObservationResultV1{}, fmt.Errorf("proposal observation conflicts with Revision %s", revision)
	}
	return result, nil
}

func Admit(ctx context.Context, store postgres.Store, client *absurd.Client, input postgres.NewJob) (spine.Job, bool, error) {
	job, created, err := store.Admit(ctx, input)
	if err != nil {
		return spine.Job{}, false, err
	}
	if !job.AdmissionOpen {
		return job, created, nil
	}
	err = store.WithJobFence(ctx, job.ID, func() error {
		spawned, err := client.Spawn(ctx, RunTaskName, Params{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: postgres.MessageTaskKey(job.ID)})
		if err != nil {
			return fmt.Errorf("schedule admitted Job in Absurd: %w", err)
		}
		if err := store.AttachMessageTask(ctx, job.ID, spawned.TaskID); err != nil {
			return fmt.Errorf("attach Job message task: %w", err)
		}
		return nil
	})
	if err != nil {
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
	if err := client.EmitEvent(ctx, config.QueueName, WakeEvent(message.JobID, message.Sequence), Wake{JobID: message.JobID, Sequence: message.Sequence}); err != nil {
		return message, created, fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same from ID and input: %w", message.ID, message.Sequence, err)
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
	return scheduleCleanup(ctx, store, client, jobID, "")
}

func scheduleCleanup(ctx context.Context, store postgres.Store, client *absurd.Client, jobID, skipTaskID string) (spine.Job, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return spine.Job{}, err
	}
	if job.CleanupState == spine.CleanupComplete {
		return job, nil
	}
	if err := cleanupPublicationSafe(ctx, store, job); err != nil {
		return spine.Job{}, err
	}
	// Close admission before taking the long Job effect fence, then cancel
	// through Absurd's public API. A running handler observes cancellation at
	// its heartbeat and cancels the opaque child context; the Job fence still
	// prevents cleanup from overtaking any late external effect.
	if err := store.CloseAdmission(ctx, jobID); err != nil {
		return spine.Job{}, err
	}
	// Reload after admission closes so cancellation uses the current durable
	// task binding rather than the earlier snapshot.
	job, err = store.Job(ctx, jobID)
	if err != nil {
		return spine.Job{}, err
	}
	if err := cancelAttachedTasks(ctx, client, job, skipTaskID); err != nil {
		return spine.Job{}, err
	}
	var result spine.Job
	err = store.WithJobFence(ctx, jobID, func() error {
		current, err := store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if err := cleanupPublicationSafe(ctx, store, current); err != nil {
			return err
		}
		// Recheck the binding under the Job fence before cleanup becomes eligible.
		if err := cancelAttachedTasks(ctx, client, current, skipTaskID); err != nil {
			return err
		}
		spawned, err := client.Spawn(ctx, CleanupTaskName, Params{JobID: jobID}, absurd.SpawnOptions{IdempotencyKey: "cleanup:v3:" + jobID})
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

func cancelAttachedTasks(ctx context.Context, client *absurd.Client, job spine.Job, skipTaskID string) error {
	for _, taskID := range []string{job.TaskID} {
		if taskID == "" || taskID == skipTaskID {
			continue
		}
		if err := client.CancelTask(ctx, client.QueueName(), taskID); err != nil {
			return fmt.Errorf("cancel attached Absurd task %s: %w", taskID, err)
		}
		snapshot, err := client.FetchTaskResult(ctx, client.QueueName(), taskID)
		if err != nil {
			return err
		}
		if snapshot == nil || !snapshot.IsTerminal() {
			return fmt.Errorf("attached Absurd task %s did not reach a public terminal result", taskID)
		}
	}
	return nil
}

func cleanupPublicationSafe(ctx context.Context, store postgres.Store, job spine.Job) error {
	if job.WorkflowPhase != "publishing" && job.WorkflowPhase != "publication-blocked" && job.WorkflowPhase != "published" {
		return nil
	}
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
		return nil
	}
	_, pull, err := store.PublicationActions(ctx, job.ID, job.Revision)
	if err != nil {
		return fmt.Errorf("cleanup requires publication reconciliation for the exact pull-request Action: %w", err)
	}
	if pull.State != spine.ActionPending {
		return fmt.Errorf("cleanup cannot proceed while the exact pull-request Action is %s; retry publication to reconcile and record any exposed proposal first", pull.State)
	}
	return nil
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

func verifyAttachedTask(ctx context.Context, store postgres.Store, jobID, taskName string) error {
	return store.WithJobFence(ctx, jobID, func() error {
		job, err := store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		attachedID := job.TaskID
		if taskName == CleanupTaskName {
			attachedID = job.CleanupTaskID
		}
		if attachedID == "" {
			task, ok := absurd.TaskFromContext(ctx)
			if !ok {
				return absurd.ErrNoTaskContext
			}
			if taskName == CleanupTaskName {
				err = store.SetCleanupTaskID(ctx, jobID, task.TaskID())
			} else {
				err = store.AttachMessageTask(ctx, jobID, task.TaskID())
			}
			if err != nil {
				return fmt.Errorf("recover public Spawn attachment for %s: %w", taskName, err)
			}
			attachedID = task.TaskID()
		}
		return verifyTaskContext(ctx, attachedID, taskName)
	})
}
