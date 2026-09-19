package core

import (
	"context"
	"errors"
	"fmt"
	provider "github.com/aphronio/dorf/internal/sandbox"

	"github.com/aphronio/dorf/internal/sandbox"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const CleanupTaskName = "dorf-session-cleanup-v3"

type SessionTaskParams struct {
	SessionID string `json:"session_id"`
}

type TaskResultV1 struct {
	SessionID string `json:"session_id"`
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
	Native         NativeSession
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
	WithSessionFence(context.Context, string, func() error) error
	ScheduleCleanup(context.Context, string, string, string) error
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

// VerifyAttachedTask checks the committed task attachment and lifecycle state.
// Scheduling owns attachment; a running task cannot grant itself authority.
func (a Application) VerifyAttachedTask(ctx context.Context, sessionID, taskName string) error {
	return a.Store.WithSessionFence(ctx, sessionID, func() error {
		session, err := exactCurrentAttachedTask(ctx, a.Store, sessionID, taskName)
		if err != nil {
			return err
		}
		if taskName == CleanupTaskName {
			if session.AdmissionOpen || session.CleanupState != CleanupScheduled {
				return fmt.Errorf("cleanup task %s cannot act before cleanup is scheduled", session.CurrentTaskID)
			}
		} else if !session.AdmissionOpen || session.CleanupState != CleanupPending {
			return fmt.Errorf("ordinary task %s cannot act after cleanup begins", session.CurrentTaskID)
		}
		return nil
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
	if task.TaskName() != taskName || session.CurrentTaskID != task.TaskID() || session.CurrentTaskName != taskName {
		return Session{}, fmt.Errorf("task %s is not the exact current %s attachment for Session %s", task.TaskID(), taskName, sessionID)
	}
	return session, nil
}
