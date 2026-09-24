package controlapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"net/http"
	"time"
)

func (h *handler) checkpointOperations(w http.ResponseWriter, r *http.Request) persistence.Operations {
	operations, ok := h.sessions.(persistence.Operations)
	if !ok {
		h.serviceError(w, r, core.ErrNativeUnavailable)
		return nil
	}
	return operations
}

type Checkpoint struct {
	ID        string              `json:"id"`
	SessionID string              `json:"session_id"`
	Status    string              `json:"status"`
	Boundary  *CheckpointBoundary `json:"boundary,omitempty"`
	ExpiresAt time.Time           `json:"expires_at,omitzero"`
}
type CheckpointBoundary struct {
	NativeRevision int64 `json:"native_revision"`
}

func checkpointView(attempt persistence.CaptureAttempt) Checkpoint {
	result := Checkpoint{ID: attempt.ID, SessionID: attempt.SessionID, Status: attempt.State, ExpiresAt: attempt.ExpiresAt}
	if attempt.Boundary != nil {
		result.Boundary = &CheckpointBoundary{NativeRevision: attempt.Boundary.NativeRevision}
	}
	if attempt.State == "ready" {
		result.ExpiresAt = time.Time{}
	}
	return result
}
func (h *handler) checkpointStartRoute(w http.ResponseWriter, r *http.Request, client controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, false) {
		return
	}
	key, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	operations := h.checkpointOperations(w, r)
	if operations == nil {
		return
	}
	id := fmt.Sprintf("cp_%x", sha256.Sum256([]byte(client.ID+"\x00"+r.PathValue("session")+"\x00"+key)))
	result, err := operations.StartCapture(r.Context(), r.PathValue("session"), id)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusAccepted, checkpointView(result))
}
func (h *handler) checkpointRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodGet, false) {
		return
	}
	operations := h.checkpointOperations(w, r)
	if operations == nil {
		return
	}
	result, err := operations.ObserveCapture(r.Context(), r.PathValue("checkpoint"), "status")
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, checkpointView(result))
}
func (h *handler) activateSessionRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, false) {
		return
	}
	activator, ok := h.sessions.(interface {
		ActivateSession(context.Context, string) (Session, error)
	})
	if !ok {
		h.serviceError(w, r, core.ErrNativeUnavailable)
		return
	}
	session, err := activator.ActivateSession(r.Context(), r.PathValue("session"))
	h.sessionResponse(w, r, session, err)
}
