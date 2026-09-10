package controlapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
)

type fakeProfiles struct {
	list  controlapi.ProfileList
	err   error
	calls int
}

func (p *fakeProfiles) List(context.Context) (controlapi.ProfileList, error) {
	p.calls++
	return p.list, p.err
}

func TestProfileListingAuthenticatesAndAcceptsOnlyAnExactRead(t *testing.T) {
	profiles := &fakeProfiles{list: controlapi.ProfileList{Profiles: []controlapi.ProfileSummary{
		{Name: "cloud", Provider: "e2b", Harness: "codex", Default: true, Verified: true},
	}}}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: "valid"}, nil, profiles).Handler
	for _, test := range []struct {
		name, method, target, credential, body, code string
		status                                       int
	}{
		{"anonymous", "GET", "/v1/profiles", "", "", "unauthenticated", 401},
		{"revoked", "GET", "/v1/profiles", "revoked", "", "unauthenticated", 401},
		{"mutation", "POST", "/v1/profiles", "valid", "", "method_not_allowed", 405},
		{"query", "GET", "/v1/profiles?provider=e2b", "valid", "", "invalid_query", 400},
		{"body", "GET", "/v1/profiles", "valid", "{}", "body_not_allowed", 415},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			if test.credential != "" {
				request.Header.Set("Authorization", "Bearer "+test.credential)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			requireProblem(t, response, test.status, test.code)
		})
	}
	if profiles.calls != 0 {
		t.Fatalf("rejected requests read profiles %d times", profiles.calls)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/profiles", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	requireStatusType(t, response, http.StatusOK, "application/json")
	if got := strings.TrimSpace(response.Body.String()); got != `{"profiles":[{"name":"cloud","provider":"e2b","harness":"codex","default":true,"verified":true}]}` {
		t.Fatalf("profile response=%s", got)
	}
	if response.Header().Get("Cache-Control") != "no-store, no-transform" {
		t.Fatalf("profile cache policy=%q", response.Header().Get("Cache-Control"))
	}
	profiles.list.Profiles = []controlapi.ProfileSummary{}
	empty := httptest.NewRecorder()
	handler.ServeHTTP(empty, request)
	if got := strings.TrimSpace(empty.Body.String()); empty.Code != 200 || got != `{"profiles":[]}` {
		t.Fatalf("empty profile response=%d %s", empty.Code, got)
	}
	profiles.err = fmt.Errorf("private provider configuration")
	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, request)
	requireProblem(t, failed, http.StatusInternalServerError, "internal_error")
	assertSecretsAbsent(t, failed.Body.String(), "private provider configuration")
}
