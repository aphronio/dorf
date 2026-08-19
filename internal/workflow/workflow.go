package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	RunTaskName             = postgres.MessageTaskName
	InvestigationTaskName   = "dorf-codebase-investigation-v1"
	CleanupTaskName         = "dorf-job-cleanup-v3"
	activeAgentPollInterval = time.Second
	retryBaseDelaySeconds   = 5
	retryBackoffFactor      = 2
	retryMaxDelaySeconds    = 60
)

type Params struct {
	JobID string `json:"job_id"`
}

// TaskResultV1 is persisted by Absurd as the stable result contract for both
// Job and cleanup tasks.
type TaskResultV1 struct {
	JobID   string `json:"job_id"`
	Outcome string `json:"outcome"`
}

// WakeV1 is persisted by Absurd under one immutable Job-local FIFO event.
type WakeV1 struct {
	JobID    string `json:"job_id"`
	Sequence int64  `json:"sequence"`
}

type ProviderChecker interface {
	Check(context.Context, string) error
}

type sandboxProfileStore interface {
	Job(context.Context, string) (spine.Job, error)
	SetWorkflowAttention(context.Context, string, string, string) error
	SetCleanupAttention(context.Context, string, string) error
}

// Runtime is the provider-neutral execution bundle for one durably selected
// Sandbox profile. Provider and Harness selection remain in the composition
// root; workflows only consume their established contracts.
type Runtime struct {
	Service  spine.Service
	Proposal ProposalRuntime
	Profile  RuntimeProfile
}

type RuntimeResolver interface {
	Resolve(context.Context, string) (Runtime, error)
}

func WakeEvent(jobID string, sequence int64) string {
	return fmt.Sprintf("dorf.job-message:%s:%020d", jobID, sequence)
}

// taskSpawnOptions keeps Dorf's bounded task retry policy explicit at the
// Absurd authority boundary. Absurd persists and applies the resulting
// schedule; Dorf does not mirror attempt timing in its own facts.
func taskSpawnOptions(idempotencyKey string) absurd.SpawnOptions {
	return absurd.SpawnOptions{
		IdempotencyKey: idempotencyKey,
		RetryStrategy: &absurd.RetryStrategy{
			Kind:        "exponential",
			BaseSeconds: retryBaseDelaySeconds,
			Factor:      retryBackoffFactor,
			MaxSeconds:  retryMaxDelaySeconds,
		},
	}
}

func Register(client *absurd.Client, store postgres.Store, runtimes RuntimeResolver) {
	client.MustRegister(absurd.Task(RunTaskName, func(ctx context.Context, params Params) (TaskResultV1, error) {
		if err := verifyAttachedTask(ctx, store, params.JobID, RunTaskName); err != nil {
			return TaskResultV1{}, err
		}
		runtime, err := runtimeForJob(ctx, store, runtimes, params.JobID, CodingToProposalDefinition(), false)
		if err != nil {
			return TaskResultV1{}, err
		}
		proposal := runtime.Proposal
		proposal.Publication = proposal.Publication.WithClaimCheck(absurdruntime.RequireClaim)
		proposal.Outcome = proposal.Outcome.WithClaimCheck(absurdruntime.RequireClaim)
		if proposal.PollInterval <= 0 {
			proposal.PollInterval = 30 * time.Second
		}
		// Sequence 1 is present before this task is spawned. Every later FIFO
		// position owns one immutable Absurd event identity, starting at 2.
		for {
			work, err := RunJob(ctx, runtime.Service, store, proposal, params.JobID)
			if err != nil {
				return TaskResultV1{}, err
			}
			if work.Kind == WorkComplete {
				outcome, err := store.Outcome(ctx, params.JobID)
				if err != nil {
					return TaskResultV1{}, err
				}
				if outcome != nil {
					task := absurd.MustTaskContext(ctx)
					if _, err := scheduleCleanup(ctx, store, client, params.JobID, task.TaskID()); err != nil {
						return TaskResultV1{}, err
					}
					return TaskResultV1{JobID: params.JobID, Outcome: string(outcome.Kind)}, nil
				}
				return TaskResultV1{JobID: params.JobID, Outcome: "admission-closed"}, nil
			}
			sequence, err := store.NextWakeSequence(ctx, params.JobID)
			if err != nil {
				return TaskResultV1{}, err
			}
			options := wakeOptions(work, sequence, proposal.PollInterval)
			wake, err := absurd.AwaitEvent[WakeV1](ctx, WakeEvent(params.JobID, sequence), options)
			if err != nil {
				var timeout *absurd.TimeoutError
				if (work.Kind == WorkObserveProposal || work.Kind == WorkObserveAgent) && errors.As(err, &timeout) {
					continue
				}
				return TaskResultV1{}, err
			}
			if wake.JobID != params.JobID || wake.Sequence != sequence {
				return TaskResultV1{}, fmt.Errorf("message wake payload conflicts with Job %s sequence %d", params.JobID, sequence)
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
	client.MustRegister(absurd.Task(InvestigationTaskName, func(ctx context.Context, params Params) (TaskResultV1, error) {
		if err := verifyAttachedTask(ctx, store, params.JobID, InvestigationTaskName); err != nil {
			return TaskResultV1{}, err
		}
		runtime, err := runtimeForJob(ctx, store, runtimes, params.JobID, CodebaseInvestigationDefinition(), false)
		if err != nil {
			return TaskResultV1{}, err
		}
		for {
			work, err := RunCodebaseInvestigation(ctx, runtime.Service, store, params.JobID)
			if err != nil {
				return TaskResultV1{}, err
			}
			if work.Kind == InvestigationWorkComplete {
				report, err := store.CodebaseInvestigationReport(ctx, params.JobID)
				if err != nil {
					return TaskResultV1{}, err
				}
				if report != nil {
					task := absurd.MustTaskContext(ctx)
					if _, err := scheduleCleanup(ctx, store, client, params.JobID, task.TaskID()); err != nil {
						return TaskResultV1{}, err
					}
					return TaskResultV1{JobID: params.JobID, Outcome: "report-recorded"}, nil
				}
				return TaskResultV1{JobID: params.JobID, Outcome: "admission-closed"}, nil
			}
			sequence, err := store.NextWakeSequence(ctx, params.JobID)
			if err != nil {
				return TaskResultV1{}, err
			}
			options := absurd.AwaitEventOptions{StepName: fmt.Sprintf("dorf/investigation-wake/v1/%020d", sequence)}
			if work.Kind == InvestigationWorkObserveAgent {
				options.StepName = fmt.Sprintf("dorf/investigation-agent-wake/v1/%s/%020d", work.FactID, sequence)
				options.Timeout = activeAgentPollInterval
			}
			wake, err := absurd.AwaitEvent[WakeV1](ctx, WakeEvent(params.JobID, sequence), options)
			if err != nil {
				var timeout *absurd.TimeoutError
				if work.Kind == InvestigationWorkObserveAgent && errors.As(err, &timeout) {
					continue
				}
				return TaskResultV1{}, err
			}
			if wake.JobID != params.JobID || wake.Sequence != sequence {
				return TaskResultV1{}, fmt.Errorf("message wake payload conflicts with Job %s sequence %d", params.JobID, sequence)
			}
		}
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
	client.MustRegister(absurd.Task(CleanupTaskName, func(ctx context.Context, params Params) (TaskResultV1, error) {
		if err := verifyAttachedTask(ctx, store, params.JobID, CleanupTaskName); err != nil {
			return TaskResultV1{}, err
		}
		job, err := store.Job(ctx, params.JobID)
		if err != nil {
			return TaskResultV1{}, err
		}
		definition, err := definitionForJob(job)
		if err != nil {
			return TaskResultV1{}, err
		}
		runtime, err := runtimeForLoadedJob(ctx, store, runtimes, job, definition, true)
		if err != nil {
			return TaskResultV1{}, err
		}
		return absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (TaskResultV1, error) {
			if err := runCleanup(workCtx, runtime.Service, store, params.JobID); err != nil {
				return TaskResultV1{}, err
			}
			return TaskResultV1{JobID: params.JobID, Outcome: "cleanup-complete"}, nil
		})
	}, absurd.TaskOptions{DefaultMaxAttempts: 5}))
}

func runtimeForJob(ctx context.Context, store sandboxProfileStore, runtimes RuntimeResolver, jobID string, expected Definition, cleanup bool) (Runtime, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return Runtime{}, err
	}
	return runtimeForLoadedJob(ctx, store, runtimes, job, expected, cleanup)
}

func runtimeForLoadedJob(ctx context.Context, store sandboxProfileStore, runtimes RuntimeResolver, job spine.Job, expected Definition, cleanup bool) (Runtime, error) {
	jobID := job.ID
	if job.Workflow != expected.Name || job.WorkflowRevision != expected.Revision {
		detail := fmt.Sprintf("Job requires workflow %s revision %s, but task executes %s revision %s", job.Workflow, job.WorkflowRevision, expected.Name, expected.Revision)
		var attentionErr error
		if cleanup {
			attentionErr = store.SetCleanupAttention(ctx, jobID, detail)
		} else {
			attentionErr = store.SetWorkflowAttention(ctx, jobID, "workflow-profile", detail)
		}
		if attentionErr != nil {
			return Runtime{}, fmt.Errorf("%s; record workflow mismatch attention: %w", detail, attentionErr)
		}
		return Runtime{}, errors.New(detail)
	}
	if runtimes == nil {
		return Runtime{}, fmt.Errorf("Sandbox runtime resolution is not configured")
	}
	runtime, err := runtimes.Resolve(ctx, job.SandboxProfile)
	if err != nil {
		return Runtime{}, fmt.Errorf("resolve Sandbox profile %q: %w", job.SandboxProfile, err)
	}
	if err := requireJobProfile(ctx, store, job, runtime.Profile, expected, cleanup); err != nil {
		return Runtime{}, err
	}
	return runtime, nil
}

func requireJobProfile(ctx context.Context, store sandboxProfileStore, job spine.Job, profile RuntimeProfile, expected Definition, cleanup bool) error {
	configured := strings.TrimSpace(profile.SandboxProfile)
	if job.SandboxProfile != configured {
		detail := fmt.Sprintf("Job requires Sandbox profile %q, but this worker resolved %q", job.SandboxProfile, configured)
		var attentionErr error
		if cleanup {
			attentionErr = store.SetCleanupAttention(ctx, job.ID, detail)
		} else {
			attentionErr = store.SetWorkflowAttention(ctx, job.ID, "sandbox-profile", detail)
		}
		if attentionErr != nil {
			return fmt.Errorf("%s; record profile mismatch attention: %w", detail, attentionErr)
		}
		return errors.New(detail)
	}
	if err := profile.Require(expected); err != nil {
		detail := err.Error()
		var attentionErr error
		if cleanup {
			attentionErr = store.SetCleanupAttention(ctx, job.ID, detail)
		} else {
			attentionErr = store.SetWorkflowAttention(ctx, job.ID, "provider-capabilities", detail)
		}
		if attentionErr != nil {
			return fmt.Errorf("%s; record provider capability attention: %w", detail, attentionErr)
		}
		return errors.New(detail)
	}
	return nil
}

func wakeOptions(work Work, sequence int64, proposalPollInterval time.Duration) absurd.AwaitEventOptions {
	options := absurd.AwaitEventOptions{StepName: fmt.Sprintf("dorf/message-wake/v1/%020d", sequence)}
	switch work.Kind {
	case WorkObserveProposal:
		options.StepName = fmt.Sprintf("dorf/proposal-wake/v2/%s/%020d", work.Revision, sequence)
		options.Timeout = proposalPollInterval
	case WorkObserveAgent:
		options.StepName = fmt.Sprintf("dorf/agent-run-wake/v1/%s/%020d", work.FactID, sequence)
		options.Timeout = activeAgentPollInterval
	}
	return options
}

func observeProposal(ctx context.Context, proposal ProposalRuntime, jobID, revision string) (ProposalObservationResultV1, error) {
	result, err := absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (ProposalObservationResultV1, error) {
		return proposal.Observe(workCtx, jobID, revision)
	})
	if err != nil {
		return ProposalObservationResultV1{}, err
	}
	if result.Revision != revision {
		return ProposalObservationResultV1{}, fmt.Errorf("proposal observation conflicts with Revision %s", revision)
	}
	return result, nil
}

func Admit(ctx context.Context, store postgres.Store, client *absurd.Client, providers ProviderChecker, profile RuntimeProfile, input postgres.NewJob) (spine.Job, bool, error) {
	input.Workflow = spine.WorkflowCodingToProposal
	input.WorkflowRevision = spine.CodingToProposalRevision
	return admit(ctx, store, client, providers, profile, CodingToProposalDefinition(), RunTaskName, postgres.MessageTaskKey(spine.JobID(strings.TrimSpace(input.AdmissionKey))), input)
}

func AdmitCodebaseInvestigation(ctx context.Context, store postgres.Store, client *absurd.Client, providers ProviderChecker, profile RuntimeProfile, input postgres.NewJob) (spine.Job, bool, error) {
	input.Workflow = spine.WorkflowCodebaseInvestigation
	input.WorkflowRevision = spine.CodebaseInvestigationRevision
	return admit(ctx, store, client, providers, profile, CodebaseInvestigationDefinition(), InvestigationTaskName, "codebase-investigation:v1:"+spine.JobID(strings.TrimSpace(input.AdmissionKey)), input)
}

func admit(ctx context.Context, store postgres.Store, client *absurd.Client, providers ProviderChecker, profile RuntimeProfile, definition Definition, taskName, taskKey string, input postgres.NewJob) (spine.Job, bool, error) {
	key := strings.TrimSpace(input.AdmissionKey)
	if key != "" {
		_, err := store.Job(ctx, spine.JobID(key))
		switch {
		case err == nil:
			// An idempotent retry validates the original input below without
			// depending on the Gateway still being available.
		case errors.Is(err, postgres.ErrNotFound):
			if err := profile.Require(definition); err != nil {
				return spine.Job{}, false, err
			}
			if providers == nil {
				return spine.Job{}, false, fmt.Errorf("provider readiness is not configured")
			}
			if err := providers.Check(ctx, strings.TrimSpace(input.ProviderConnection)); err != nil {
				return spine.Job{}, false, fmt.Errorf("Provider Connection %q is not ready: %w", strings.TrimSpace(input.ProviderConnection), err)
			}
		default:
			return spine.Job{}, false, err
		}
	}
	job, created, err := store.Admit(ctx, input)
	if err != nil {
		return spine.Job{}, false, err
	}
	if !job.AdmissionOpen {
		return job, created, nil
	}
	err = store.WithJobFence(ctx, job.ID, func() error {
		spawned, err := client.Spawn(ctx, taskName, Params{JobID: job.ID}, taskSpawnOptions(taskKey))
		if err != nil {
			return fmt.Errorf("schedule admitted Job in Absurd: %w", err)
		}
		if err := store.AttachMessageTask(ctx, job.ID, spawned.TaskID); err != nil {
			return fmt.Errorf("attach Job message task: %w", err)
		}
		return nil
	})
	if err != nil {
		return spine.Job{}, false, err
	}
	job, err = store.Job(ctx, job.ID)
	return job, created, err
}

func AdmitMessage(ctx context.Context, store postgres.Store, client *absurd.Client, input postgres.NewMessage) (spine.Message, bool, error) {
	message, created, err := store.AdmitMessage(ctx, input)
	if err != nil {
		return spine.Message{}, false, err
	}
	// Events carry no delivery truth. Re-emitting on an idempotent client retry
	// repairs a crash after PostgreSQL admission but before this wake hint.
	if err := client.EmitEvent(ctx, config.QueueName, WakeEvent(message.JobID, message.Sequence), WakeV1{JobID: message.JobID, Sequence: message.Sequence}); err != nil {
		return message, created, fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same from ID and input: %w", message.ID, message.Sequence, err)
	}
	return message, created, nil
}

// RetrySetup atomically records a new setup Action generation and its FIFO
// wake, then emits the recoverable Absurd event hint.
func RetrySetup(ctx context.Context, store postgres.Store, client *absurd.Client, jobID, retryID, input string) (spine.Action, spine.Message, bool, error) {
	action, message, created, err := store.RetrySetup(ctx, jobID, retryID, input)
	if err != nil {
		return action, message, created, err
	}
	if action.State == spine.ActionFailed {
		return action, message, false, nil
	}
	// Events carry no delivery truth. Re-emission is the recovery path for a
	// crash after the Action/message transaction and before this wake hint.
	if err := client.EmitEvent(ctx, config.QueueName, WakeEvent(message.JobID, message.Sequence), WakeV1{JobID: message.JobID, Sequence: message.Sequence}); err != nil {
		return action, message, created, fmt.Errorf("setup retry message %s sequence %d was accepted, but its wake hint failed; retry the same setup identity and input: %w", message.ID, message.Sequence, err)
	}
	return action, message, created, nil
}

func ScheduleCleanup(ctx context.Context, store postgres.Store, client *absurd.Client, jobID string) (spine.Job, error) {
	return scheduleCleanup(ctx, store, client, jobID, "")
}

func scheduleCleanup(ctx context.Context, store postgres.Store, client *absurd.Client, jobID, skipTaskID string) (spine.Job, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return spine.Job{}, err
	}
	if job.CleanupState == spine.CleanupComplete {
		return job, nil
	}
	// Publication and cleanup both lock the Job before claiming authority.
	// The winner makes the loser fail without leaving a half-owned workflow.
	if err := store.CloseAdmissionForCleanup(ctx, jobID); err != nil {
		return spine.Job{}, err
	}
	// Close admission before taking the long Job effect fence, then cancel
	// through Absurd's public API. A running handler observes cancellation at
	// its heartbeat and cancels the opaque child context; the Job fence still
	// prevents cleanup from overtaking any late external effect.
	// Reload after admission closes so cancellation uses the current durable
	// task binding rather than the earlier snapshot.
	job, err = store.Job(ctx, jobID)
	if err != nil {
		return spine.Job{}, err
	}
	if err := cancelAttachedTasks(ctx, client, job, skipTaskID); err != nil {
		return spine.Job{}, err
	}
	var result spine.Job
	err = store.WithJobFence(ctx, jobID, func() error {
		current, err := store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		// Recheck the binding under the Job fence before cleanup becomes eligible.
		if err := cancelAttachedTasks(ctx, client, current, skipTaskID); err != nil {
			return err
		}
		spawned, err := client.Spawn(ctx, CleanupTaskName, Params{JobID: jobID}, taskSpawnOptions("cleanup:v3:"+jobID))
		if err != nil {
			return fmt.Errorf("schedule cleanup in Absurd: %w", err)
		}
		if err := store.SetCleanupTaskID(ctx, jobID, spawned.TaskID); err != nil {
			return err
		}
		result, err = store.Job(ctx, jobID)
		return err
	})
	return result, err
}

func cancelAttachedTasks(ctx context.Context, client *absurd.Client, job spine.Job, skipTaskID string) error {
	for _, taskID := range []string{job.TaskID} {
		if taskID == "" || taskID == skipTaskID {
			continue
		}
		if err := client.CancelTask(ctx, client.QueueName(), taskID); err != nil {
			return fmt.Errorf("cancel attached Absurd task %s: %w", taskID, err)
		}
		snapshot, err := client.FetchTaskResult(ctx, client.QueueName(), taskID)
		if err != nil {
			return err
		}
		if snapshot == nil || !snapshot.IsTerminal() {
			return fmt.Errorf("attached Absurd task %s did not reach a public terminal result", taskID)
		}
	}
	return nil
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

func verifyAttachedTask(ctx context.Context, store postgres.Store, jobID, taskName string) error {
	return store.WithJobFence(ctx, jobID, func() error {
		job, err := store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		attachedID := job.TaskID
		if taskName == CleanupTaskName {
			attachedID = job.CleanupTaskID
		}
		if attachedID == "" {
			task, ok := absurd.TaskFromContext(ctx)
			if !ok {
				return absurd.ErrNoTaskContext
			}
			if taskName == CleanupTaskName {
				err = store.SetCleanupTaskID(ctx, jobID, task.TaskID())
			} else {
				err = store.AttachMessageTask(ctx, jobID, task.TaskID())
			}
			if err != nil {
				return fmt.Errorf("recover public Spawn attachment for %s: %w", taskName, err)
			}
			attachedID = task.TaskID()
		}
		return verifyTaskContext(ctx, attachedID, taskName)
	})
}
