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
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestHandlerBoundary(t *testing.T) {
	credential := "dcr_client-secret-never-returned"
	enrollment := "enr_AAAAAAAAAAAAAAAAAAAAAA.enrollment-secret-never-returned"
	auth := &fakeAuth{credential: credential, client: controlauth.Client{ID: "client-1", Name: "laptop"}}
	sessions := &fakeSessions{session: controlapi.Session{ID: "session-1"}}
	server := controlapi.NewServer(controlapi.Discovery{
		Product: "dorf", Version: "1.2.3", Capabilities: []string{"direct_sessions"},
	}, auth, sessions, nil)
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
		{http.MethodDelete, "/v1/sessions", nil},
		{http.MethodGet, "/v1/sessions/session-1", nil},
		{http.MethodDelete, "/v1/sessions/session-1", nil},
		{http.MethodGet, "/v1/sessions/session-1/watch", nil},
		{http.MethodGet, "/v1/sessions?limit=1", nil},
		{http.MethodPost, "/v1/sessions/session-1/events", strings.NewReader(`{}`)},
		{http.MethodGet, "/v1/sessions/session-1/turns/turn", nil},
		{http.MethodPut, "/v1/sessions/session-1/events", nil},
		{http.MethodPost, "/v1/sessions/session-1/retries", nil},
		{http.MethodPut, "/v1/sessions/session-1/cleanup", nil},
		{http.MethodGet, "/v1/sandboxes/sandbox-1/files?path=REPORT.md", nil},
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

	missingKey := do(http.MethodPost, "/v1/sessions", credential, "", strings.NewReader(`{"agents_md":"ship it","profile":"default","model":"model-1","reasoning":"high"}`))
	requireProblem(t, missingKey, http.StatusBadRequest, "idempotency_key_required")

	strict := do(http.MethodPost, "/v1/sessions", credential, "request-key-2", strings.NewReader(`{"agents_md":"ship it","profile":"default","model":"model-1","reasoning":"high","provider_credential":"leak"}`))
	requireProblem(t, strict, http.StatusBadRequest, "invalid_json")
	expandedGoal := strings.Repeat("\x00", 1<<20)
	expandedBody, err := json.Marshal(controlapi.CreateSessionRequest{AgentsMD: expandedGoal, Profile: "default"})
	if err != nil || len(expandedBody) <= 6<<20 {
		t.Fatalf("encode expanded 1 MiB goal: bytes=%d err=%v", len(expandedBody), err)
	}
	expanded := do(http.MethodPost, "/v1/sessions", credential, "request-key-expanded", bytes.NewReader(expandedBody))
	requireStatusType(t, expanded, http.StatusCreated, "application/json")
	if sessions.gotInput.AgentsMD != expandedGoal {
		t.Fatal("JSON escaping changed the exact 1 MiB goal")
	}
	wrongMethod := do(http.MethodDelete, "/v1/sessions/session-1", credential, "", nil)
	requireProblem(t, wrongMethod, http.StatusMethodNotAllowed, "method_not_allowed")
	sessionsWrongMethod := do(http.MethodDelete, "/v1/sessions", credential, "", nil)
	requireProblem(t, sessionsWrongMethod, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := sessionsWrongMethod.Header().Get("Allow"); allow != "GET, POST" {
		t.Fatalf("sessions Allow=%q, want GET, POST", allow)
	}
	redirectSpelling := do(http.MethodGet, "/v1/", "", "", nil)
	requireProblem(t, redirectSpelling, http.StatusNotFound, "not_found")
	if redirectSpelling.Header().Get("Location") != "" {
		t.Fatalf("non-canonical path redirected to %q", redirectSpelling.Header().Get("Location"))
	}
}

func TestAdmissionsAcceptExplicitAIConnectionAndOmittedModel(t *testing.T) {
	credential := "dcr_admission-connection"
	base := controlapi.Session{ID: "session-1"}
	tests := []struct {
		name     string
		target   string
		body     string
		sessions *fakeSessions
		got      func(*fakeSessions) string
		model    func(*fakeSessions) string
	}{
		{
			name: "direct", target: "/v1/sessions",
			body:     `{"agents_md":"ship","ai_connection":"work-openai"}`,
			sessions: &fakeSessions{session: controlapi.Session{ID: base.ID}},
			got:      func(j *fakeSessions) string { return j.gotInput.AIConnection },
			model:    func(j *fakeSessions) string { return j.gotInput.Model },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, test.sessions, nil).Handler
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+credential)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "admit-1")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			requireStatusType(t, response, http.StatusCreated, "application/json")
			if got := test.got(test.sessions); got != "work-openai" {
				t.Fatalf("AIConnection=%q, want work-openai", got)
			}
			if got := test.model(test.sessions); got != "" {
				t.Fatalf("Model=%q, want omitted", got)
			}
		})
	}
}

func TestEnrollmentRedemptionUsesDeploymentWideRateLimit(t *testing.T) {
	auth := &fakeAuth{
		client:    controlauth.Client{Name: "laptop"},
		redeemErr: controlauth.ErrEnrollmentUnavailable,
	}
	handler := controlapi.NewServer(controlapi.Discovery{}, auth, &fakeSessions{}, nil).Handler
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

func TestSessionListUsesStrictBoundedQueryAndExplicitEmptyCollection(t *testing.T) {
	credential := "dcr_session-list"
	next := "next-page"
	admittedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	sessions := &fakeSessions{list: controlapi.SessionList{
		Sessions:   []controlapi.SessionSummary{{ID: "session-2", AdmittedAt: admittedAt}},
		NextCursor: &next,
	}}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil).Handler
	do := func(target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	response := do("/v1/sessions?limit=2&cursor=page-one")
	requireStatusType(t, response, http.StatusOK, "application/json")
	var page controlapi.SessionList
	decode(t, response, &page)
	if sessions.listLimit != 2 || sessions.listCursor != "page-one" || len(page.Sessions) != 1 || page.Sessions[0].ID != "session-2" || page.NextCursor == nil || *page.NextCursor != next {
		t.Fatalf("request limit/cursor=%d/%q page=%#v", sessions.listLimit, sessions.listCursor, page)
	}

	sessions.list = controlapi.SessionList{}
	empty := do("/v1/sessions")
	requireStatusType(t, empty, http.StatusOK, "application/json")
	if body := empty.Body.String(); !strings.Contains(body, `"sessions":[]`) || !strings.Contains(body, `"next_cursor":null`) {
		t.Fatalf("empty page omitted explicit collection/cursor: %s", body)
	}
	requireProblem(t, do("/v1/sessions?limit=101"), http.StatusBadRequest, "invalid_query")
	requireProblem(t, do("/v1/sessions?cursor="), http.StatusBadRequest, "invalid_cursor")

	sessions.listErr = controlapi.ErrInvalidCursor
	requireProblem(t, do("/v1/sessions?cursor=tampered"), http.StatusBadRequest, "invalid_cursor")
}

func TestSandboxFileResponseContract(t *testing.T) {
	credential := "dcr_control-client"
	contents := []byte{0x00, 0xff, '\n'}
	sessions := &fakeSessions{session: controlapi.Session{Sandboxes: []controlapi.Sandbox{{ID: "sandbox-1"}}}, file: contents}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil).Handler
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
		!bytes.Equal(response.Body.Bytes(), contents) || sessions.filePath != "nested/REPORT+.bin" {
		t.Fatalf("file response status/type/length/digest/path=%d/%q/%q/%q/%q", response.Code, response.Header().Get("Content-Type"), response.Header().Get("Content-Length"), response.Header().Get("Content-Digest"), sessions.filePath)
	}
	requireProblem(t, get("/v1/sandboxes/sandbox-1/files"), http.StatusBadRequest, "file_path_required")
}

func TestSessionWatchEmitsChangedSnapshotsAndStopsOnServerShutdown(t *testing.T) {
	credential := "dcr_control-client"
	sessions := &fakeSessions{session: controlapi.Session{ID: "session-1", Model: "first", Sandboxes: []controlapi.Sandbox{}}}
	api := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil)

	open := func(lastID string) (*streamResponse, context.CancelFunc, <-chan struct{}) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1/watch", nil).WithContext(ctx)
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
	firstID, firstSession := readSnapshotEvent(t, bufio.NewReader(bytes.NewReader(firstResponse.bytes())))
	if firstSession.Model != "first" || len(firstID) != 64 {
		t.Fatalf("first snapshot id/session=%q/%#v", firstID, firstSession)
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
	sessions.mu.Lock()
	sessions.session.Model = "changed"
	sessions.mu.Unlock()
	resumedResponse.awaitFlush(t)
	changedID, changedSession := readSnapshotEvent(t, bufio.NewReader(bytes.NewReader(resumedResponse.bytes())))
	if changedSession.Model != "changed" || changedID == firstID {
		t.Fatalf("changed snapshot id/session=%q/%#v after %q", changedID, changedSession, firstID)
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

func TestSessionWatchReauthenticatesNoLaterThanCredentialExpiry(t *testing.T) {
	credential := "dcr_expiring-client"
	auth := &fakeAuth{credential: credential, client: controlauth.Client{CredentialExpiresAt: time.Now().Add(100 * time.Millisecond)}}
	sessions := &fakeSessions{session: controlapi.Session{ID: "session-1", Sandboxes: []controlapi.Sandbox{}}}
	api := controlapi.NewServer(controlapi.Discovery{}, auth, sessions, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1/watch", nil)
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
	reconnect := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1/watch", nil)
	reconnect.Header.Set("Authorization", "Bearer "+credential)
	reconnect.Header.Set("Accept", "text/event-stream")
	rejected := httptest.NewRecorder()
	api.Handler.ServeHTTP(rejected, reconnect)
	requireProblem(t, rejected, http.StatusUnauthorized, "unauthenticated")
}

func TestSessionWatchReturnsAuthenticationProblemWhenCredentialExpiresBeforeStreaming(t *testing.T) {
	credential := "dcr_expiring-before-stream"
	auth := &fakeAuth{credential: credential, client: controlauth.Client{CredentialExpiresAt: time.Now().Add(25 * time.Millisecond)}}
	sessions := &fakeSessions{session: controlapi.Session{ID: "session-1"}, waitForGetContext: true}
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1/watch", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()

	controlapi.NewServer(controlapi.Discovery{}, auth, sessions, nil).Handler.ServeHTTP(response, request)

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

func readSnapshotEvent(t *testing.T, reader *bufio.Reader) (string, controlapi.Session) {
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
			var session controlapi.Session
			if err := json.Unmarshal([]byte(data), &session); err != nil {
				t.Fatalf("decode snapshot %q: %v", data, err)
			}
			return id, session
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

type fakeSessions struct {
	execCalls         int
	execCommand       provider.Command
	execErr           error
	mu                sync.Mutex
	session           controlapi.Session
	list              controlapi.SessionList
	listErr           error
	listLimit         int
	listCursor        string
	gotInput          controlapi.CreateSessionRequest
	retry             controlapi.Retry
	file              []byte
	fileWrites        int
	fileAbsent        bool
	filePath          string
	messageKey        string
	retryKey          string
	messageCreated    bool
	messageErr        error
	retryCreated      bool
	cleanupCalls      int
	waitForGetContext bool
}

func (j *fakeSessions) List(_ context.Context, limit int, cursor string) (controlapi.SessionList, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.listLimit, j.listCursor = limit, cursor
	return j.list, j.listErr
}

func (j *fakeSessions) Create(_ context.Context, _ string, _ string, input controlapi.CreateSessionRequest) (controlapi.Session, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.gotInput = input
	return j.session, true, nil
}

func (j *fakeSessions) Get(ctx context.Context, id string) (controlapi.Session, error) {
	j.mu.Lock()
	wait := j.waitForGetContext
	if id != j.session.ID {
		j.mu.Unlock()
		return controlapi.Session{}, controlapi.ErrSessionNotFound
	}
	view := j.session
	j.mu.Unlock()
	if wait {
		<-ctx.Done()
		return controlapi.Session{}, ctx.Err()
	}
	return view, nil
}

func (j *fakeSessions) Retry(_ context.Context, sessionID, key string) (controlapi.Retry, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if sessionID != j.session.ID {
		return controlapi.Retry{}, false, controlapi.ErrSessionNotFound
	}
	j.retryKey = key
	return j.retry, j.retryCreated, nil
}

func (j *fakeSessions) ReadSandboxFile(_ context.Context, sandboxID, path string) ([]byte, error) {
	if len(j.session.Sandboxes) == 0 || sandboxID != j.session.Sandboxes[0].ID {
		return nil, controlapi.ErrSandboxNotFound
	}
	j.filePath = path
	return append([]byte(nil), j.file...), nil
}

func (j *fakeSessions) RequestCleanup(_ context.Context, id string) (controlapi.Session, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cleanupCalls++
	if id != j.session.ID {
		return controlapi.Session{}, controlapi.ErrSessionNotFound
	}
	return j.session, nil
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

type watchDeadlineSessions struct {
	*fakeSessions
	deadline time.Time
}

func (sessions *watchDeadlineSessions) Get(ctx context.Context, _ string) (controlapi.Session, error) {
	sessions.deadline, _ = ctx.Deadline()
	return controlapi.Session{}, controlapi.ErrSessionNotFound
}

func TestNonExpiringClientWatchStillHasAuthenticationDeadline(t *testing.T) {
	credential := "dcr_non-expiring-client"
	auth := &fakeAuth{credential: credential, client: controlauth.Client{}}
	sessions := &watchDeadlineSessions{fakeSessions: &fakeSessions{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/session-1/watch", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "text/event-stream")
	before := time.Now()
	controlapi.NewServer(controlapi.Discovery{}, auth, sessions, nil).Handler.ServeHTTP(httptest.NewRecorder(), request)
	after := time.Now()
	if sessions.deadline.Before(before.Add(time.Minute)) || sessions.deadline.After(after.Add(time.Minute)) {
		t.Fatalf("non-expiring Client watch deadline=%v, want one minute authentication lifetime", sessions.deadline)
	}
}

func (j *fakeSessions) WriteSandboxFile(_ context.Context, sandboxID, name string, contents []byte, ifAbsent bool) error {
	j.fileWrites++
	j.filePath, j.file, j.fileAbsent = name, contents, ifAbsent
	return nil
}

func TestSandboxFileWriteContract(t *testing.T) {
	credential := "dcr_control-client"
	sessions := &fakeSessions{}
	api := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil)
	put := func(token, contentType, condition, content string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/sandbox-1/files?path=SOUL.md", strings.NewReader(content))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", contentType)
		if condition != "" {
			request.Header.Set("If-None-Match", condition)
		}
		response := httptest.NewRecorder()
		api.Handler.ServeHTTP(response, request)
		return response
	}
	if response := put("wrong", "application/octet-stream", "", "private"); response.Code != 401 {
		t.Fatalf("unauthorized=%d", response.Code)
	}
	requireProblem(t, put(credential, "text/plain", "", "text"), 415, "unsupported_media_type")
	requireProblem(t, put(credential, "application/octet-stream", "bad", "text"), 400, "invalid_query")
	requireProblem(t, put(credential, "application/octet-stream", "", strings.Repeat("x", provider.MaxFileWriteBytes+1)), 413, "body_too_large")
	if sessions.fileWrites != 0 {
		t.Fatal("invalid request reached file writer")
	}
	if response := put(credential, "application/octet-stream", "*", "complete\x00bytes"); response.Code != 204 {
		t.Fatalf("write=%d %s", response.Code, response.Body.String())
	}
	if sessions.filePath != "SOUL.md" || string(sessions.file) != "complete\x00bytes" || !sessions.fileAbsent {
		t.Fatalf("write=%+v", sessions)
	}
	if response := put(credential, "application/octet-stream", "", ""); response.Code != 204 {
		t.Fatalf("blank write=%d", response.Code)
	}
	if len(sessions.file) != 0 || sessions.fileAbsent {
		t.Fatal("blank replacement was changed")
	}
}

func (f *fakeSessions) ExecSandbox(_ context.Context, _ string, command provider.Command) (provider.CommandResult, error) {
	f.execCalls++
	f.execCommand = command
	return provider.CommandResult{ExitCode: 7, Stdout: "out", Stderr: "err"}, f.execErr
}

func TestSandboxExecReportsExitStatusAndDoesNotReplayUncertainCommands(t *testing.T) {
	credential := "dcr_control-client"
	sessions := &fakeSessions{}
	api := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil)
	execute := func(token, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sandbox-1/exec", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		api.Handler.ServeHTTP(response, request)
		return response
	}
	if response := execute("wrong", `{"argv":["true"]}`); response.Code != http.StatusUnauthorized || sessions.execCalls != 0 {
		t.Fatal("unauthenticated command executed")
	}
	for _, body := range []string{`{"argv":[]}`, `{"argv":["sleep","1"],"timeout_seconds":121}`, `{"argv":["true"],"host":"elsewhere"}`, `{"argv":["cat"],"stdin":"` + strings.Repeat("a", provider.MaxCommandBytes-2) + `"}`} {
		if response := execute(credential, body); response.Code < 400 || sessions.execCalls != 0 {
			t.Fatal("invalid command executed")
		}
	}
	body := `{"argv":["printf","%s","literal $HOME"],"stdin":"` + strings.Repeat(`\u0000`, provider.MaxCommandBytes-21) + `","timeout_seconds":90}`
	response := execute(credential, body)
	var result provider.CommandResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || result.ExitCode != 7 || result.Stdout != "out" || sessions.execCalls != 1 || sessions.execCommand.Argv[2] != "literal $HOME" {
		t.Fatalf("command result=%+v status=%d err=%v", result, response.Code, err)
	}
	sessions.execErr = controlapi.ErrSandboxExecFailed
	response = execute(credential, `{"argv":["install","something"]}`)
	var problem controlapi.Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil || response.Code != http.StatusBadGateway || problem.Retryable || sessions.execCalls != 2 {
		t.Fatalf("uncertain command was replayed or misreported: %+v calls=%d", problem, sessions.execCalls)
	}
}

func (f *fakeSessions) ReadSandboxStatus(context.Context, string) (provider.Status, error) {
	return provider.Status{Provider: "e2b", State: "paused"}, nil
}
