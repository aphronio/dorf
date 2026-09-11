package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	intent        MessageDeliveryIntent
	refreshSkills bool
}

// RefreshSkills requests a reload before the next eligible fresh Turn.
func RefreshSkills() MessageOption { return MessageOption{refreshSkills: true} }

// Steer explicitly prioritizes the Message against active work. Omitting it
// admits an ordinary FIFO follow.
func Steer() MessageOption { return MessageOption{intent: MessageSteer} }

// PreferSteer chooses Steer at admission when an active Turn exists; otherwise
// it admits a Follow. The resolved intent and target never change afterward.
func PreferSteer() MessageOption { return MessageOption{intent: MessageAuto} }

func (h JobHandle) ID() string { return h.id }

func (h SandboxHandle) ID() string { return h.id }

func (h SandboxHandle) Agent() AgentHandle {
	return AgentHandle{jobID: h.jobID, sandboxID: h.id, application: h.application}
}

// ReadFile returns the exact bytes of one regular Sandbox file
// while holding the Job resource fence. Core does not discover, interpret, or
// retain the file; callers must read what they need before requesting cleanup.
func (h SandboxHandle) ReadFile(ctx context.Context, relativePath string) ([]byte, error) {
	if h.application == nil || h.application.Store == nil || h.application.SandboxRuntimes == nil || h.jobID == "" || h.id == "" {
		return nil, fmt.Errorf("Sandbox handle is not bound to Core file access")
	}
	if err := provider.ValidateFilePath(relativePath); err != nil {
		return nil, err
	}
	var contents []byte
	err := h.application.Store.WithJobFence(ctx, h.jobID, func() error {
		job, err := h.application.Store.Job(ctx, h.jobID)
		if err != nil {
			return err
		}
		if job.CleanupState != CleanupPending {
			return fmt.Errorf("%w for Job %s", ErrSandboxFileCleanupFenced, job.ID)
		}
		owned, err := h.application.Store.Sandbox(ctx, h.id)
		if err != nil {
			return err
		}
		if owned.JobID != job.ID || owned.ID != h.id {
			return fmt.Errorf("Sandbox %s does not belong to Job %s", h.id, job.ID)
		}
		runtime, err := h.application.SandboxRuntimes.ResolveSandbox(ctx, job.SandboxProfile)
		if err != nil {
			return fmt.Errorf("resolve Sandbox profile %q for file read: %w", job.SandboxProfile, err)
		}
		if runtime.SandboxProfile != job.SandboxProfile || runtime.Files == nil {
			return fmt.Errorf("Sandbox runtime does not provide file access for Job profile %q", job.SandboxProfile)
		}
		contents, err = runtime.Files.ReadSandboxFile(ctx, job, owned, relativePath)
		return err
	})
	return contents, err
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
		return nil
	})
	if err != nil {
		return SandboxHandle{}, err
	}
	handle := h.sandboxHandle(owned.ID)
	if err := h.executeSandboxEnsure(ctx, job, owned); err != nil {
		return SandboxHandle{}, err
	}
	return handle, nil
}

// Message admits one durable human Message through the exact bound Sandbox.
// Its consumer resolves the execution envelope in the supplied transaction; Core owns the
// caller-retained key, default-follow option semantics, receipt, and wake.
func (h AgentHandle) Message(ctx context.Context, key string, input MessageInput, options ...MessageOption) (MessageReceipt, error) {
	if h.application == nil || h.application.Store == nil || h.jobID == "" || h.sandboxID == "" {
		return MessageReceipt{}, fmt.Errorf("Agent handle is not bound to a Job Sandbox")
	}
	if h.application.AgentMessages == nil {
		return MessageReceipt{}, fmt.Errorf("Agent Message execution-envelope resolution is not configured")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return MessageReceipt{}, fmt.Errorf("Agent Message requires a caller-retained send key and text or attachments")
	}
	if len(key) > 256 {
		return MessageReceipt{}, fmt.Errorf("Agent Message send key must be at most 256 characters")
	}
	if !ValidMessageInput(input) {
		return MessageReceipt{}, fmt.Errorf("Agent Message requires valid text or attachments within the accepted limits")
	}
	intent, refreshSkills, err := resolveMessageOptions(options)
	if err != nil {
		return MessageReceipt{}, err
	}
	request := MessageAdmission{
		JobID: h.jobID, SandboxID: h.sandboxID, FromKind: MessageFromHuman,
		FromID: key, Input: input.Text, Attachments: append([]MessageAttachment(nil), input.Attachments...), Intent: intent,
		RefreshSkills: refreshSkills,
	}
	return h.admitMessage(ctx, key, request)
}

func resolveMessageOptions(options []MessageOption) (MessageDeliveryIntent, bool, error) {
	intent, refreshSkills := MessageFollow, false
	for _, option := range options {
		if option.refreshSkills {
			refreshSkills = true
			continue
		}
		if intent != MessageFollow {
			return "", false, fmt.Errorf("Agent Message accepts at most one delivery option")
		}
		switch option.intent {
		case MessageSteer, MessageAuto:
			intent = option.intent
		default:
			return "", false, fmt.Errorf("unsupported Agent Message delivery option")
		}
	}
	return intent, refreshSkills, nil
}

func (h AgentHandle) admitMessage(ctx context.Context, key string, request MessageAdmission) (MessageReceipt, error) {
	admitted, err := h.application.AgentMessages.AdmitAgentMessage(ctx, request)
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
	accepted := MessageAdmission{
		JobID: message.JobID, SandboxID: admitted.SandboxID, FromKind: message.FromKind, FromID: message.FromID,
		Input: message.Input, Attachments: message.Attachments, Intent: request.Intent, RefreshSkills: message.RefreshSkills,
	}
	if !sameMessageAdmission(accepted, request) || message.ID != expectedID || message.Sequence <= 0 || !request.Intent.accepts(message.Intent) || !targetValid {
		return MessageReceipt{}, fmt.Errorf("Agent Message admission returned a foreign receipt")
	}
	if !admitted.Created {
		job, err := h.application.Store.Job(ctx, h.jobID)
		if err != nil {
			return receipt, fmt.Errorf("load Job after Message replay: %w", err)
		}
		if job.CleanupState != CleanupPending {
			return receipt, nil
		}
	}
	if h.application.Tasks == nil {
		return receipt, fmt.Errorf("message %s sequence %d was accepted, but its wake hint failed; retry the same send key and text: Absurd is not configured", message.ID, message.Sequence)
	}
	if err := h.application.EmitMessageWake(ctx, message); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func sameMessageAdmission(left, right MessageAdmission) bool {
	if left.RefreshSkills != right.RefreshSkills || left.JobID != right.JobID || left.SandboxID != right.SandboxID ||
		left.FromKind != right.FromKind || left.FromID != right.FromID || left.Input != right.Input || left.Intent != right.Intent ||
		len(left.Attachments) != len(right.Attachments) {
		return false
	}
	for index := range left.Attachments {
		if left.Attachments[index] != right.Attachments[index] {
			return false
		}
	}
	return true
}

func (h JobHandle) executeSandboxEnsure(ctx context.Context, job Job, owned Sandbox) error {
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
	actionID := ScopedActionID(job.ID, ActionSandboxCreate, owned.ID)
	err = runtime.Execution.ExecuteSandboxAction(ctx, job.ID, owned.ID, ActionSandboxCreate)
	if err == nil || !provider.IsArtifactUnavailable(err) {
		return err
	}
	attentionErr := h.application.Store.RecordSandboxProfileUnavailable(ctx, job.ID, job.SandboxProfile, actionID, err)
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
