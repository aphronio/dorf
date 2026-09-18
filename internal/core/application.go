package core

import (
	"context"
	"errors"
	"fmt"
	provider "github.com/aphronio/dorf/internal/sandbox"

	"github.com/aphronio/dorf/internal/sandbox"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// Persisted task names and payload keys remain stable across the Session rename.
const CleanupTaskName = "dorf-job-cleanup-v3"

type SessionTaskParams struct {
	SessionID      string `json:"job_id"`
	PreviousTaskID string `json:"previous_task_id,omitempty"`
}

type TaskResultV1 struct {
	SessionID string `json:"job_id"`
	Outcome   string `json:"outcome"`
}

// CleanupRuntime is the provider-neutral capability resolved from one Session's
// durably pinned Sandbox profile after cleanup has been requested.
type CleanupRuntime struct {
	Execution      CleanupExecution
	SandboxProfile SandboxProfileRef
}

type CleanupRuntimeResolver interface {
	ResolveCleanup(context.Context, SandboxProfileRef) (CleanupRuntime, error)
}

type SandboxStatusReader interface {
	ReadSandboxStatus(context.Context, Session, Sandbox) (provider.Status, error)
}

type SandboxRuntime struct {
	Status         SandboxStatusReader
	Timeline       SandboxTimelineReader
	Execution      Execution
	Files          SandboxFileReader
	Commands       SandboxCommandExecutor
	SandboxProfile SandboxProfileRef
}

type SandboxFileReader interface {
	ReadSandboxFile(context.Context, Session, Sandbox, string) ([]byte, error)
}

type SandboxFileWriter interface {
	WriteSandboxFile(context.Context, Session, Sandbox, string, []byte, bool) error
}

type SandboxCommandExecutor interface {
	ExecSandbox(context.Context, Session, Sandbox, provider.Command) (provider.CommandResult, error)
}

type SandboxRuntimeResolver interface {
	ResolveSandbox(context.Context, SandboxProfileRef) (SandboxRuntime, error)
}

// ApplicationStore is the durable Core custody required by the application boundary.
// PostgreSQL is the current implementation, not part of the consumer contract.
type ApplicationStore interface {
	SandboxActivityStore
	Session(context.Context, string) (Session, error)
	Sandbox(context.Context, string) (Sandbox, error)
	SessionTasks(context.Context, string) ([]SessionTask, error)
	WithSessionFence(context.Context, string, func() error) error
	AttachSessionTask(context.Context, string, string, string, string) error
	ScheduleCleanup(context.Context, string, string, string) error
	AttachCleanupTask(context.Context, string, string, string, string) error
	RecordSandboxProfileUnavailable(context.Context, string, string, string, error) error
	SetCleanupAttention(context.Context, string, string) error
}

// StopForUnavailableSandboxProfile turns one definitive provider artifact
// failure into durable attention instead of asking Absurd to retry an input
// that cannot succeed. The concrete consumer supplies the exact current fact identity.
func (a Application) StopForUnavailableSandboxProfile(ctx context.Context, sessionID, source string, cause error) (TaskResultV1, bool, error) {
	if !sandbox.IsArtifactUnavailable(cause) {
		return TaskResultV1{}, false, nil
	}
	session, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return TaskResultV1{}, true, err
	}
	if err := a.Store.RecordSandboxProfileUnavailable(ctx, session.ID, session.SandboxProfile, source, cause); err != nil {
		return TaskResultV1{}, true, errors.Join(cause, fmt.Errorf("record unavailable Sandbox profile %q: %w", session.SandboxProfile, err))
	}
	return TaskResultV1{SessionID: session.ID, Outcome: "sandbox-profile-unavailable"}, true, nil
}

type Application struct {
	Store           ApplicationStore
	Tasks           *absurd.Client
	AgentMessages   AgentMessageAdmission
	SandboxRuntimes SandboxRuntimeResolver
	CleanupRuntimes CleanupRuntimeResolver
}

func (a Application) requestCleanup(ctx context.Context, sessionID string) (Session, error) {
	if a.Tasks == nil {
		return Session{}, fmt.Errorf("cleanup scheduling is not configured")
	}
	if err := a.Store.ScheduleCleanup(ctx, a.Tasks.QueueName(), sessionID, currentTaskID(ctx)); err != nil {
		return Session{}, err
	}
	return a.Store.Session(ctx, sessionID)
}

func currentTaskID(ctx context.Context) string {
	task, ok := absurd.TaskFromContext(ctx)
	if !ok {
		return ""
	}
	return task.TaskID()
}

// VerifyAttachedTask reconciles the public Absurd task identity with the
// exact durable Session attachment before any task is allowed to act.
func (a Application) VerifyAttachedTask(ctx context.Context, sessionID, taskName, expectedPreviousTaskID string) error {
	return a.Store.WithSessionFence(ctx, sessionID, func() error {
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return absurd.ErrNoTaskContext
		}
		session, err := a.Store.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		if taskName == CleanupTaskName {
			if session.AdmissionOpen || (session.CleanupState != CleanupRequested && session.CleanupState != CleanupScheduled) {
				return fmt.Errorf("cleanup task %s cannot act before cleanup is requested", task.TaskID())
			}
		} else if !session.AdmissionOpen || session.CleanupState != CleanupPending {
			return fmt.Errorf("ordinary task %s cannot act after cleanup begins", task.TaskID())
		}
		attachments, err := a.Store.SessionTasks(ctx, sessionID)
		if err != nil {
			return err
		}
		for i, attachment := range attachments {
			if attachment.TaskID != task.TaskID() {
				continue
			}
			if attachment.TaskID != session.CurrentTaskID {
				return fmt.Errorf("%s task %s is no longer the Session's current attachment", taskName, task.TaskID())
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
		if session.CurrentTaskID != task.TaskID() {
			if taskName == CleanupTaskName {
				err = a.Store.AttachCleanupTask(ctx, sessionID, expectedPreviousTaskID, task.TaskID(), taskName)
			} else {
				err = a.Store.AttachSessionTask(ctx, sessionID, expectedPreviousTaskID, task.TaskID(), taskName)
			}
			if err != nil {
				return fmt.Errorf("recover public Spawn attachment for %s: %w", taskName, err)
			}
		}
		return verifyTaskContext(ctx, task.TaskID(), taskName)
	})
}

func (a Application) verifyCurrentTask(ctx context.Context, sessionID, taskName string) error {
	return a.Store.WithSessionFence(ctx, sessionID, func() error {
		session, err := exactCurrentAttachedTask(ctx, a.Store, sessionID, taskName)
		if err != nil {
			return err
		}
		if !session.AdmissionOpen || session.CleanupState != CleanupPending {
			return fmt.Errorf("task %s is not the exact current open Session attachment", session.CurrentTaskID)
		}
		return nil
	})
}

type currentTaskStore interface {
	Session(context.Context, string) (Session, error)
	SessionTasks(context.Context, string) ([]SessionTask, error)
}

// exactCurrentAttachedTask is the one authority check shared by Core's Session
// effects. It proves that the running Absurd task is both the Session's current
// task and the exact durably attached task name.
func exactCurrentAttachedTask(ctx context.Context, store currentTaskStore, sessionID, taskName string) (Session, error) {
	task, ok := absurd.TaskFromContext(ctx)
	if !ok {
		return Session{}, absurd.ErrNoTaskContext
	}
	if taskName == "" {
		taskName = task.TaskName()
	}
	session, err := store.Session(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	if task.TaskName() != taskName || session.CurrentTaskID != task.TaskID() {
		return Session{}, fmt.Errorf("task %s is not the exact current %s attachment for Session %s", task.TaskID(), taskName, sessionID)
	}
	attachments, err := store.SessionTasks(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	for _, attachment := range attachments {
		if attachment.TaskID == task.TaskID() && attachment.TaskName == taskName {
			return session, verifyTaskContext(ctx, attachment.TaskID, attachment.TaskName)
		}
	}
	return Session{}, fmt.Errorf("task %s has no exact durable %s attachment for Session %s", task.TaskID(), taskName, sessionID)
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
