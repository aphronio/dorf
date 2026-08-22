package coding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/profile"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	TaskName                = "dorf-coding-job-v3"
	activeAgentPollInterval = time.Second
	idleMessagePollInterval = 30 * time.Second
)

func TaskKey(jobID string) string { return "coding-job:v3:" + jobID }

type ProviderChecker interface {
	Check(context.Context, string) error
}

type Runtime struct {
	Profile  profile.Runtime
	Agent    core.AgentReconciliation
	Coding   CodingExecution
	Proposal ProposalRuntime
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
			if runtime.Agent == nil {
				return core.TaskResultV1{}, fmt.Errorf("Agent reconciliation is not configured")
			}
			if err := runtime.Agent.ReconcileJobAgent(ctx, params.JobID); err != nil {
				if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.JobID, params.JobID, err); stopped {
					return result, stopErr
				}
				return core.TaskResultV1{}, err
			}
			work, err := RunJob(ctx, jobHandle, runtime.Coding, store, proposal, params.JobID)
			if err != nil {
				if result, stopped, stopErr := application.StopForUnavailableSandboxProfile(ctx, params.JobID, work.FactID, err); stopped {
					return result, stopErr
				}
				return core.TaskResultV1{}, err
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
			wake, err := absurd.AwaitEvent[core.MessageWakeV1](ctx, core.MessageWakeEvent(params.JobID, sequence), wakeOptions(work, sequence, proposal.PollInterval))
			if err != nil {
				var timeout *absurd.TimeoutError
				if errors.As(err, &timeout) {
					continue
				}
				return core.TaskResultV1{}, err
			}
			if wake.JobID != params.JobID || wake.Sequence != sequence {
				return core.TaskResultV1{}, fmt.Errorf("message wake payload conflicts with Job %s sequence %d", params.JobID, sequence)
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
}

func runtimeForJob(ctx context.Context, store Store, runtimes RuntimeResolver, jobID string) (Runtime, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return Runtime{}, err
	}
	definition := WorkflowDefinition()
	if job.Workflow != definition.Name || job.WorkflowRevision != definition.Revision {
		detail := fmt.Sprintf("Job requires workflow %s revision %s, but task executes %s revision %s", job.Workflow, job.WorkflowRevision, definition.Name, definition.Revision)
		return Runtime{}, recordRuntimeAttention(ctx, store, job.ID, "workflow-profile", detail)
	}
	if runtimes == nil {
		return Runtime{}, fmt.Errorf("Sandbox runtime resolution is not configured")
	}
	runtime, err := runtimes.ResolveCoding(ctx, job.SandboxProfile)
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve Sandbox profile %q: %w", job.SandboxProfile, err)
	}
	if configured := strings.TrimSpace(runtime.Profile.SandboxProfile); configured != job.SandboxProfile {
		detail := fmt.Sprintf("Job requires Sandbox profile %q, but this worker resolved %q", job.SandboxProfile, configured)
		return Runtime{}, recordRuntimeAttention(ctx, store, job.ID, "sandbox-profile", detail)
	}
	if err := runtime.Profile.Require(definition.Name, definition.Revision, definition.RequiredProviderCapabilities); err != nil {
		return Runtime{}, recordRuntimeAttention(ctx, store, job.ID, "provider-capabilities", err.Error())
	}
	return runtime, nil
}

func recordRuntimeAttention(ctx context.Context, store Store, jobID, source, detail string) error {
	if err := store.SetWorkflowAttention(ctx, jobID, source, detail); err != nil {
		return fmt.Errorf("%s; record workflow attention: %w", detail, err)
	}
	return errors.New(detail)
}

func wakeOptions(work Work, sequence int64, proposalPollInterval time.Duration) absurd.AwaitEventOptions {
	options := absurd.AwaitEventOptions{StepName: fmt.Sprintf("dorf/message-wake/v1/%020d", sequence), Timeout: idleMessagePollInterval}
	switch work.Kind {
	case WorkObserveProposal:
		options.StepName = fmt.Sprintf("dorf/proposal-wake/v2/%s/%020d", work.Revision, sequence)
		options.Timeout = proposalPollInterval
	case WorkWaitAgent:
		options.StepName = fmt.Sprintf("dorf/agent-run-wake/v1/%s/%020d", work.FactID, sequence)
		options.Timeout = activeAgentPollInterval
	}
	return options
}

func Admit(ctx context.Context, store Store, application core.Application, providers ProviderChecker, runtime profile.Runtime, input Admission) (core.Job, bool, error) {
	input.Workflow = Workflow
	input.WorkflowRevision = WorkflowRevision
	key := strings.TrimSpace(input.AdmissionKey)
	if key != "" {
		exists, err := store.JobExists(ctx, core.JobID(key))
		if err != nil {
			return core.Job{}, false, err
		}
		if !exists {
			definition := WorkflowDefinition()
			if err := runtime.Require(definition.Name, definition.Revision, definition.RequiredProviderCapabilities); err != nil {
				return core.Job{}, false, err
			}
			if providers == nil {
				return core.Job{}, false, fmt.Errorf("provider readiness is not configured")
			}
			if err := providers.Check(ctx, strings.TrimSpace(input.ProviderConnection)); err != nil {
				return core.Job{}, false, fmt.Errorf("AI connection %q is not ready: %w", strings.TrimSpace(input.ProviderConnection), err)
			}
		}
	}
	job, created, err := store.AdmitCoding(ctx, input)
	if err != nil || !job.AdmissionOpen {
		return job, created, err
	}
	if err := application.ScheduleJobTask(ctx, job, TaskName, TaskKey(job.ID)); err != nil {
		return core.Job{}, false, err
	}
	job, err = store.Job(ctx, job.ID)
	return job, created, err
}
