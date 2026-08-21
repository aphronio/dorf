package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/sandbox"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const CleanupTaskName = "dorf-job-cleanup-v3"

type JobTaskParams struct {
	JobID          string `json:"job_id"`
	PreviousTaskID string `json:"previous_task_id,omitempty"`
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

type SandboxRuntime struct {
	Execution      SandboxExecution
	SandboxProfile string
}

type SandboxRuntimeResolver interface {
	ResolveSandbox(context.Context, string) (SandboxRuntime, error)
}

// ApplicationStore is the durable Core custody required by the application boundary.
// PostgreSQL is the current implementation, not part of the consumer contract.
type ApplicationStore interface {
	Job(context.Context, string) (Job, error)
	Sandbox(context.Context, string) (Sandbox, error)
	EnsureSandbox(context.Context, string, string) (Sandbox, error)
	JobTasks(context.Context, string) ([]JobTask, error)
	CleanupRequests(context.Context) ([]string, error)
	WithJobFence(context.Context, string, func() error) error
	AttachJobTask(context.Context, string, string, string, string) error
	RequestCleanup(context.Context, string) error
	AttachCleanupTask(context.Context, string, string, string, string) error
	GetOrCreateSandboxAction(context.Context, string, ActionKind) (Action, error)
	RecordSandboxActionSuccess(context.Context, string) error
	RecordSandboxProfileUnavailable(context.Context, string, string, string, error) error
	SetCleanupAttention(context.Context, string, string) error
	CompleteCleanup(context.Context, string, string) error
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
		return TaskResultV1{}, true, errors.Join(cause, fmt.Errorf("record unavailable Sandbox profile %q: %w", job.SandboxProfile, err))
	}
	return TaskResultV1{JobID: job.ID, Outcome: "sandbox-profile-unavailable"}, true, nil
}

type Application struct {
	Store           ApplicationStore
	Tasks           *absurd.Client
	SandboxRuntimes SandboxRuntimeResolver
	CleanupRuntimes CleanupRuntimeResolver
}

func (a Application) requestCleanup(ctx context.Context, jobID string) (Job, error) {
	job, err := a.Store.Job(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	if job.CleanupState == CleanupComplete {
		return job, nil
	}
	if job.CleanupState == CleanupScheduled {
		return job, nil
	}
	if job.CleanupState == CleanupPending {
		if err := a.Store.WithJobFence(ctx, jobID, func() error {
			return a.Store.RequestCleanup(ctx, jobID)
		}); err != nil {
			return Job{}, err
		}
	}
	job, err = a.Store.Job(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	// Another requester or continuous recovery may have attached the cleanup task
	// after this caller observed requested. Never cancel that winning cleanup.
	if job.CleanupState == CleanupScheduled || job.CleanupState == CleanupComplete {
		return job, nil
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
		if current.CleanupState == CleanupScheduled || current.CleanupState == CleanupComplete {
			result = current
			return nil
		}
		if err := a.cancelAttachedTask(ctx, current, skipTaskID); err != nil {
			return err
		}
		spawned, err := a.Tasks.Spawn(ctx, CleanupTaskName, JobTaskParams{JobID: jobID, PreviousTaskID: current.CurrentTaskID}, absurdruntime.TaskSpawnOptions("cleanup:v3:"+jobID))
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

// RecoverCleanupRequests schedules every explicit cleanup request that was
// durably recorded before its public Absurd Spawn could be attached.
func (a Application) RecoverCleanupRequests(ctx context.Context) error {
	jobIDs, err := a.Store.CleanupRequests(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		if _, err := a.requestCleanup(ctx, jobID); err != nil {
			return fmt.Errorf("recover cleanup request for Job %s: %w", jobID, err)
		}
	}
	return nil
}

// ReconcileCleanupRequests continuously closes the only public Spawn gap:
// a durable cleanup request whose requester exited before attaching cleanup.
func (a Application) ReconcileCleanupRequests(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("cleanup reconciliation interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := a.RecoverCleanupRequests(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
func (a Application) VerifyAttachedTask(ctx context.Context, jobID, taskName, expectedPreviousTaskID string) error {
	return a.Store.WithJobFence(ctx, jobID, func() error {
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return absurd.ErrNoTaskContext
		}
		job, err := a.Store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if taskName == CleanupTaskName {
			if job.AdmissionOpen || (job.CleanupState != CleanupRequested && job.CleanupState != CleanupScheduled) {
				return fmt.Errorf("cleanup task %s cannot act before cleanup is requested", task.TaskID())
			}
		} else if !job.AdmissionOpen || job.CleanupState != CleanupPending {
			return fmt.Errorf("ordinary task %s cannot act after cleanup begins", task.TaskID())
		}
		attachments, err := a.Store.JobTasks(ctx, jobID)
		if err != nil {
			return err
		}
		for i, attachment := range attachments {
			if attachment.TaskID != task.TaskID() {
				continue
			}
			if attachment.TaskID != job.CurrentTaskID {
				return fmt.Errorf("%s task %s is no longer the Job's current attachment", taskName, task.TaskID())
			}
			if attachment.TaskName != taskName {
				return fmt.Errorf("task %s is durably attached as %s, not %s", task.TaskID(), attachment.TaskName, taskName)
			}
			previous := ""
			if i > 0 {
				previous = attachments[i-1].TaskID
			}
			if previous != expectedPreviousTaskID {
				return fmt.Errorf("task %s predecessor is %q, not Spawn predecessor %q", task.TaskID(), previous, expectedPreviousTaskID)
			}
			return verifyTaskContext(ctx, attachment.TaskID, attachment.TaskName)
		}
		if job.CurrentTaskID != task.TaskID() {
			if taskName == CleanupTaskName {
				err = a.Store.AttachCleanupTask(ctx, jobID, expectedPreviousTaskID, task.TaskID(), taskName)
			} else {
				err = a.Store.AttachJobTask(ctx, jobID, expectedPreviousTaskID, task.TaskID(), taskName)
			}
			if err != nil {
				return fmt.Errorf("recover public Spawn attachment for %s: %w", taskName, err)
			}
		}
		return verifyTaskContext(ctx, task.TaskID(), taskName)
	})
}

func (a Application) verifyCurrentTask(ctx context.Context, jobID, taskName string) error {
	return a.Store.WithJobFence(ctx, jobID, func() error {
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return absurd.ErrNoTaskContext
		}
		job, err := a.Store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if !job.AdmissionOpen || job.CleanupState != CleanupPending || job.CurrentTaskID != task.TaskID() {
			return fmt.Errorf("task %s is not the exact current open Job attachment", task.TaskID())
		}
		attachments, err := a.Store.JobTasks(ctx, jobID)
		if err != nil {
			return err
		}
		for _, attachment := range attachments {
			if attachment.TaskID == task.TaskID() && attachment.TaskName == taskName {
				return verifyTaskContext(ctx, attachment.TaskID, attachment.TaskName)
			}
		}
		return fmt.Errorf("task %s has no exact durable Job attachment", task.TaskID())
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
