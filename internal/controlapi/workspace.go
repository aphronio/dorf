package controlapi

import (
	"context"
	"net/http"

	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
)

type WorkspaceReader interface {
	ReadWorkspace(context.Context, string) (persistence.Workspace, error)
}

func (h *handler) workspaceRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodGet, false) {
		return
	}
	reader, ok := h.sessions.(WorkspaceReader)
	if !ok {
		h.serviceError(w, r, core.ErrNativeUnavailable)
		return
	}
	result, err := reader.ReadWorkspace(r.Context(), r.PathValue("session"))
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, result)
}
