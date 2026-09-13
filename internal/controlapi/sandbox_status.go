package controlapi

import (
	"github.com/aphronio/dorf/internal/controlauth"
	"net/http"
)

func (h *handler) sandboxStatusRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodGet, false) {
		return
	}
	result, err := h.jobs.ReadSandboxStatus(r.Context(), r.PathValue("sandbox"))
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, result)
}
