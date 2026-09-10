package controlapi

import (
	"github.com/aphronio/dorf/internal/sandbox"
	"io"
	"mime"
	"net/http"
)

func (h *handler) writeFile(w http.ResponseWriter, r *http.Request, name string) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/octet-stream" {
		h.fail(w, problem("unsupported_media_type"))
		return
	}
	conditions := r.Header.Values("If-None-Match")
	if len(conditions) > 1 || (len(conditions) == 1 && conditions[0] != "*") {
		h.fail(w, problem("invalid_query"))
		return
	}
	contents, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sandbox.MaxWorkspaceFileWriteBytes))
	if err != nil {
		h.fail(w, problem("body_too_large"))
		return
	}
	if err := h.jobs.WriteSandboxFile(r.Context(), r.PathValue("sandbox"), name, contents, len(conditions) == 1); err != nil {
		h.serviceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
