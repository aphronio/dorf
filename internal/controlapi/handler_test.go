package controlapi_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
	for _, route := range []struct {
		method string
		path   string
		body   io.Reader
	}{
		{http.MethodDelete, "/v1/jobs", nil},
		{http.MethodGet, "/v1/jobs/job-1", nil},
		{http.MethodDelete, "/v1/jobs/job-1", nil},
		{http.MethodGet, "/v1/jobs/job-1/watch", nil},
		{http.MethodGet, "/v1/jobs?limit=1", nil},
		{http.MethodPost, "/v1/jobs/job-1/messages", strings.NewReader(`{}`)},
		{http.MethodGet, "/v1/jobs/job-1/messages/message-1", nil},
		{http.MethodPost, "/v1/jobs/job-1/retries", nil},
		{http.MethodGet, "/v1/jobs/job-1/evidence", nil},
		{http.MethodPut, "/v1/jobs/job-1/abandon", nil},
		{http.MethodPut, "/v1/jobs/job-1/cleanup", nil},
		{http.MethodGet, "/v1/sandboxes/sandbox-1/files?path=REPORT.md", nil},
		{http.MethodPost, "/v1/workflows/coding/jobs", strings.NewReader(`{}`)},
		{http.MethodPost, "/v1/workflows/codebase-investigation/jobs", strings.NewReader(`{}`)},
	} {
		requireProblem(t, do(route.method, route.path, "", "", route.body), http.StatusUnauthorized, "unauthenticated")
	}
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
	jobsWrongMethod := do(http.MethodDelete, "/v1/jobs", credential, "", nil)
	requireProblem(t, jobsWrongMethod, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := jobsWrongMethod.Header().Get("Allow"); allow != "GET, POST" {
		t.Fatalf("jobs Allow=%q, want GET, POST", allow)
	}
	redirectSpelling := do(http.MethodGet, "/v1/", "", "", nil)
	requireProblem(t, redirectSpelling, http.StatusNotFound, "not_found")
	if redirectSpelling.Header().Get("Location") != "" {
		t.Fatalf("non-canonical path redirected to %q", redirectSpelling.Header().Get("Location"))
	}
}

func TestAdmissionsAcceptExplicitAIConnection(t *testing.T) {
	credential := "dcr_admission-connection"
	base := controlapi.Job{ID: "job-1"}
	tests := []struct {
		name   string
		target string
		body   string
		jobs   *fakeJobs
		got    func(*fakeJobs) string
	}{
		{
			name: "direct", target: "/v1/jobs",
			body: `{"goal":"ship","model":"model-1","ai_connection":"work-openai"}`,
			jobs: &fakeJobs{job: controlapi.Job{ID: base.ID, Kind: controlapi.JobKindDirect}},
			got:  func(j *fakeJobs) string { return j.gotInput.AIConnection },
		},
		{
			name: "coding", target: "/v1/workflows/coding/jobs",
			body: `{"goal":"ship","repository":"https://github.com/acme/widget.git","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_branch":"main","model":"model-1","ai_connection":"work-openai"}`,
			jobs: &fakeJobs{job: controlapi.Job{ID: base.ID, Kind: controlapi.JobKindCoding}, view: controlapi.CodingJob{Job: controlapi.Job{ID: base.ID, Kind: controlapi.JobKindCoding}}},
			got:  func(j *fakeJobs) string { return j.codingInput.AIConnection },
		},
		{
			name: "investigation", target: "/v1/workflows/codebase-investigation/jobs",
			body: `{"brief":"trace it","repository":"https://github.com/acme/widget.git","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","model":"model-1","ai_connection":"work-openai"}`,
			jobs: &fakeJobs{job: controlapi.Job{ID: base.ID, Kind: controlapi.JobKindInvestigation}, view: controlapi.InvestigationJob{Job: controlapi.Job{ID: base.ID, Kind: controlapi.JobKindInvestigation}}},
			got:  func(j *fakeJobs) string { return j.investigationInput.AIConnection },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, test.jobs).Handler
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+credential)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "admit-1")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			requireStatusType(t, response, http.StatusCreated, "application/json")
			if got := test.got(test.jobs); got != "work-openai" {
				t.Fatalf("AIConnection=%q, want work-openai", got)
			}
		})
	}
}

func TestEnrollmentRedemptionUsesDeploymentWideRateLimit(t *testing.T) {
	auth := &fakeAuth{
		client:    controlauth.Client{Name: "laptop"},
		redeemErr: controlauth.ErrEnrollmentUnavailable,
	}
	handler := controlapi.NewServer(controlapi.Discovery{}, auth, &fakeJobs{}).Handler
	redeem := func(enrollment string) *httptest.ResponseRecorder {
		t.Helper()
		body := fmt.Sprintf(`{"enrollment_code":%q,"client_name":"laptop","credential":"dcr_attacker-generated"}`, enrollment)
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/enrollments/redeem", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	for attempt := 0; attempt < 10; attempt++ {
		response := redeem(fmt.Sprintf("enr_%022d.fake-secret", attempt))
		requireProblem(t, response, http.StatusUnauthorized, "enrollment_unavailable")
	}

	limited := redeem("entirely-fake-and-different")
	requireProblem(t, limited, http.StatusTooManyRequests, "rate_limited")
	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("rate-limited enrollment response omitted Retry-After")
	}
	var rateProblem controlapi.Problem
	decode(t, limited, &rateProblem)
	if !rateProblem.Retryable {
		t.Fatal("rate-limited enrollment was not marked retryable")
	}
	if auth.redeemCalls != 10 {
		t.Fatalf("authentication service received %d redemption attempts after the shared limit, want 10", auth.redeemCalls)
	}
}

func TestJobListUsesStrictBoundedQueryAndExplicitEmptyCollection(t *testing.T) {
	credential := "dcr_job-list"
	next := "next-page"
	admittedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	jobs := &fakeJobs{list: controlapi.JobList{
		Jobs:       []controlapi.JobSummary{{ID: "job-2", Kind: controlapi.JobKindDirect, AdmittedAt: admittedAt}},
		NextCursor: &next,
	}}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, jobs).Handler
	do := func(target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	response := do("/v1/jobs?limit=2&cursor=page-one")
	requireStatusType(t, response, http.StatusOK, "application/json")
	var page controlapi.JobList
	decode(t, response, &page)
	if jobs.listLimit != 2 || jobs.listCursor != "page-one" || len(page.Jobs) != 1 || page.Jobs[0].ID != "job-2" || page.NextCursor == nil || *page.NextCursor != next {
		t.Fatalf("request limit/cursor=%d/%q page=%#v", jobs.listLimit, jobs.listCursor, page)
	}

	jobs.list = controlapi.JobList{}
	empty := do("/v1/jobs")
	requireStatusType(t, empty, http.StatusOK, "application/json")
	if body := empty.Body.String(); !strings.Contains(body, `"jobs":[]`) || !strings.Contains(body, `"next_cursor":null`) {
		t.Fatalf("empty page omitted explicit collection/cursor: %s", body)
	}
	requireProblem(t, do("/v1/jobs?limit=101"), http.StatusBadRequest, "invalid_query")
	requireProblem(t, do("/v1/jobs?cursor="), http.StatusBadRequest, "invalid_cursor")

	jobs.listErr = controlapi.ErrInvalidCursor
	requireProblem(t, do("/v1/jobs?cursor=tampered"), http.StatusBadRequest, "invalid_cursor")
}

func TestJobConditionalGetAndDirectInteractionRoutes(t *testing.T) {
	credential := "dcr_control-client"
	message := controlapi.Message{
		ID: "message-2", JobID: "job-1", Sequence: 2, Intent: "follow",
		Delivery: controlapi.State{State: "completed"}, Result: &controlapi.MessageResult{Outcome: "completed", Output: "done"},
		AdmittedAt: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
	}
	jobs := &fakeJobs{
		job:     controlapi.Job{ID: "job-1", Kind: "direct", Goal: "ship", InitialMessageID: "message-1", Sandboxes: []controlapi.Sandbox{{ID: "sandbox-1", Name: "default"}}},
		message: message, messageCreated: true,
		retry: controlapi.Retry{JobID: "job-1", State: "scheduled"}, retryCreated: true,
	}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, jobs).Handler

	request := func(method, target string, body io.Reader) *http.Request {
		req := httptest.NewRequest(method, target, body)
		req.Header.Set("Authorization", "Bearer "+credential)
		return req
	}
	do := func(req *http.Request) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	first := do(request(http.MethodGet, "/v1/jobs/job-1", nil))
	requireStatusType(t, first, http.StatusOK, "application/json")
	etag := first.Header().Get("ETag")
	if len(etag) != 66 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		t.Fatalf("ETag=%q, want quoted SHA-256 representation hash", etag)
	}
	conditionalRequest := request(http.MethodGet, "/v1/jobs/job-1", nil)
	conditionalRequest.Header.Set("If-None-Match", etag)
	conditional := do(conditionalRequest)
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 || conditional.Header().Get("ETag") != etag {
		t.Fatalf("conditional response status/body/etag=%d/%q/%q", conditional.Code, conditional.Body.String(), conditional.Header().Get("ETag"))
	}

	messageRequest := request(http.MethodPost, "/v1/jobs/job-1/messages", strings.NewReader(`{"text":"continue","intent":"follow"}`))
	messageRequest.Header.Set("Content-Type", "application/json")
	messageRequest.Header.Set("Idempotency-Key", "send-2")
	sent := do(messageRequest)
	requireStatusType(t, sent, http.StatusCreated, "application/json")
	var accepted controlapi.Message
	decode(t, sent, &accepted)
	if accepted.ID != message.ID || accepted.Result == nil || *accepted.Result != *message.Result || jobs.messageKey != "send-2" || jobs.messageInput != (controlapi.SendMessageRequest{Text: "continue", Intent: "follow"}) {
		t.Fatalf("Message=%#v key/input=%q/%#v, want %#v/send-2", accepted, jobs.messageKey, jobs.messageInput, message)
	}
	retryRequest := request(http.MethodPost, "/v1/jobs/job-1/retries", nil)
	retryRequest.Header.Set("Idempotency-Key", "retry-3")
	retried := do(retryRequest)
	requireStatusType(t, retried, http.StatusCreated, "application/json")
	var retry controlapi.Retry
	decode(t, retried, &retry)
	if retry != jobs.retry || jobs.retryKey != "retry-3" {
		t.Fatalf("Retry=%#v key=%q, want %#v/retry-3", retry, jobs.retryKey, jobs.retry)
	}

	query := do(request(http.MethodGet, "/v1/jobs/job-1?extra=true", nil))
	requireProblem(t, query, http.StatusBadRequest, "invalid_query")
	missingMessage := do(request(http.MethodGet, "/v1/jobs/job-1/messages/other", nil))
	requireProblem(t, missingMessage, http.StatusNotFound, "message_not_found")
	wrongWatchType := do(request(http.MethodGet, "/v1/jobs/job-1/watch", nil))
	requireProblem(t, wrongWatchType, http.StatusNotAcceptable, "not_acceptable")
	invalidResumeRequest := request(http.MethodGet, "/v1/jobs/job-1/watch", nil)
	invalidResumeRequest.Header.Set("Accept", "text/event-stream")
	invalidResumeRequest.Header.Set("Last-Event-ID", "not-a-representation-hash")
	requireProblem(t, do(invalidResumeRequest), http.StatusBadRequest, "invalid_last_event_id")
	conditionalCleanup := request(http.MethodPut, "/v1/jobs/job-1/cleanup", nil)
	conditionalCleanup.Header.Set("If-None-Match", "*")
	requireProblem(t, do(conditionalCleanup), http.StatusBadRequest, "unsupported_precondition")
	if jobs.cleanupCalls != 0 {
		t.Fatal("unsupported cleanup precondition reached the mutation")
	}
}

func TestAbandonIsAuthenticatedIdempotentAndReturnsCanonicalJob(t *testing.T) {
	credential := "dcr_abandon"
	job := controlapi.CodingJob{Job: controlapi.Job{ID: "job-coding", Kind: controlapi.JobKindCoding, Goal: "ship"}}
	jobs := &fakeJobs{job: job.Job, view: job}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, jobs).Handler
	put := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/v1/jobs/job-coding/abandon", nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for call := 1; call <= 2; call++ {
		response := put()
		requireStatusType(t, response, http.StatusOK, "application/json")
		var got controlapi.CodingJob
		decode(t, response, &got)
		if got.ID != job.ID || got.Kind != controlapi.JobKindCoding || response.Header().Get("ETag") == "" || jobs.abandonCalls != call {
			t.Fatalf("call %d: job/etag/calls=%#v/%q/%d", call, got, response.Header().Get("ETag"), jobs.abandonCalls)
		}
	}

	jobs.abandonErr = controlapi.ErrAbandonUnavailable
	requireProblem(t, put(), http.StatusConflict, "abandon_unavailable")
}

func TestConcreteWorkflowJobRepresentationDrivesETag(t *testing.T) {
	credential := "dcr_control-client"
	base := controlapi.Job{
		ID: "job-coding", Kind: controlapi.JobKindCoding, Goal: "ship",
	}
	coding := controlapi.CodingJob{
		Job: base, WorkflowRevision: "3", Repository: "https://github.com/acme/widget.git", Revision: strings.Repeat("b", 40),
	}
	jobs := &fakeJobs{job: base, view: coding}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, jobs).Handler
	get := func(etag string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-coding", nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		if etag != "" {
			request.Header.Set("If-None-Match", etag)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	first := get("")
	requireStatusType(t, first, http.StatusOK, "application/json")
	firstETag := first.Header().Get("ETag")
	coding.Proposal = &controlapi.CodingProposal{Number: 42, URL: "https://github.com/acme/widget/pull/42", Revision: coding.Revision}
	jobs.mu.Lock()
	jobs.view = coding
	jobs.mu.Unlock()
	changed := get(firstETag)
	requireStatusType(t, changed, http.StatusOK, "application/json")
	var gotCoding controlapi.CodingJob
	decode(t, changed, &gotCoding)
	if gotCoding.ID != base.ID || gotCoding.Kind != controlapi.JobKindCoding || gotCoding.Proposal == nil || gotCoding.Proposal.Number != 42 || changed.Header().Get("ETag") == firstETag {
		t.Fatalf("changed coding Job/etag=%#v/%q after %q", gotCoding, changed.Header().Get("ETag"), firstETag)
	}

	coding.Kind = controlapi.JobKindDirect
	jobs.mu.Lock()
	jobs.view = coding
	jobs.mu.Unlock()
	requireProblem(t, get(""), http.StatusInternalServerError, "internal_error")
}

func TestSandboxFileResponseContract(t *testing.T) {
	credential := "dcr_control-client"
	contents := []byte{0x00, 0xff, '\n'}
	jobs := &fakeJobs{job: controlapi.Job{Sandboxes: []controlapi.Sandbox{{ID: "sandbox-1"}}}, file: contents}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, jobs).Handler
	get := func(target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	response := get("/v1/sandboxes/sandbox-1/files?path=nested%2FREPORT%2B.bin")
	digest := sha256.Sum256(contents)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/octet-stream" ||
		response.Header().Get("Content-Length") != fmt.Sprint(len(contents)) ||
		response.Header().Get("Content-Digest") != "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":" ||
		!bytes.Equal(response.Body.Bytes(), contents) || jobs.filePath != "nested/REPORT+.bin" {
		t.Fatalf("file response status/type/length/digest/path=%d/%q/%q/%q/%q", response.Code, response.Header().Get("Content-Type"), response.Header().Get("Content-Length"), response.Header().Get("Content-Digest"), jobs.filePath)
	}
	requireProblem(t, get("/v1/sandboxes/sandbox-1/files"), http.StatusBadRequest, "file_path_required")
}

func TestJobWatchEmitsChangedSnapshotsAndStopsOnServerShutdown(t *testing.T) {
	credential := "dcr_control-client"
	jobs := &fakeJobs{job: controlapi.Job{ID: "job-1", Kind: "direct", Goal: "first", Sandboxes: []controlapi.Sandbox{}}}
	api := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, jobs)

	open := func(lastID string) (*streamResponse, context.CancelFunc, <-chan struct{}) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-1/watch", nil).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+credential)
		request.Header.Set("Accept", "text/event-stream")
		if lastID != "" {
			request.Header.Set("Last-Event-ID", lastID)
		}
		response := newStreamResponse()
		done := make(chan struct{})
		go func() {
			api.Handler.ServeHTTP(response, request)
			close(done)
		}()
		return response, cancel, done
	}

	firstResponse, cancelFirst, firstDone := open("")
	firstResponse.awaitFlush(t)
	firstID, firstJob := readSnapshotEvent(t, bufio.NewReader(bytes.NewReader(firstResponse.bytes())))
	if firstJob.Goal != "first" || len(firstID) != 64 {
		t.Fatalf("first snapshot id/job=%q/%#v", firstID, firstJob)
	}
	if status, header, bounded := firstResponse.metadata(); status != http.StatusOK || header.Get("Content-Type") != "text/event-stream" || header.Get("Cache-Control") != "no-store, no-transform" || !bounded {
		t.Fatalf("watch status/type/cache/bounded-write=%d/%q/%q/%t", status, header.Get("Content-Type"), header.Get("Cache-Control"), bounded)
	}
	cancelFirst()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled watch did not stop")
	}

	resumedResponse, cancelResumed, resumedDone := open(firstID)
	defer cancelResumed()
	resumedResponse.awaitFlush(t)
	if got := resumedResponse.bytes(); len(got) != 0 {
		t.Fatalf("matching Last-Event-ID replayed unchanged snapshot: %q", got)
	}
	jobs.mu.Lock()
	jobs.job.Goal = "changed"
	jobs.mu.Unlock()
	resumedResponse.awaitFlush(t)
	changedID, changedJob := readSnapshotEvent(t, bufio.NewReader(bytes.NewReader(resumedResponse.bytes())))
	if changedJob.Goal != "changed" || changedID == firstID {
		t.Fatalf("changed snapshot id/job=%q/%#v after %q", changedID, changedJob, firstID)
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := api.Shutdown(shutdown); err != nil {
		t.Fatalf("shutdown API with active watch: %v", err)
	}
	select {
	case <-resumedDone:
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop after server shutdown")
	}
}

func TestJobWatchReauthenticatesNoLaterThanCredentialExpiry(t *testing.T) {
	credential := "dcr_expiring-client"
	auth := &fakeAuth{credential: credential, client: controlauth.Client{CredentialExpiresAt: time.Now().Add(100 * time.Millisecond)}}
	jobs := &fakeJobs{job: controlapi.Job{ID: "job-1", Kind: "direct", Sandboxes: []controlapi.Sandbox{}}}
	api := controlapi.NewServer(controlapi.Discovery{}, auth, jobs)
	request := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-1/watch", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "text/event-stream")
	response := newStreamResponse()
	done := make(chan struct{})
	go func() {
		api.Handler.ServeHTTP(response, request)
		close(done)
	}()
	response.awaitFlush(t)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch outlived its Client credential")
	}

	auth.credential = "revoked"
	reconnect := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-1/watch", nil)
	reconnect.Header.Set("Authorization", "Bearer "+credential)
	reconnect.Header.Set("Accept", "text/event-stream")
	rejected := httptest.NewRecorder()
	api.Handler.ServeHTTP(rejected, reconnect)
	requireProblem(t, rejected, http.StatusUnauthorized, "unauthenticated")
}

func TestJobWatchReturnsAuthenticationProblemWhenCredentialExpiresBeforeStreaming(t *testing.T) {
	credential := "dcr_expiring-before-stream"
	auth := &fakeAuth{credential: credential, client: controlauth.Client{CredentialExpiresAt: time.Now().Add(25 * time.Millisecond)}}
	jobs := &fakeJobs{job: controlapi.Job{ID: "job-1", Kind: "direct"}, waitForGetContext: true}
	request := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-1/watch", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()

	controlapi.NewServer(controlapi.Discovery{}, auth, jobs).Handler.ServeHTTP(response, request)

	requireProblem(t, response, http.StatusUnauthorized, "unauthenticated")
}

type streamResponse struct {
	mu              sync.Mutex
	header          http.Header
	status          int
	body            bytes.Buffer
	flushes         chan struct{}
	boundedDeadline bool
}

func newStreamResponse() *streamResponse {
	return &streamResponse{header: make(http.Header), flushes: make(chan struct{}, 4)}
}

func (w *streamResponse) Header() http.Header { return w.header }

func (w *streamResponse) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}

func (w *streamResponse) Write(contents []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(contents)
}

func (w *streamResponse) Flush() {
	select {
	case w.flushes <- struct{}{}:
	default:
	}
}

func (w *streamResponse) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		w.mu.Lock()
		w.boundedDeadline = true
		w.mu.Unlock()
	}
	return nil
}

func (w *streamResponse) awaitFlush(t *testing.T) {
	t.Helper()
	select {
	case <-w.flushes:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for stream flush")
	}
}

func (w *streamResponse) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.body.Bytes()...)
}

func (w *streamResponse) metadata() (int, http.Header, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status, w.header.Clone(), w.boundedDeadline
}

func readSnapshotEvent(t *testing.T, reader *bufio.Reader) (string, controlapi.Job) {
	t.Helper()
	var event, id, data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read snapshot event: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if event != "snapshot" || id == "" || data == "" {
				t.Fatalf("SSE event/type/id/data=%q/%q/%q", event, id, data)
			}
			var job controlapi.DirectJob
			if err := json.Unmarshal([]byte(data), &job); err != nil {
				t.Fatalf("decode snapshot %q: %v", data, err)
			}
			return id, job.Job
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

type fakeAuth struct {
	credential         string
	client             controlauth.Client
	redeemedCode       string
	redeemedCredential string
	redeemCalls        int
	redeemErr          error
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
	if a.redeemErr != nil {
		return controlauth.Client{}, false, a.redeemErr
	}
	return a.client, a.redeemCalls == 1, nil
}

type fakeJobs struct {
	mu                 sync.Mutex
	job                controlapi.Job
	view               controlapi.JobView
	list               controlapi.JobList
	listErr            error
	listLimit          int
	listCursor         string
	gotInput           controlapi.AdmitJobRequest
	codingInput        controlapi.AdmitCodingJobRequest
	investigationInput controlapi.AdmitInvestigationJobRequest
	message            controlapi.Message
	retry              controlapi.Retry
	file               []byte
	filePath           string
	messageKey         string
	retryKey           string
	messageInput       controlapi.SendMessageRequest
	messageCreated     bool
	retryCreated       bool
	abandonCalls       int
	abandonErr         error
	cleanupCalls       int
	waitForGetContext  bool
}

func (j *fakeJobs) List(_ context.Context, limit int, cursor string) (controlapi.JobList, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.listLimit, j.listCursor = limit, cursor
	return j.list, j.listErr
}

func (j *fakeJobs) AdmitDirect(_ context.Context, _ string, input controlapi.AdmitJobRequest) (controlapi.DirectJob, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.gotInput = input
	return controlapi.DirectJob{Job: j.job}, true, nil
}

func (j *fakeJobs) AdmitCoding(_ context.Context, _ string, input controlapi.AdmitCodingJobRequest) (controlapi.CodingJob, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.codingInput = input
	job, _ := j.current().(controlapi.CodingJob)
	return job, true, nil
}

func (j *fakeJobs) AdmitInvestigation(_ context.Context, _ string, input controlapi.AdmitInvestigationJobRequest) (controlapi.InvestigationJob, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.investigationInput = input
	job, _ := j.current().(controlapi.InvestigationJob)
	return job, true, nil
}

func (j *fakeJobs) Get(ctx context.Context, id string) (controlapi.JobView, error) {
	j.mu.Lock()
	wait := j.waitForGetContext
	if id != j.job.ID {
		j.mu.Unlock()
		return nil, controlapi.ErrJobNotFound
	}
	view := j.current()
	j.mu.Unlock()
	if wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return view, nil
}

func (j *fakeJobs) SendMessage(_ context.Context, jobID, key string, input controlapi.SendMessageRequest) (controlapi.Message, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if jobID != j.job.ID {
		return controlapi.Message{}, false, controlapi.ErrJobNotFound
	}
	j.messageKey = key
	j.messageInput = input
	return j.message, j.messageCreated, nil
}

func (j *fakeJobs) GetMessage(_ context.Context, jobID, messageID string) (controlapi.Message, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if jobID != j.job.ID || messageID != j.message.ID {
		return controlapi.Message{}, controlapi.ErrMessageNotFound
	}
	return j.message, nil
}

func (j *fakeJobs) Retry(_ context.Context, jobID, key string) (controlapi.Retry, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if jobID != j.job.ID {
		return controlapi.Retry{}, false, controlapi.ErrJobNotFound
	}
	j.retryKey = key
	return j.retry, j.retryCreated, nil
}

func (j *fakeJobs) Abandon(_ context.Context, id string) (controlapi.JobView, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.abandonCalls++
	if id != j.job.ID {
		return nil, controlapi.ErrJobNotFound
	}
	if j.abandonErr != nil {
		return nil, j.abandonErr
	}
	return j.current(), nil
}

func (j *fakeJobs) ReadSandboxFile(_ context.Context, sandboxID, path string) ([]byte, error) {
	if len(j.job.Sandboxes) == 0 || sandboxID != j.job.Sandboxes[0].ID {
		return nil, controlapi.ErrSandboxNotFound
	}
	j.filePath = path
	return append([]byte(nil), j.file...), nil
}

func (j *fakeJobs) Evidence(_ context.Context, jobID string) ([]controlapi.Evidence, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if jobID != j.job.ID {
		return nil, controlapi.ErrJobNotFound
	}
	return nil, nil
}

func (j *fakeJobs) RequestCleanup(_ context.Context, id string) (controlapi.JobView, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cleanupCalls++
	if id != j.job.ID {
		return nil, controlapi.ErrJobNotFound
	}
	return j.current(), nil
}

func (j *fakeJobs) current() controlapi.JobView {
	if j.view != nil {
		return j.view
	}
	return controlapi.DirectJob{Job: j.job}
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
