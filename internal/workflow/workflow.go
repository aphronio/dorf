package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	RunTaskName             = postgres.MessageTaskName
	InvestigationTaskName   = "dorf-codebase-investigation-v2"
	activeAgentPollInterval = time.Second
)

// WakeV1 is persisted by Absurd under one immutable Job-local FIFO event.
type WakeV1 struct {
	JobID    string `json:"job_id"`
	Sequence int64  `json:"sequence"`
}

type ProviderChecker interface {
	Check(context.Context, string) error
}

type sandboxProfileStore interface {
	Job(context.Context, string) (core.Job, error)
	SetWorkflowAttention(context.Context, string, string, string) error
}

type CodingRuntime struct {
	Profile  RuntimeProfile
	Coding   coding.CodingExecution
	Proposal coding.ProposalRuntime
}

type InvestigationRuntime struct {
	Profile       RuntimeProfile
	Investigation investigation.Service
}

type RuntimeResolver interface {
	ResolveCoding(context.Context, string) (CodingRuntime, error)
	ResolveInvestigation(context.Context, string) (InvestigationRuntime, error)
}

func WakeEvent(jobID string, sequence int64) string {
	return fmt.Sprintf("dorf.job-message:%s:%020d", jobID, sequence)
}

func Register(client *absurd.Client, store postgres.Store, runtimes RuntimeResolver, application core.Application) {
	client.MustRegister(absurd.Task(RunTaskName, func(ctx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		if err := application.VerifyAttachedTask(ctx, params.JobID, RunTaskName); err != nil {
			return core.TaskResultV1{}, err
		}
		runtime, err := codingRuntimeForJob(ctx, store, runtimes, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		proposal := runtime.Proposal
		if proposal.PollInterval <= 0 {
			proposal.PollInterval = 30 * time.Second
		}
		// Sequence 1 is present before this task is spawned. Every later FIFO
		// position owns one immutable Absurd event identity, starting at 2.
		for {
			work, err := coding.RunJob(ctx, runtime.Coding, store, proposal, params.JobID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			if work.Kind == coding.WorkComplete {
				outcome, err := store.Outcome(ctx, params.JobID)
				if err != nil {
					return core.TaskResultV1{}, err
				}
				if outcome != nil {
					if _, err := application.RequestCleanup(ctx, params.JobID); err != nil {
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
			options := wakeOptions(work, sequence, proposal.PollInterval)
			wake, err := absurd.AwaitEvent[WakeV1](ctx, WakeEvent(params.JobID, sequence), options)
			if err != nil {
				var timeout *absurd.TimeoutError
				if (work.Kind == coding.WorkObserveProposal || work.Kind == coding.WorkObserveAgent) && errors.As(err, &timeout) {
					continue
				}
				return core.TaskResultV1{}, err
			}
			if wake.JobID != params.JobID || wake.Sequence != sequence {
				return core.TaskResultV1{}, fmt.Errorf("message wake payload conflicts with Job %s sequence %d", params.JobID, sequence)
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
	client.MustRegister(absurd.Task(InvestigationTaskName, func(ctx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		if err := application.VerifyAttachedTask(ctx, params.JobID, InvestigationTaskName); err != nil {
			return core.TaskResultV1{}, err
		}
		runtime, err := investigationRuntimeForJob(ctx, store, runtimes, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		for {
			work, err := investigation.Run(ctx, runtime.Investigation, store, params.JobID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			if work.Kind == investigation.WorkComplete {
				return core.TaskResultV1{JobID: params.JobID, Outcome: "admission-closed"}, nil
			}
			sequence, err := store.NextWakeSequence(ctx, params.JobID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			options := absurd.AwaitEventOptions{StepName: fmt.Sprintf("dorf/investigation-wake/v2/%020d", sequence)}
			if work.Kind == investigation.WorkObserveAgent {
				options.StepName = fmt.Sprintf("dorf/investigation-agent-wake/v2/%s/%020d", work.FactID, sequence)
				options.Timeout = activeAgentPollInterval
			}
			wake, err := absurd.AwaitEvent[WakeV1](ctx, WakeEvent(params.JobID, sequence), options)
			if err != nil {
				var timeout *absurd.TimeoutError
				if work.Kind == investigation.WorkObserveAgent && errors.As(err, &timeout) {
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

func codingRuntimeForJob(ctx context.Context, store sandboxProfileStore, runtimes RuntimeResolver, jobID string) (CodingRuntime, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return CodingRuntime{}, err
	}
	expected := CodingToProposalDefinition()
	if err := requireWorkflow(ctx, store, job, expected); err != nil {
		return CodingRuntime{}, err
	}
	if runtimes == nil {
		return CodingRuntime{}, fmt.Errorf("Sandbox runtime resolution is not configured")
	}
	runtime, err := runtimes.ResolveCoding(ctx, job.SandboxProfile)
	if err != nil {
		return CodingRuntime{}, fmt.Errorf("resolve Sandbox profile %q: %w", job.SandboxProfile, err)
	}
	if err := requireJobProfile(ctx, store, job, runtime.Profile, expected); err != nil {
		return CodingRuntime{}, err
	}
	return runtime, nil
}

func investigationRuntimeForJob(ctx context.Context, store sandboxProfileStore, runtimes RuntimeResolver, jobID string) (InvestigationRuntime, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return InvestigationRuntime{}, err
	}
	expected := CodebaseInvestigationDefinition()
	if err := requireWorkflow(ctx, store, job, expected); err != nil {
		return InvestigationRuntime{}, err
	}
	if runtimes == nil {
		return InvestigationRuntime{}, fmt.Errorf("Sandbox runtime resolution is not configured")
	}
	runtime, err := runtimes.ResolveInvestigation(ctx, job.SandboxProfile)
	if err != nil {
		return InvestigationRuntime{}, fmt.Errorf("resolve Sandbox profile %q: %w", job.SandboxProfile, err)
	}
	if err := requireJobProfile(ctx, store, job, runtime.Profile, expected); err != nil {
		return InvestigationRuntime{}, err
	}
	return runtime, nil
}

func requireWorkflow(ctx context.Context, store sandboxProfileStore, job core.Job, expected Definition) error {
	if job.Workflow != expected.Name || job.WorkflowRevision != expected.Revision {
		detail := fmt.Sprintf("Job requires workflow %s revision %s, but task executes %s revision %s", job.Workflow, job.WorkflowRevision, expected.Name, expected.Revision)
		attentionErr := store.SetWorkflowAttention(ctx, job.ID, "workflow-profile", detail)
		if attentionErr != nil {
			return fmt.Errorf("%s; record workflow mismatch attention: %w", detail, attentionErr)
		}
		return errors.New(detail)
	}
	return nil
}

func requireJobProfile(ctx context.Context, store sandboxProfileStore, job core.Job, profile RuntimeProfile, expected Definition) error {
	configured := strings.TrimSpace(profile.SandboxProfile)
	if job.SandboxProfile != configured {
		detail := fmt.Sprintf("Job requires Sandbox profile %q, but this worker resolved %q", job.SandboxProfile, configured)
		attentionErr := store.SetWorkflowAttention(ctx, job.ID, "sandbox-profile", detail)
		if attentionErr != nil {
			return fmt.Errorf("%s; record profile mismatch attention: %w", detail, attentionErr)
		}
		return errors.New(detail)
	}
	if err := profile.Require(expected.Name, expected.Revision, expected.RequiredProviderCapabilities); err != nil {
		detail := err.Error()
		attentionErr := store.SetWorkflowAttention(ctx, job.ID, "provider-capabilities", detail)
		if attentionErr != nil {
			return fmt.Errorf("%s; record provider capability attention: %w", detail, attentionErr)
		}
		return errors.New(detail)
	}
	return nil
}

func wakeOptions(work coding.Work, sequence int64, proposalPollInterval time.Duration) absurd.AwaitEventOptions {
	options := absurd.AwaitEventOptions{StepName: fmt.Sprintf("dorf/message-wake/v1/%020d", sequence)}
	switch work.Kind {
	case coding.WorkObserveProposal:
		options.StepName = fmt.Sprintf("dorf/proposal-wake/v2/%s/%020d", work.Revision, sequence)
		options.Timeout = proposalPollInterval
	case coding.WorkObserveAgent:
		options.StepName = fmt.Sprintf("dorf/agent-run-wake/v1/%s/%020d", work.FactID, sequence)
		options.Timeout = activeAgentPollInterval
	}
	return options
}

func Admit(ctx context.Context, store postgres.Store, client *absurd.Client, providers ProviderChecker, profile RuntimeProfile, input postgres.NewCodingJob) (core.Job, bool, error) {
	input.NewJob.Workflow = coding.Workflow
	input.NewJob.WorkflowRevision = coding.WorkflowRevision
	return admit(ctx, store, client, providers, profile, CodingToProposalDefinition(), RunTaskName, postgres.MessageTaskKey(core.JobID(strings.TrimSpace(input.AdmissionKey))), input.NewJob, func() (core.Job, bool, error) {
		return store.AdmitCoding(ctx, input)
	})
}

func AdmitCodebaseInvestigation(ctx context.Context, store postgres.Store, client *absurd.Client, providers ProviderChecker, profile RuntimeProfile, input postgres.NewInvestigationJob) (core.Job, bool, error) {
	input.NewJob.Workflow = investigation.Workflow
	input.NewJob.WorkflowRevision = investigation.WorkflowRevision
	return admit(ctx, store, client, providers, profile, CodebaseInvestigationDefinition(), InvestigationTaskName, "codebase-investigation:v2:"+core.JobID(strings.TrimSpace(input.AdmissionKey)), input.NewJob, func() (core.Job, bool, error) {
		return store.AdmitInvestigation(ctx, input)
	})
}

func admit(ctx context.Context, store postgres.Store, client *absurd.Client, providers ProviderChecker, profile RuntimeProfile, definition Definition, taskName, taskKey string, input postgres.NewJob, persist func() (core.Job, bool, error)) (core.Job, bool, error) {
	key := strings.TrimSpace(input.AdmissionKey)
	if key != "" {
		_, err := store.Job(ctx, core.JobID(key))
		switch {
		case err == nil:
			// An idempotent retry validates the original input below without
			// depending on the Gateway still being available.
		case errors.Is(err, postgres.ErrNotFound):
			if err := profile.Require(definition.Name, definition.Revision, definition.RequiredProviderCapabilities); err != nil {
				return core.Job{}, false, err
			}
			if providers == nil {
				return core.Job{}, false, fmt.Errorf("provider readiness is not configured")
			}
			if err := providers.Check(ctx, strings.TrimSpace(input.ProviderConnection)); err != nil {
				return core.Job{}, false, fmt.Errorf("Provider Connection %q is not ready: %w", strings.TrimSpace(input.ProviderConnection), err)
			}
		default:
			return core.Job{}, false, err
		}
	}
	job, created, err := persist()
	if err != nil {
		return core.Job{}, false, err
	}
	if !job.AdmissionOpen {
		return job, created, nil
	}
	err = store.WithJobFence(ctx, job.ID, func() error {
		spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID}, absurdruntime.TaskSpawnOptions(taskKey))
		if err != nil {
			return fmt.Errorf("schedule admitted Job in Absurd: %w", err)
		}
		if err := store.AttachJobTask(ctx, job.ID, job.CurrentTaskID, spawned.TaskID, taskName); err != nil {
			return fmt.Errorf("attach Job task: %w", err)
		}
		return nil
	})
	if err != nil {
		return core.Job{}, false, err
	}
	job, err = store.Job(ctx, job.ID)
	return job, created, err
}

func AdmitMessage(ctx context.Context, store postgres.Store, client *absurd.Client, input postgres.NewMessage) (core.Message, bool, error) {
	job, err := store.Job(ctx, input.JobID)
	if err != nil {
		return core.Message{}, false, err
	}
	var message core.Message
	var created bool
	switch {
	case job.Workflow == coding.Workflow && job.WorkflowRevision == coding.WorkflowRevision:
		message, created, err = store.AdmitCodingMessage(ctx, input)
	case job.Workflow == investigation.Workflow && job.WorkflowRevision == investigation.WorkflowRevision:
		message, created, err = store.AdmitInvestigationMessage(ctx, input)
	default:
		return core.Message{}, false, fmt.Errorf("workflow %s revision %s does not accept Messages in this slice", job.Workflow, job.WorkflowRevision)
	}
	if err != nil {
		return core.Message{}, false, err
	}
	// Events carry no delivery truth. Re-emitting on an idempotent client retry
	// repairs a crash after PostgreSQL admission but before this wake hint.
	if err := client.EmitEvent(ctx, config.QueueName, WakeEvent(message.JobID, message.Sequence), WakeV1{JobID: message.JobID, Sequence: message.Sequence}); err != nil {
		return message, created, fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same from ID and input: %w", message.ID, message.Sequence, err)
	}
	return message, created, nil
}
