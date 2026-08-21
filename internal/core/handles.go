package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	id string
}

func (h JobHandle) ID() string { return h.id }

func (h SandboxHandle) ID() string { return h.id }

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
	handle := SandboxHandle{id: owned.ID}
	if action.State == ActionSucceeded {
		return handle, nil
	}
	if err := h.executeSandboxEnsure(ctx, job, owned, action); err != nil {
		return SandboxHandle{}, err
	}
	return handle, nil
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
