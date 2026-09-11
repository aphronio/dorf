package controlapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/aphronio/dorf/internal/controlauth"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func (h *handler) sandboxExecRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, true) {
		return
	}
	var command provider.Command
	r.Body = http.MaxBytesReader(w, r.Body, provider.MaxCommandRequestBytes)
	if !h.decode(w, r, &command) {
		return
	}
	if err := command.Validate(); err != nil {
		h.fail(w, problem("invalid_input"))
		return
	}
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(command.Timeout() + 5*time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.fail(w, problem("sandbox_exec_unavailable"))
		return
	}
	result, err := h.jobs.ExecSandbox(r.Context(), r.PathValue("sandbox"), command)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, result)
}
