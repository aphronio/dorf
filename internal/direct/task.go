package direct

import (
	"context"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	TaskName                = "dorf-direct-job-v1"
	activeAgentPollInterval = time.Second
	idleMessagePollInterval = 30 * time.Second
)

func TaskKey(sessionID string) string { return "direct-job:v1:" + sessionID }

type Execution interface {
	core.AgentReconciliation
	core.SandboxExecution
}

type Runtime struct {
	SandboxProfile core.SandboxProfileRef
	Execution      Execution
}

type RuntimeResolver interface {
	ResolveDirect(context.Context, core.SandboxProfileRef) (Runtime, error)
}

type Store interface {
	Session(context.Context, string) (core.Session, error)
}

// Register installs the direct client's durable bootstrap task. Once the
// exact Sandbox and route exist, Core's generic Agent reconciliation owns all
// Message delivery and recovery.
func Register(application core.Application, store Store, runtimes RuntimeResolver) {
	application.Tasks.MustRegister(absurd.Task(TaskName, func(ctx context.Context, params core.SessionTaskParams) (core.TaskResultV1, error) {
		if err := application.VerifyAttachedTask(ctx, params.SessionID, TaskName, params.PreviousTaskID); err != nil {
			return core.TaskResultV1{}, err
		}
		custody, err := application.OpenSession(ctx, params.SessionID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		session, err := store.Session(ctx, params.SessionID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		if session.Workflow != "" || session.WorkflowRevision != "" {
			return core.TaskResultV1{}, fmt.Errorf("Session %s is not direct", session.ID)
		}
		if runtimes == nil {
			return core.TaskResultV1{}, fmt.Errorf("Sandbox runtime resolution is not configured")
		}
		runtime, err := runtimes.ResolveDirect(ctx, session.ProfileRef())
		if err != nil {
			return core.TaskResultV1{}, fmt.Errorf("resolve Sandbox profile %q: %w", session.SandboxProfile, err)
		}
		if runtime.SandboxProfile != session.ProfileRef() {
			return core.TaskResultV1{}, fmt.Errorf("Session requires Sandbox profile %q, but this worker resolved %q", session.SandboxProfile, runtime.SandboxProfile)
		}
		if runtime.Execution == nil {
			return core.TaskResultV1{}, fmt.Errorf("direct Agent runtime is not configured")
		}

		mainSandboxID := core.MainSandboxName(session.ID)
		mainSandbox, err := custody.EnsureDefaultSandbox(ctx)
		if err != nil {
			source := core.ScopedActionID(session.ID, core.ActionSandboxCreate, mainSandboxID)
			if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.SessionID, source, err); stopped {
				return result, stopErr
			}
			return core.TaskResultV1{}, err
		}
		if mainSandbox.ID() != mainSandboxID {
			return core.TaskResultV1{}, fmt.Errorf("ensured Sandbox %s changed selected identity %s", mainSandbox.ID(), mainSandboxID)
		}
		if err := runtime.Execution.ExecuteSandboxAction(ctx, session.ID, mainSandbox.ID(), core.ActionRouteCreate); err != nil {
			source := core.ScopedActionID(session.ID, core.ActionRouteCreate, mainSandbox.ID())
			if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.SessionID, source, err); stopped {
				return result, stopErr
			}
			return core.TaskResultV1{}, err
		}

		for {
			revision, progress, err := reconcileAtWakeRevision(ctx, application, runtime.Execution, params.SessionID)
			if err != nil {
				if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.SessionID, params.SessionID, err); stopped {
					return result, stopErr
				}
				return core.TaskResultV1{}, err
			}
			if progress == core.AgentReconciliationReady {
				continue
			}
			core.ReconcileIdle(ctx, runtime.Execution, params.SessionID)
			expectedRevision := revision + 1
			stepName, timeout := wakeOptions(progress, expectedRevision)
			if err := application.AwaitSessionExecutionWake(ctx, params.SessionID, expectedRevision, stepName, timeout); err != nil {
				return core.TaskResultV1{}, err
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
}

func reconcileAtWakeRevision(ctx context.Context, application core.Application, execution core.AgentReconciliation, sessionID string) (int64, core.AgentReconciliationProgress, error) {
	revision, err := application.SessionExecutionWakeRevision(ctx, sessionID)
	if err != nil {
		return 0, core.AgentReconciliationIdle, err
	}
	progress, err := execution.ReconcileSessionAgent(ctx, sessionID)
	return revision, progress, err
}

func wakeOptions(progress core.AgentReconciliationProgress, revision int64) (string, time.Duration) {
	if progress == core.AgentReconciliationPending {
		return fmt.Sprintf("dorf/direct-agent-wake/v2/%020d", revision), activeAgentPollInterval
	}
	return fmt.Sprintf("dorf/direct-job-wake/v2/%020d", revision), idleMessagePollInterval
}
