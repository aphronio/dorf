package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// JobHandle is an opaque, immutable binding to one durable Job identity.
type JobHandle struct {
	id          string
	application *Application
}

// SandboxHandle is an opaque, immutable binding to one exact Job-owned
// Sandbox. Provider ownership material is never exposed through this handle.
type SandboxHandle struct {
	id          string
	jobID       string
	application *Application
}

// AgentHandle is a convenience binding to the profile-selected Harness in one
// exact Job-owned Sandbox. It creates no durable Agent identity.
type AgentHandle struct {
	jobID       string
	sandboxID   string
	application *Application
}

// MessageReceipt is immutable acknowledgement of one durable Message
// admission. Delivery and AgentRun reconciliation continue asynchronously on
// the Job's attached Absurd task.
type MessageReceipt struct {
	MessageID    string
	JobID        string
	SandboxID    string
	Sequence     int64
	Intent       MessageDeliveryIntent
	TargetTurnID string
	AdmittedAt   time.Time
	Created      bool
}

type MessageOption struct {
	intent MessageDeliveryIntent
}

// Steer explicitly prioritizes the Message against active work. Omitting it
// admits an ordinary FIFO follow.
func Steer() MessageOption { return MessageOption{intent: MessageSteer} }

func (h JobHandle) ID() string { return h.id }

func (h SandboxHandle) ID() string { return h.id }

func (h SandboxHandle) Agent() AgentHandle {
	return AgentHandle{jobID: h.jobID, sandboxID: h.id, application: h.application}
}

func (a Application) OpenJob(ctx context.Context, id string) (JobHandle, error) {
	id = strings.TrimSpace(id)
	job, err := a.Store.Job(ctx, id)
	if err != nil {
		return JobHandle{}, err
	}
	return a.jobHandle(job.ID), nil
}

func (a Application) jobHandle(id string) JobHandle {
	return JobHandle{id: id, application: &a}
}

func (h JobHandle) EnsureDefaultSandbox(ctx context.Context) (SandboxHandle, error) {
	return h.ensureSandbox(ctx, DefaultSandbox)
}

func (h JobHandle) EnsureNamedSandbox(ctx context.Context, name string) (SandboxHandle, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == DefaultSandbox {
		return SandboxHandle{}, fmt.Errorf("named Sandbox requires a nonempty name other than %q", DefaultSandbox)
	}
	return h.ensureSandbox(ctx, name)
}

// DefaultSandbox returns the already-owned default Sandbox without creating
// infrastructure. Message callers use this read-only acquisition path outside
// an Absurd task claim.
func (h JobHandle) DefaultSandbox(ctx context.Context) (SandboxHandle, error) {
	return h.Sandbox(ctx, MainSandboxName(h.id))
}

// Sandbox returns one already-owned exact Sandbox without exposing provider
// custody. It is the read-only bridge from a workflow-selected Message fact to
// the Sandbox-bound Agent convenience handle.
func (h JobHandle) Sandbox(ctx context.Context, id string) (SandboxHandle, error) {
	if h.application == nil || h.application.Store == nil || h.id == "" || strings.TrimSpace(id) == "" {
		return SandboxHandle{}, fmt.Errorf("Job handle is not bound to Core")
	}
	owned, err := h.application.Store.Sandbox(ctx, id)
	if err != nil {
		return SandboxHandle{}, err
	}
	if owned.JobID != h.id {
		return SandboxHandle{}, fmt.Errorf("Sandbox %s does not belong to Job %s", owned.ID, h.id)
	}
	return h.sandboxHandle(owned.ID), nil
}

func (h JobHandle) sandboxHandle(id string) SandboxHandle {
	return SandboxHandle{id: id, jobID: h.id, application: h.application}
}

func (h JobHandle) ensureSandbox(ctx context.Context, name string) (SandboxHandle, error) {
	if h.application == nil || h.application.Store == nil || h.id == "" {
		return SandboxHandle{}, fmt.Errorf("Job handle is not bound to Core")
	}
	task, claimed := absurd.TaskFromContext(ctx)
	if !claimed {
		return SandboxHandle{}, absurd.ErrNoTaskContext
	}
	if err := h.application.verifyCurrentTask(ctx, h.id, task.TaskName()); err != nil {
		return SandboxHandle{}, fmt.Errorf("verify attached task before ensuring Sandbox: %w", err)
	}

	var job Job
	var owned Sandbox
	var action Action
	err := h.application.Store.WithJobFence(ctx, h.id, func() error {
		var err error
		job, err = h.application.Store.Job(ctx, h.id)
		if err != nil {
			return err
		}
		if !job.AdmissionOpen || job.CleanupState != CleanupPending {
			return fmt.Errorf("Job %s cannot ensure Sandbox %q after cleanup begins", h.id, name)
		}
		owned, err = h.application.Store.EnsureSandbox(ctx, h.id, name)
		if err != nil {
			return err
		}
		action, err = h.application.Store.GetOrCreateSandboxAction(ctx, owned.ID, ActionSandboxCreate)
		return err
	})
	if err != nil {
		return SandboxHandle{}, err
	}
	handle := h.sandboxHandle(owned.ID)
	if action.State == ActionSucceeded {
		return handle, nil
	}
	if err := h.executeSandboxEnsure(ctx, job, owned, action); err != nil {
		return SandboxHandle{}, err
	}
	return handle, nil
}

// Message admits one durable human Message through the exact bound Sandbox.
// Module authorization remains in the supplied transaction; Core owns the
// caller-retained key, default-follow option semantics, receipt, and wake.
func (h AgentHandle) Message(ctx context.Context, key, input string, options ...MessageOption) (MessageReceipt, error) {
	if h.application == nil || h.application.Store == nil || h.jobID == "" || h.sandboxID == "" {
		return MessageReceipt{}, fmt.Errorf("Agent handle is not bound to a Job Sandbox")
	}
	if h.application.AgentMessages == nil {
		return MessageReceipt{}, fmt.Errorf("Agent Message authorization is not configured")
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.TrimSpace(input) == "" {
		return MessageReceipt{}, fmt.Errorf("Agent Message requires a caller-retained send key and complete text")
	}
	if len(key) > 256 {
		return MessageReceipt{}, fmt.Errorf("Agent Message send key must be at most 256 characters")
	}
	if len(input) > 1<<20 {
		return MessageReceipt{}, fmt.Errorf("Agent Message text exceeds 1 MiB")
	}
	intent := MessageFollow
	if len(options) > 1 {
		return MessageReceipt{}, fmt.Errorf("Agent Message accepts at most one delivery option")
	}
	if len(options) == 1 {
		intent = options[0].intent
		if intent != MessageSteer {
			return MessageReceipt{}, fmt.Errorf("unsupported Agent Message delivery option")
		}
	}
	admitted, err := h.application.AgentMessages.AdmitAgentMessage(ctx, MessageAdmission{
		JobID: h.jobID, SandboxID: h.sandboxID, FromKind: MessageFromHuman,
		FromID: key, Input: input, Intent: intent,
	})
	message := admitted.Message
	receipt := MessageReceipt{
		MessageID: message.ID, JobID: message.JobID, SandboxID: admitted.SandboxID, Sequence: message.Sequence,
		Intent: message.Intent, TargetTurnID: message.TargetTurnID,
		AdmittedAt: message.AdmittedAt, Created: admitted.Created,
	}
	if err != nil {
		return receipt, err
	}
	expectedID := MessageID(h.jobID, MessageFromHuman, key)
	targetValid := message.Intent == MessageFollow && message.TargetTurnID == "" || message.Intent == MessageSteer && message.TargetTurnID != ""
	if message.ID != expectedID || message.JobID != h.jobID || admitted.SandboxID != h.sandboxID || message.FromKind != MessageFromHuman || message.FromID != key ||
		message.Input != input || message.Sequence <= 0 || message.Intent != intent || !targetValid {
		return MessageReceipt{}, fmt.Errorf("Agent Message admission returned a foreign receipt")
	}
	if h.application.Tasks == nil {
		return receipt, fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same send key and text: Absurd is not configured", message.ID, message.Sequence)
	}
	if err := h.application.EmitMessageWake(ctx, message); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// Reconcile advances one already-admitted Message through the Harness using
// only stable Message and exact Sandbox identities. It performs one bounded
// recovery cycle and never changes Message admission.
func (h AgentHandle) Reconcile(ctx context.Context, messageID string) (MessageResult, error) {
	if h.application == nil || h.application.Store == nil || h.jobID == "" || h.sandboxID == "" {
		return MessageResult{}, fmt.Errorf("Agent handle is not bound to a Job Sandbox")
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return MessageResult{}, fmt.Errorf("Agent reconciliation requires a durable Message identity")
	}
	if h.application.SandboxRuntimes == nil {
		return MessageResult{}, fmt.Errorf("Sandbox runtime resolution is not configured")
	}
	job, err := h.application.Store.Job(ctx, h.jobID)
	if err != nil {
		return MessageResult{}, err
	}
	runtime, err := h.application.SandboxRuntimes.ResolveSandbox(ctx, job.SandboxProfile)
	if err != nil {
		return MessageResult{}, fmt.Errorf("resolve Sandbox runtime for profile %q: %w", job.SandboxProfile, err)
	}
	if runtime.Execution == nil || strings.TrimSpace(runtime.SandboxProfile) != job.SandboxProfile {
		return MessageResult{}, fmt.Errorf("Sandbox runtime does not match Job profile %q", job.SandboxProfile)
	}
	return runtime.Execution.ReconcileAgentMessage(ctx, h.jobID, messageID, h.sandboxID)
}

func (h JobHandle) executeSandboxEnsure(ctx context.Context, job Job, owned Sandbox, action Action) error {
	if h.application.SandboxRuntimes == nil {
		return fmt.Errorf("Sandbox runtime resolution is not configured")
	}
	runtime, err := h.application.SandboxRuntimes.ResolveSandbox(ctx, job.SandboxProfile)
	if err != nil {
		return fmt.Errorf("resolve Sandbox profile %q: %w", job.SandboxProfile, err)
	}
	if strings.TrimSpace(runtime.SandboxProfile) != job.SandboxProfile || runtime.Execution == nil {
		return fmt.Errorf("Sandbox runtime does not match Job profile %q", job.SandboxProfile)
	}
	err = absurdruntime.RunActionStep(ctx, action.ID, func(workCtx context.Context) error {
		return runtime.Execution.ExecuteSandboxAction(workCtx, job.ID, action.ID)
	})
	if err == nil || !provider.IsArtifactUnavailable(err) {
		return err
	}
	attentionErr := h.application.Store.RecordSandboxProfileUnavailable(ctx, job.ID, job.SandboxProfile, action.ID, err)
	if attentionErr != nil {
		return errors.Join(err, fmt.Errorf("record unavailable Sandbox profile %q: %w", job.SandboxProfile, attentionErr))
	}
	return err
}

func (h JobHandle) RequestCleanup(ctx context.Context) error {
	if h.application == nil || h.application.Store == nil || h.id == "" {
		return fmt.Errorf("Job handle is not bound to Core")
	}
	_, err := h.application.requestCleanup(ctx, h.id)
	return err
}
