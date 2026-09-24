package controlapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
)

func TestCheckpointAPIAuthorityAndNoRemoteCallback(t *testing.T) {
	auth := &fakeAuth{credential: "synthetic-credential", client: controlauth.Client{ID: "client"}}
	handler := controlapi.NewServer(controlapi.Discovery{}, auth, &fakeSessions{}, nil).Handler
	paths := map[string]string{
		"/v1/sessions/source/checkpoint-boundary": "GET",
		"/v1/sessions/source/checkpoint-captures": "POST",
		"/v1/checkpoint-captures/attempt":         "DELETE",
		"/v1/checkpoint-captures/attempt/commit":  "POST",
		"/v1/sessions/source/checkpoint-branches": "POST",
		"/v1/checkpoint-branches/branch":          "GET",
		"/v1/checkpoint-branches/branch/release":  "POST",
	}
	for path, method := range paths {
		request := httptest.NewRequest(method, path, nil)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		requireProblem(t, result, 401, "unauthenticated")
	}
	for _, tc := range []struct {
		path, body string
		status     int
		code       string
	}{
		{"/v1/sessions/source/checkpoint-captures", `{"callback_url":"http://private.invalid/","command":"sh"}`, 415, "body_not_allowed"},
		{"/v1/sessions/source/checkpoint-branches", `{"id":"unreachable/branch","repository":"store","snapshot_id":"` + strings.Repeat("a", 64) + `"}`, 422, "invalid_input"},
	} {
		request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer synthetic-credential")
		request.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		requireProblem(t, result, tc.status, tc.code)
	}
}
