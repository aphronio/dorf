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
		"/v1/sessions/source/checkpoints": "POST",
		"/v1/checkpoints/cp_test":         "GET",
		"/v1/sessions/restored/activate":  "POST",
	}
	for path, method := range paths {
		request := httptest.NewRequest(method, path, nil)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		requireProblem(t, result, 401, "unauthenticated")
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/source/checkpoints", strings.NewReader(`{"callback_url":"http://private.invalid/"}`))
	request.Header.Set("Authorization", "Bearer synthetic-credential")
	request.Header.Set("Content-Type", "application/json")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	requireProblem(t, result, 415, "body_not_allowed")
}
