package core

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/sandbox"
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

// CleanupRuntime is the provider-neutral capability resolved from one Job's
// durably pinned Sandbox profile after cleanup has been requested.
type CleanupRuntime struct {
	Execution      CleanupExecution
	SandboxProfile string
}

type CleanupRuntimeResolver interface {
	ResolveCleanup(context.Context, string) (CleanupRuntime, error)
}

// ApplicationStore is the durable Core custody required by the application boundary.
// PostgreSQL is the current implementation, not part of the consumer contract.
type ApplicationStore interface {
	Job(context.Context, string) (Job, error)
	JobTasks(context.Context, string) ([]JobTask, error)
	WithJobFence(context.Context, string, func() error) error
	AttachJobTask(context.Context, string, string, string, string) error
	CloseAdmissionForCleanup(context.Context, string) error
	AttachCleanupTask(context.Context, string, string, string, string) error
	GetOrCreateSandboxAction(context.Context, string, ActionKind) (Action, error)
	RecordSandboxProfileUnavailable(context.Context, string, string, string, error) error
	SetCleanupAttention(context.Context, string, string) error
	CompleteCleanup(context.Context, string) error
}

// StopForUnavailableSandboxProfile turns one definitive provider artifact
// failure into durable attention instead of asking Absurd to retry an input
// that cannot succeed. The workflow supplies the exact current fact identity.
func (a Application) StopForUnavailableSandboxProfile(ctx context.Context, jobID, source string, cause error) (TaskResultV1, bool, error) {
	if !sandbox.IsArtifactUnavailable(cause) {
		return TaskResultV1{}, false, nil
	}
	job, err := a.Store.Job(ctx, jobID)
	if err != nil {
		return TaskResultV1{}, true, err
	}
	if err := a.Store.RecordSandboxProfileUnavailable(ctx, job.ID, job.SandboxProfile, source, cause); err != nil {
		return TaskResultV1{}, true, fmt.Errorf("record unavailable Sandbox profile %q: %w", job.SandboxProfile, err)
	}
	return TaskResultV1{JobID: job.ID, Outcome: "sandbox-profile-unavailable"}, true, nil
}

type Application struct {
	Store           ApplicationStore
	Tasks           *absurd.Client
	CleanupRuntimes CleanupRuntimeResolver
}

// RequestCleanup closes further admission, settles the currently attached
// task, and durably hands the Job to Core cleanup. Calling it again converges
// on the same cleanup task or completed receipt.
func (a Application) RequestCleanup(ctx context.Context, jobID string) (Job, error) {
	job, err := a.Store.Job(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	if job.CleanupState == CleanupComplete {
		return job, nil
	}
	if err := a.Store.CloseAdmissionForCleanup(ctx, jobID); err != nil {
		return Job{}, err
	}
	job, err = a.Store.Job(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	skipTaskID := currentTaskID(ctx)
	if err := a.cancelAttachedTask(ctx, job, skipTaskID); err != nil {
		return Job{}, err
	}

	var result Job
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

func (a Application) cancelAttachedTask(ctx context.Context, job Job, skipTaskID string) error {
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

// VerifyAttachedTask reconciles the public Absurd task identity with the
// exact durable Job attachment before any task is allowed to act.
func (a Application) VerifyAttachedTask(ctx context.Context, jobID, taskName string) error {
	return a.Store.WithJobFence(ctx, jobID, func() error {
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return absurd.ErrNoTaskContext
		}
		job, err := a.Store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		attachments, err := a.Store.JobTasks(ctx, jobID)
		if err != nil {
			return err
		}
		for _, attachment := range attachments {
			if attachment.TaskID != task.TaskID() {
				continue
			}
			if attachment.TaskID != job.CurrentTaskID {
				return fmt.Errorf("%s task %s is no longer the Job's current attachment", taskName, task.TaskID())
			}
			if attachment.TaskName != taskName {
				return fmt.Errorf("task %s is durably attached as %s, not %s", task.TaskID(), attachment.TaskName, taskName)
			}
			return verifyTaskContext(ctx, attachment.TaskID, attachment.TaskName)
		}
		if job.CurrentTaskID != task.TaskID() {
			if taskName == CleanupTaskName {
				err = a.Store.AttachCleanupTask(ctx, jobID, job.CurrentTaskID, task.TaskID(), taskName)
			} else {
				err = a.Store.AttachJobTask(ctx, jobID, job.CurrentTaskID, task.TaskID(), taskName)
			}
			if err != nil {
				return fmt.Errorf("recover public Spawn attachment for %s: %w", taskName, err)
			}
		}
		return verifyTaskContext(ctx, task.TaskID(), taskName)
	})
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
