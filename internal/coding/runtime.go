package coding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	TaskName                = "dorf-coding-job-v3"
	activeAgentPollInterval = time.Second
	idleMessagePollInterval = 30 * time.Second
)

func TaskKey(jobID string) string { return "coding-job:v3:" + jobID }

type Runtime struct {
	SandboxProfile string
	Agent          core.AgentReconciliation
	Coding         CodingExecution
	Proposal       ProposalRuntime
}

type RuntimeResolver interface {
	ResolveCoding(context.Context, string) (Runtime, error)
}

// Register installs the coding workflow's task and recovery loop.
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
		proposal := runtime.Proposal
		if proposal.PollInterval <= 0 {
			proposal.PollInterval = 30 * time.Second
		}
		for {
			work, err := RunJob(ctx, jobHandle, runtime.Coding, store, proposal, params.JobID)
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
				outcome, err := store.Outcome(ctx, params.JobID)
				if err != nil {
					return core.TaskResultV1{}, err
				}
				if outcome != nil {
					if err := jobHandle.RequestCleanup(ctx); err != nil {
						return core.TaskResultV1{}, err
					}
					return core.TaskResultV1{JobID: params.JobID, Outcome: string(outcome.Kind)}, nil
				}
				return core.TaskResultV1{JobID: params.JobID, Outcome: "admission-closed"}, nil
			}
			sequence, err := store.NextWakeSequence(ctx, params.JobID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			stepName, timeout := wakeOptions(work, sequence, proposal.PollInterval)
			if err := application.AwaitMessageWake(ctx, params.JobID, sequence, stepName, timeout); err != nil {
				return core.TaskResultV1{}, err
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
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
	runtime, err := runtimes.ResolveCoding(ctx, job.SandboxProfile)
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

func wakeOptions(work Work, sequence int64, proposalPollInterval time.Duration) (string, time.Duration) {
	stepName, timeout := fmt.Sprintf("dorf/message-wake/v1/%020d", sequence), idleMessagePollInterval
	switch work.Kind {
	case WorkObserveProposal:
		stepName, timeout = fmt.Sprintf("dorf/proposal-wake/v2/%s/%020d", work.Revision, sequence), proposalPollInterval
	case WorkWaitAgent:
		stepName, timeout = fmt.Sprintf("dorf/agent-run-wake/v1/%s/%020d", work.FactID, sequence), activeAgentPollInterval
	}
	return stepName, timeout
}
