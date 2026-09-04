package direct

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	TaskName                = "dorf-direct-job-v1"
	activeAgentPollInterval = time.Second
	idleMessagePollInterval = 30 * time.Second
)

func TaskKey(jobID string) string { return "direct-job:v1:" + jobID }

type Execution interface {
	core.AgentReconciliation
	core.SandboxExecution
}

type Runtime struct {
	SandboxProfile string
	Execution      Execution
}

type RuntimeResolver interface {
	ResolveDirect(context.Context, string) (Runtime, error)
}

type Store interface {
	Job(context.Context, string) (core.Job, error)
	NextWakeSequence(context.Context, string) (int64, error)
}

// Register installs the direct client's durable bootstrap task. Once the
// exact Sandbox and route exist, Core's generic Agent reconciliation owns all
// Message delivery and recovery.
func Register(application core.Application, store Store, runtimes RuntimeResolver) {
	application.Tasks.MustRegister(absurd.Task(TaskName, func(ctx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		if err := application.VerifyAttachedTask(ctx, params.JobID, TaskName, params.PreviousTaskID); err != nil {
			return core.TaskResultV1{}, err
		}
		custody, err := application.OpenJob(ctx, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		job, err := store.Job(ctx, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		if job.Workflow != "" || job.WorkflowRevision != "" {
			return core.TaskResultV1{}, fmt.Errorf("Job %s is not direct", job.ID)
		}
		if runtimes == nil {
			return core.TaskResultV1{}, fmt.Errorf("Sandbox runtime resolution is not configured")
		}
		runtime, err := runtimes.ResolveDirect(ctx, job.SandboxProfile)
		if err != nil {
			return core.TaskResultV1{}, fmt.Errorf("resolve Sandbox profile %q: %w", job.SandboxProfile, err)
		}
		if strings.TrimSpace(runtime.SandboxProfile) != job.SandboxProfile {
			return core.TaskResultV1{}, fmt.Errorf("Job requires Sandbox profile %q, but this worker resolved %q", job.SandboxProfile, runtime.SandboxProfile)
		}
		if runtime.Execution == nil {
			return core.TaskResultV1{}, fmt.Errorf("direct Agent runtime is not configured")
		}

		mainSandboxID := core.MainSandboxName(job.ID)
		mainSandbox, err := custody.EnsureDefaultSandbox(ctx)
		if err != nil {
			source := core.ScopedActionID(job.ID, core.ActionSandboxCreate, mainSandboxID)
			if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.JobID, source, err); stopped {
				return result, stopErr
			}
			return core.TaskResultV1{}, err
		}
		if mainSandbox.ID() != mainSandboxID {
			return core.TaskResultV1{}, fmt.Errorf("ensured Sandbox %s changed selected identity %s", mainSandbox.ID(), mainSandboxID)
		}
		if err := runtime.Execution.ExecuteSandboxAction(ctx, job.ID, mainSandbox.ID(), core.ActionRouteCreate); err != nil {
			source := core.ScopedActionID(job.ID, core.ActionRouteCreate, mainSandbox.ID())
			if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.JobID, source, err); stopped {
				return result, stopErr
			}
			return core.TaskResultV1{}, err
		}

		for {
			progress, err := runtime.Execution.ReconcileJobAgent(ctx, params.JobID)
			if err != nil {
				if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.JobID, params.JobID, err); stopped {
					return result, stopErr
				}
				return core.TaskResultV1{}, err
			}
			sequence, err := store.NextWakeSequence(ctx, params.JobID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			stepName, timeout := wakeOptions(progress, sequence)
			if err := application.AwaitMessageWake(ctx, params.JobID, sequence, stepName, timeout); err != nil {
				return core.TaskResultV1{}, err
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
}

func wakeOptions(progress core.AgentReconciliationProgress, sequence int64) (string, time.Duration) {
	if progress == core.AgentReconciliationPending {
		return fmt.Sprintf("dorf/direct-agent-wake/v1/%020d", sequence), activeAgentPollInterval
	}
	return fmt.Sprintf("dorf/direct-message-wake/v1/%020d", sequence), idleMessagePollInterval
}
