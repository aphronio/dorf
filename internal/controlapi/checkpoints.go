package controlapi

import (
	"net/http"
	"regexp"

	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
)

var checkpointBranchID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

func (h *handler) checkpointOperations(w http.ResponseWriter, r *http.Request) persistence.Operations {
	operations, ok := h.sessions.(persistence.Operations)
	if !ok {
		h.serviceError(w, r, core.ErrNativeUnavailable)
		return nil
	}
	return operations
}

func (h *handler) checkpointBoundaryRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodGet, false) {
		return
	}
	operations := h.checkpointOperations(w, r)
	if operations == nil {
		return
	}
	result, err := operations.CheckpointBoundary(r.Context(), r.PathValue("session"))
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, result)
}

func (h *handler) captureStartRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, false) {
		return
	}
	operations := h.checkpointOperations(w, r)
	if operations == nil {
		return
	}
	result, err := operations.StartCapture(r.Context(), r.PathValue("session"))
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusAccepted, result)
}

func (h *handler) captureRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	method, action := http.MethodGet, "status"
	if r.Method == http.MethodDelete {
		method, action = http.MethodDelete, "cancel"
	}
	if !h.exact(w, r, method, false) {
		return
	}
	h.captureObservation(w, r, action)
}
func (h *handler) captureCommitRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, false) {
		return
	}
	h.captureObservation(w, r, "commit")
}
func (h *handler) captureObservation(w http.ResponseWriter, r *http.Request, action string) {
	operations := h.checkpointOperations(w, r)
	if operations == nil {
		return
	}
	result, err := operations.ObserveCapture(r.Context(), r.PathValue("capture"), action)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, result)
}

func (h *handler) branchStartRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, true) {
		return
	}
	var request struct {
		ID         string `json:"id"`
		Repository string `json:"repository"`
		SnapshotID string `json:"snapshot_id"`
	}
	if !h.decode(w, r, &request) {
		return
	}
	input := persistence.BranchRequest{ID: request.ID, SourceSessionID: r.PathValue("session"), Repository: request.Repository, SnapshotID: request.SnapshotID}
	if input.Validate() != nil || !checkpointBranchID.MatchString(input.ID) {
		h.fail(w, problem("invalid_input"))
		return
	}
	operations := h.checkpointOperations(w, r)
	if operations == nil {
		return
	}
	result, err := operations.BranchCheckpoint(r.Context(), input)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusAccepted, result)
}
func (h *handler) branchRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodGet, false) {
		return
	}
	h.branchObservation(w, r, false)
}
func (h *handler) branchReleaseRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, false) {
		return
	}
	h.branchObservation(w, r, true)
}
func (h *handler) branchObservation(w http.ResponseWriter, r *http.Request, release bool) {
	operations := h.checkpointOperations(w, r)
	if operations == nil {
		return
	}
	result, err := operations.ObserveBranch(r.Context(), r.PathValue("branch"), release)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, result)
}
