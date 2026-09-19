package controlreader

import (
	"context"
	"net/http"

	"github.com/aphronio/dorf/internal/persistence"
)

const WorkspacePath = "/v1/sessions/workspace"

func (s Service) ReadWorkspace(ctx context.Context, sessionID string) (persistence.Workspace, error) {
	if !validIdentity(sessionID) {
		return persistence.Workspace{}, ErrSessionNotFound
	}
	session, err := s.Store.Session(ctx, sessionID)
	if err != nil {
		return persistence.Workspace{}, err
	}
	if s.Workspace == nil {
		return persistence.Workspace{}, ErrUnavailable
	}
	return s.Workspace(ctx, session)
}

func (c Client) ReadWorkspace(ctx context.Context, sessionID string) (persistence.Workspace, error) {
	response, err := c.request(ctx, WorkspacePath, observationRequest{SessionID: sessionID})
	if err != nil {
		return persistence.Workspace{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return persistence.Workspace{}, decodeProblem(response)
	}
	var result persistence.Workspace
	err = decodeJSONResponse(response, &result, MaxRequestBytes, "workspace")
	return result, err
}
