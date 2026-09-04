package investigation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	TaskName                = "dorf-codebase-investigation-v2"
	activeAgentPollInterval = time.Second
	idleMessagePollInterval = 30 * time.Second
)

func TaskKey(jobID string) string { return "codebase-investigation:v2:" + jobID }

type Runtime struct {
	SandboxProfile string
	Agent          core.AgentReconciliation
	Investigation  gitworkspace.Execution
}

type RuntimeResolver interface {
	ResolveInvestigation(context.Context, string) (Runtime, error)
}

// Register installs the investigation workflow's task and recovery loop.
func Register(application core.Application, store Store, runtimes RuntimeResolver) {
	application.Tasks.MustRegister(absurd.Task(TaskName, func(ctx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		if err := application.VerifyAttachedTask(ctx, params.JobID, TaskName, params.PreviousTaskID); err != nil {
			return core.TaskResultV1{}, err
		}
		jobHandle, err := application.OpenJob(ctx, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		runtime, err := runtimeForJob(ctx, store, runtimes, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		for {
			work, err := Run(ctx, jobHandle, runtime.Investigation, store, params.JobID)
			if err != nil {
				if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.JobID, work.FactID, err); stopped {
					return result, stopErr
				}
				return core.TaskResultV1{}, err
			}
			if work.Kind == WorkWaitAgent {
				if runtime.Agent == nil {
					return core.TaskResultV1{}, fmt.Errorf("Agent reconciliation is not configured")
				}
				if _, err := runtime.Agent.ReconcileJobAgent(ctx, params.JobID); err != nil {
					if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.JobID, work.FactID, err); stopped {
						return result, stopErr
					}
					return core.TaskResultV1{}, err
				}
			}
			if work.Kind == WorkComplete {
				return core.TaskResultV1{JobID: params.JobID, Outcome: "admission-closed"}, nil
			}
			sequence, err := store.NextWakeSequence(ctx, params.JobID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			stepName, timeout := wakeOptions(work, sequence)
			if err := application.AwaitMessageWake(ctx, params.JobID, sequence, stepName, timeout); err != nil {
				return core.TaskResultV1{}, err
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
}

func wakeOptions(work Work, sequence int64) (string, time.Duration) {
	stepName, timeout := fmt.Sprintf("dorf/investigation-wake/v2/%020d", sequence), idleMessagePollInterval
	if work.Kind == WorkWaitAgent {
		stepName, timeout = fmt.Sprintf("dorf/investigation-agent-wake/v2/%s/%020d", work.FactID, sequence), activeAgentPollInterval
	}
	return stepName, timeout
}

func runtimeForJob(ctx context.Context, store Store, runtimes RuntimeResolver, jobID string) (Runtime, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return Runtime{}, err
	}
	if job.Workflow != Workflow || job.WorkflowRevision != WorkflowRevision {
		detail := fmt.Sprintf("Job requires workflow %s revision %s, but task executes %s revision %s", job.Workflow, job.WorkflowRevision, Workflow, WorkflowRevision)
		return Runtime{}, recordRuntimeAttention(ctx, store, job.ID, "workflow-profile", detail)
	}
	if runtimes == nil {
		return Runtime{}, fmt.Errorf("Sandbox runtime resolution is not configured")
	}
	runtime, err := runtimes.ResolveInvestigation(ctx, job.SandboxProfile)
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve Sandbox profile %q: %w", job.SandboxProfile, err)
	}
	if configured := strings.TrimSpace(runtime.SandboxProfile); configured != job.SandboxProfile {
		detail := fmt.Sprintf("Job requires Sandbox profile %q, but this worker resolved %q", job.SandboxProfile, configured)
		return Runtime{}, recordRuntimeAttention(ctx, store, job.ID, "sandbox-profile", detail)
	}
	return runtime, nil
}

func recordRuntimeAttention(ctx context.Context, store Store, jobID, source, detail string) error {
	if err := store.SetWorkflowAttention(ctx, jobID, source, detail); err != nil {
		return fmt.Errorf("%s; record workflow attention: %w", detail, err)
	}
	return errors.New(detail)
}
