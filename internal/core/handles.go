package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// SessionHandle is an opaque, immutable binding to one durable Session identity.
type SessionHandle struct {
	id          string
	application *Application
}

// SandboxHandle is an opaque, immutable binding to one exact Session-owned
// Sandbox. Provider ownership material is never exposed through this handle.
type SandboxHandle struct {
	id          string
	sessionID   string
	application *Application
}

func (h SessionHandle) ID() string { return h.id }

func (h SandboxHandle) ID() string { return h.id }

func (a Application) OpenSession(ctx context.Context, id string) (SessionHandle, error) {
	id = strings.TrimSpace(id)
	session, err := a.Store.Session(ctx, id)
	if err != nil {
		return SessionHandle{}, err
	}
	return a.sessionHandle(session.ID), nil
}

func (a Application) sessionHandle(id string) SessionHandle {
	return SessionHandle{id: id, application: &a}
}

// DefaultSandbox returns the already-owned default Sandbox without creating
// infrastructure. Callers use this read-only acquisition path outside
// an Absurd task claim.
func (h SessionHandle) DefaultSandbox(ctx context.Context) (SandboxHandle, error) {
	return h.Sandbox(ctx, MainSandboxName(h.id))
}

// Sandbox returns one already-owned exact Sandbox without exposing provider
// custody.
func (h SessionHandle) Sandbox(ctx context.Context, id string) (SandboxHandle, error) {
	if h.application == nil || h.application.Store == nil || h.id == "" || strings.TrimSpace(id) == "" {
		return SandboxHandle{}, fmt.Errorf("Session handle is not bound to Core")
	}
	owned, err := h.application.Store.Sandbox(ctx, id)
	if err != nil {
		return SandboxHandle{}, err
	}
	if owned.SessionID != h.id {
		return SandboxHandle{}, fmt.Errorf("Sandbox %s does not belong to Session %s", owned.ID, h.id)
	}
	return h.sandboxHandle(owned.ID), nil
}

func (h SessionHandle) sandboxHandle(id string) SandboxHandle {
	return SandboxHandle{id: id, sessionID: h.id, application: h.application}
}

func (h SessionHandle) EnsureDefaultSandbox(ctx context.Context) (SandboxHandle, error) {
	if h.application == nil || h.application.Store == nil || h.id == "" {
		return SandboxHandle{}, fmt.Errorf("Session handle is not bound to Core")
	}
	task, claimed := absurd.TaskFromContext(ctx)
	if !claimed {
		return SandboxHandle{}, absurd.ErrNoTaskContext
	}
	if err := h.application.verifyCurrentTask(ctx, h.id, task.TaskName()); err != nil {
		return SandboxHandle{}, fmt.Errorf("verify attached task before ensuring Sandbox: %w", err)
	}

	var session Session
	var owned Sandbox
	err := h.application.Store.WithSessionFence(ctx, h.id, func() error {
		var err error
		session, err = h.application.Store.Session(ctx, h.id)
		if err != nil {
			return err
		}
		if !session.AdmissionOpen || session.CleanupState != CleanupPending {
			return fmt.Errorf("Session %s cannot ensure Sandbox after cleanup begins", h.id)
		}
		owned, err = h.application.Store.Sandbox(ctx, MainSandboxName(h.id))
		if err != nil {
			return err
		}
		if owned.ID != MainSandboxName(h.id) || owned.SessionID != h.id || owned.Name != DefaultSandbox {
			return fmt.Errorf("Session %s has a foreign default Sandbox reservation", h.id)
		}
		return nil
	})
	if err != nil {
		return SandboxHandle{}, err
	}
	handle := h.sandboxHandle(owned.ID)
	if err := h.executeSandboxEnsure(ctx, session, owned); err != nil {
		return SandboxHandle{}, err
	}
	return handle, nil
}

func (h SessionHandle) executeSandboxEnsure(ctx context.Context, session Session, owned Sandbox) error {
	if h.application.SandboxRuntimes == nil {
		return fmt.Errorf("Sandbox runtime resolution is not configured")
	}
	runtime, err := h.application.SandboxRuntimes.ResolveSandbox(ctx, session.ProfileRef())
	if err != nil {
		return fmt.Errorf("resolve Sandbox profile %q: %w", session.SandboxProfile, err)
	}
	if runtime.SandboxProfile != session.ProfileRef() || runtime.Execution == nil {
		return fmt.Errorf("Sandbox runtime does not match Session profile %q", session.SandboxProfile)
	}
	actionID := ScopedActionID(session.ID, ActionSandboxCreate, owned.ID)
	err = runtime.Execution.ExecuteSandboxAction(ctx, session.ID, owned.ID, ActionSandboxCreate)
	if err == nil || !provider.IsArtifactUnavailable(err) {
		return err
	}
	attentionErr := h.application.Store.RecordSandboxProfileUnavailable(ctx, session.ID, session.SandboxProfile, actionID, err)
	if attentionErr != nil {
		return errors.Join(err, fmt.Errorf("record unavailable Sandbox profile %q: %w", session.SandboxProfile, attentionErr))
	}
	return err
}

func (h SessionHandle) RequestCleanup(ctx context.Context) error {
	if h.application == nil || h.application.Store == nil || h.id == "" {
		return fmt.Errorf("Session handle is not bound to Core")
	}
	_, err := h.application.requestCleanup(ctx, h.id)
	return err
}
