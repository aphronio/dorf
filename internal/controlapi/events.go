package controlapi

import (
	"context"
	"net/http"

	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
)

type NativeSessions interface {
	InputCapabilities(context.Context, string) (core.InputCapabilities, error)
	SubmitEvent(context.Context, string, core.NativeEvent) (core.NativeAcknowledgement, error)
	ReadNativeTurns(context.Context, string) (core.HarnessHistory, error)
}

func (h *handler) eventsRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, true) {
		return
	}
	service, ok := h.sessions.(NativeSessions)
	if !ok {
		h.serviceError(w, r, core.ErrNativeUnavailable)
		return
	}
	var event core.NativeEvent
	if !h.decode(w, r, &event) {
		return
	}
	if err := event.Validate(); err != nil {
		h.serviceError(w, r, err)
		return
	}
	ack, err := service.SubmitEvent(r.Context(), r.PathValue("session"), event)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, ack)
}

func (h *handler) turnsRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodGet, false) {
		return
	}
	service, ok := h.sessions.(NativeSessions)
	if !ok {
		h.serviceError(w, r, core.ErrNativeUnavailable)
		return
	}
	result, err := service.ReadNativeTurns(r.Context(), r.PathValue("session"))
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, result)
}

func (h *handler) inputCapabilitiesRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodGet, false) {
		return
	}
	service, ok := h.sessions.(NativeSessions)
	if !ok {
		h.serviceError(w, r, core.ErrNativeUnavailable)
		return
	}
	result, err := service.InputCapabilities(r.Context(), r.PathValue("session"))
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, result)
}
