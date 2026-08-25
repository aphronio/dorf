package controlapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
)

func TestHandlerBoundary(t *testing.T) {
	credential := "dcr_client-secret-never-returned"
	enrollment := "enr_AAAAAAAAAAAAAAAAAAAAAA.enrollment-secret-never-returned"
	auth := &fakeAuth{credential: credential, client: controlauth.Client{ID: "client-1", Name: "laptop"}}
	jobs := &fakeJobs{job: controlapi.Job{ID: "job-1", Kind: "direct"}}
	server := controlapi.NewServer(controlapi.Discovery{
		Product: "dorf", Version: "1.2.3", Capabilities: []string{"direct_jobs"},
	}, auth, jobs)
	handler := server.Handler

	do := func(method, target, bearer, idempotencyKey string, body io.Reader) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, target, body)
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		if idempotencyKey != "" {
			request.Header.Set("Idempotency-Key", idempotencyKey)
		}
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	unauthenticated := do(http.MethodGet, "/v1/me", "", "", nil)
	requireProblem(t, unauthenticated, http.StatusUnauthorized, "unauthenticated")
	revoked := do(http.MethodGet, "/v1/me", "revoked-secret", "", nil)
	requireProblem(t, revoked, http.StatusUnauthorized, "unauthenticated")
	assertSecretsAbsent(t, revoked.Body.String(), credential, enrollment, "revoked-secret")

	redeemBody := fmt.Sprintf(`{"enrollment_code":%q,"client_name":"laptop","credential":%q}`, enrollment, credential)
	redeemed := do(http.MethodPost, "/v1/auth/enrollments/redeem", "", "", strings.NewReader(redeemBody))
	requireStatusType(t, redeemed, http.StatusCreated, "application/json")
	var identity controlapi.Identity
	decode(t, redeemed, &identity)
	if identity.Principal.ID != controlauth.DeploymentOperatorPrincipalID || identity.Client.ID != "client-1" || auth.redeemedCode != enrollment || auth.redeemedCredential != credential {
		t.Fatalf("identity=%#v redeem auth=%#v", identity, auth)
	}
	assertSecretsAbsent(t, redeemed.Body.String(), credential, enrollment)
	replayedRedemption := do(http.MethodPost, "/v1/auth/enrollments/redeem", "", "", strings.NewReader(redeemBody))
	requireStatusType(t, replayedRedemption, http.StatusOK, "application/json")

	missingKey := do(http.MethodPost, "/v1/jobs", credential, "", strings.NewReader(`{"goal":"ship it","profile":"default","model":"model-1","reasoning":"high"}`))
	requireProblem(t, missingKey, http.StatusBadRequest, "idempotency_key_required")

	strict := do(http.MethodPost, "/v1/jobs", credential, "request-key-2", strings.NewReader(`{"goal":"ship it","profile":"default","model":"model-1","reasoning":"high","provider_credential":"leak"}`))
	requireProblem(t, strict, http.StatusBadRequest, "invalid_json")
	expandedGoal := strings.Repeat("\x00", 1<<20)
	expandedBody, err := json.Marshal(controlapi.AdmitJobRequest{Goal: expandedGoal, Profile: "default"})
	if err != nil || len(expandedBody) <= 6<<20 {
		t.Fatalf("encode expanded 1 MiB goal: bytes=%d err=%v", len(expandedBody), err)
	}
	expanded := do(http.MethodPost, "/v1/jobs", credential, "request-key-expanded", bytes.NewReader(expandedBody))
	requireStatusType(t, expanded, http.StatusCreated, "application/json")
	if jobs.gotInput.Goal != expandedGoal {
		t.Fatal("JSON escaping changed the exact 1 MiB goal")
	}
	wrongMethod := do(http.MethodDelete, "/v1/jobs/job-1", credential, "", nil)
	requireProblem(t, wrongMethod, http.StatusMethodNotAllowed, "method_not_allowed")
	redirectSpelling := do(http.MethodGet, "/v1/", "", "", nil)
	requireProblem(t, redirectSpelling, http.StatusNotFound, "not_found")
	if redirectSpelling.Header().Get("Location") != "" {
		t.Fatalf("non-canonical path redirected to %q", redirectSpelling.Header().Get("Location"))
	}
	var limited *httptest.ResponseRecorder
	for attempts := 0; attempts < 100; attempts++ {
		limited = do(http.MethodPost, "/v1/auth/enrollments/redeem", "", "", strings.NewReader(redeemBody))
		if limited.Code == http.StatusTooManyRequests {
			break
		}
	}
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("enrollment rate limit status=%d retry-after=%q", limited.Code, limited.Header().Get("Retry-After"))
	}
	var rateProblem controlapi.Problem
	decode(t, limited, &rateProblem)
	if !rateProblem.Retryable {
		t.Fatal("rate-limited enrollment was not marked retryable")
	}
	otherEnrollment := strings.Replace(enrollment, "enr_A", "enr_B", 1)
	otherBody := fmt.Sprintf(`{"enrollment_code":%q,"client_name":"laptop","credential":%q}`, otherEnrollment, credential)
	unrelated := do(http.MethodPost, "/v1/auth/enrollments/redeem", "", "", strings.NewReader(otherBody))
	if unrelated.Code == http.StatusTooManyRequests {
		t.Fatal("one Enrollment exhausted another Enrollment's rate limit")
	}
}

type fakeAuth struct {
	credential         string
	client             controlauth.Client
	redeemedCode       string
	redeemedCredential string
	redeemCalls        int
}

func (a *fakeAuth) Authenticate(_ context.Context, credential string) (controlauth.Client, error) {
	if credential != a.credential {
		return controlauth.Client{}, fmt.Errorf("%w: rejected secret %s", controlauth.ErrUnauthenticated, credential)
	}
	return a.client, nil
}

func (a *fakeAuth) Redeem(_ context.Context, code, name, credential string) (controlauth.Client, bool, error) {
	a.redeemedCode, a.redeemedCredential = code, credential
	a.redeemCalls++
	if name != a.client.Name {
		return controlauth.Client{}, false, controlauth.ErrInvalidInput
	}
	return a.client, a.redeemCalls == 1, nil
}

type fakeJobs struct {
	job      controlapi.Job
	gotInput controlapi.AdmitJobRequest
}

func (j *fakeJobs) AdmitDirect(_ context.Context, _ string, input controlapi.AdmitJobRequest) (controlapi.Job, bool, error) {
	j.gotInput = input
	return j.job, true, nil
}

func (j *fakeJobs) Get(_ context.Context, id string) (controlapi.Job, error) {
	if id != j.job.ID {
		return controlapi.Job{}, controlapi.ErrJobNotFound
	}
	return j.job, nil
}

func (j *fakeJobs) RequestCleanup(_ context.Context, id string) (controlapi.Job, error) {
	if id != j.job.ID {
		return controlapi.Job{}, controlapi.ErrJobNotFound
	}
	return j.job, nil
}

func requireStatusType(t *testing.T, response *httptest.ResponseRecorder, status int, contentType string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != contentType {
		t.Fatalf("status/type = %d/%q, want %d/%q; body=%s", response.Code, response.Header().Get("Content-Type"), status, contentType, response.Body.String())
	}
}

func requireProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	requireStatusType(t, response, status, "application/problem+json")
	var value controlapi.Problem
	decode(t, response, &value)
	if value.Status != status || value.Code != code || value.Type == "" || value.Title == "" || value.Details == nil {
		t.Fatalf("problem=%#v, want status=%d code=%q with stable fields", value, status, code)
	}
}

func decode(t *testing.T, response *httptest.ResponseRecorder, output any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), output); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
}

func assertSecretsAbsent(t *testing.T, body string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && bytes.Contains([]byte(body), []byte(secret)) {
			t.Fatalf("response leaked secret %q: %s", secret, body)
		}
	}
}
