package controlreader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestAuthenticatedClientReadsExactOwnedFile(t *testing.T) {
	session := core.Session{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", SessionID: session.ID, OwnershipNonce: strings.Repeat("a", 64)}
	store := &readerTestStore{session: session, sandbox: owned}
	files := &readerTestFiles{contents: []byte{0, 1, 255, '\n'}}
	service := Service{Store: store, Runtimes: readerTestRuntimes{profile: session.SandboxProfile, files: files}}
	handler, err := NewHandler(strings.Repeat("b", 64), service)
	if err != nil {
		t.Fatal(err)
	}
	clientHTTP := &http.Client{Transport: readerHandlerTransport{handler: handler}}

	unauthorized, err := NewClient("http://control-reader.test:8756", strings.Repeat("c", 64), clientHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unauthorized.ReadFile(context.Background(), owned.ID, "nested/result.bin"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong-token ReadFile() error=%v", err)
	}
	if files.calls != 0 {
		t.Fatal("unauthenticated request reached provider file authority")
	}

	client, err := NewClient("http://control-reader.test:8756", strings.Repeat("b", 64), clientHTTP)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := client.ReadFile(context.Background(), owned.ID, "nested/result.bin")
	if err != nil || !bytes.Equal(contents, files.contents) {
		t.Fatalf("ReadFile()=%v err=%v", contents, err)
	}
	if files.calls != 1 || files.path != "nested/result.bin" || files.session != session || files.sandbox != owned || store.fences != 1 {
		t.Fatalf("provider call=%d path=%q session=%+v sandbox=%+v fences=%d", files.calls, files.path, files.session, files.sandbox, store.fences)
	}
}

func TestAuthenticatedClientPreservesWholeFileAtReadLimit(t *testing.T) {
	session := core.Session{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", SessionID: session.ID, OwnershipNonce: strings.Repeat("a", 64)}
	want := bytes.Repeat([]byte{0xa5}, provider.MaxFileReadBytes)
	handler, err := NewHandler(strings.Repeat("b", 64), Service{
		Store:    &readerTestStore{session: session, sandbox: owned},
		Runtimes: readerTestRuntimes{profile: session.SandboxProfile, files: &readerTestFiles{contents: want}},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient("http://control-reader.test:8756", strings.Repeat("b", 64), &http.Client{Transport: readerHandlerTransport{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ReadFile(context.Background(), owned.ID, "large.bin")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("whole file bytes=%d want=%d err=%v", len(got), len(want), err)
	}
}

func TestFileReadEnforcesPathOwnershipAndCleanup(t *testing.T) {
	session := core.Session{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", SessionID: session.ID, OwnershipNonce: strings.Repeat("a", 64)}

	t.Run("safe relative path", func(t *testing.T) {
		files := &readerTestFiles{contents: []byte("unused")}
		service := Service{Store: &readerTestStore{session: session, sandbox: owned}, Runtimes: readerTestRuntimes{profile: session.SandboxProfile, files: files}}
		if _, err := service.ReadFile(context.Background(), owned.ID, "../secret"); !errors.Is(err, ErrInvalidFilePath) {
			t.Fatalf("ReadFile() error=%v", err)
		}
		if files.calls != 0 {
			t.Fatal("invalid path reached provider file authority")
		}
	})

	t.Run("cleanup fence", func(t *testing.T) {
		cleaning := session
		cleaning.CleanupState = core.CleanupRequested
		files := &readerTestFiles{contents: []byte("unused")}
		service := Service{Store: &readerTestStore{session: cleaning, sandbox: owned}, Runtimes: readerTestRuntimes{profile: session.SandboxProfile, files: files}}
		if _, err := service.ReadFile(context.Background(), owned.ID, "result.bin"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("ReadFile() error=%v", err)
		}
		if files.calls != 0 {
			t.Fatal("cleanup-fenced read reached provider file authority")
		}
	})

	t.Run("ownership changes under fence", func(t *testing.T) {
		foreign := owned
		foreign.SessionID = "job-foreign"
		files := &readerTestFiles{contents: []byte("unused")}
		store := &readerTestStore{session: session, sandbox: owned, sandboxInsideFence: &foreign}
		service := Service{Store: store, Runtimes: readerTestRuntimes{profile: session.SandboxProfile, files: files}}
		if _, err := service.ReadFile(context.Background(), owned.ID, "result.bin"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("ReadFile() error=%v", err)
		}
		if files.calls != 0 {
			t.Fatal("foreign Sandbox reached provider file authority")
		}
	})

	t.Run("missing ownership proof", func(t *testing.T) {
		unproven := owned
		unproven.OwnershipNonce = ""
		files := &readerTestFiles{contents: []byte("unused")}
		service := Service{Store: &readerTestStore{session: session, sandbox: unproven}, Runtimes: readerTestRuntimes{profile: session.SandboxProfile, files: files}}
		if _, err := service.ReadFile(context.Background(), owned.ID, "result.bin"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("ReadFile() error=%v", err)
		}
		if files.calls != 0 {
			t.Fatal("unproven Sandbox reached provider file authority")
		}
	})

}

func TestAuthenticatedClientUsesFixedAdmissionObservations(t *testing.T) {
	provider := &readerTestProvider{defaultConnection: "primary", defaultModel: "gpt-5.6-sol"}
	handler, err := NewHandler(strings.Repeat("f", 64), Service{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	clientHTTP := &http.Client{Transport: readerHandlerTransport{handler: handler}}
	client, err := NewClient("http://control-reader.test:8756", strings.Repeat("f", 64), clientHTTP)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := client.DefaultConnection()
	if err != nil || connection != "primary" {
		t.Fatalf("DefaultConnection()=%q err=%v", connection, err)
	}
	model, err := client.DefaultModel(connection)
	if err != nil || model != "gpt-5.6-sol" || provider.modelConnection != connection {
		t.Fatalf("DefaultModel()=%q err=%v connection=%q", model, err, provider.modelConnection)
	}
	if err := client.Check(context.Background(), connection); err != nil || provider.checked != connection {
		t.Fatalf("Check() error=%v checked=%q", err, provider.checked)
	}
}

func TestHandlerRejectsInvalidBoundaryRequestsBeforeAuthority(t *testing.T) {
	store := &readerTestStore{}
	handler, err := NewHandler(strings.Repeat("e", 64), Service{Store: store})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/files/read", strings.NewReader(strings.Repeat("x", MaxRequestBytes+1)))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("e", 64))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/files/read", strings.NewReader(`{"sandbox_id":"sandbox-1","path":"result.bin","provider":"incus"}`))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("e", 64))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("generic provider field status=%d body=%s", response.Code, response.Body.String())
	}
	if store.sandboxCalls != 0 {
		t.Fatal("invalid internal request reached durable custody")
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/files/read?provider=incus", strings.NewReader(`{"sandbox_id":"sandbox-1","path":"result.bin"}`))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("e", 64))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("generic query status=%d body=%s", response.Code, response.Body.String())
	}
	if store.sandboxCalls != 0 {
		t.Fatal("generic internal query reached durable custody")
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/files%2fread", strings.NewReader(`{"sandbox_id":"sandbox-1","path":"result.bin"}`))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("e", 64))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("encoded route status=%d body=%s", response.Code, response.Body.String())
	}
	if store.sandboxCalls != 0 {
		t.Fatal("encoded internal route reached durable custody")
	}

	for _, test := range []struct {
		name, method, path, contentType, wantCode string
		wantStatus                                int
	}{
		{name: "unknown path", method: http.MethodPost, path: "/v1/unknown", contentType: "application/json", wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "wrong method", method: http.MethodGet, path: FileReadPath, contentType: "application/json", wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed"},
		{name: "invalid content type", method: http.MethodPost, path: FileReadPath, contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType, wantCode: "invalid_content_type"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(`{"sandbox_id":"sandbox-1","path":"result.bin"}`))
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("e", 64))
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			var got problem
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || got.Code != test.wantCode || response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("problem=%+v content_type=%q err=%v", got, response.Header().Get("Content-Type"), err)
			}
			if test.wantStatus == http.StatusMethodNotAllowed && response.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("Allow=%q", response.Header().Get("Allow"))
			}
			if store.sandboxCalls != 0 {
				t.Fatal("rejected request reached durable custody")
			}
		})
	}
}

func TestHandlerAppliesProviderWorkDeadline(t *testing.T) {
	provider := &readerTestProvider{}
	handler, err := NewHandler(strings.Repeat("a", 64), Service{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, ConnectionCheckPath, strings.NewReader(`{"connection":"primary"}`))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if provider.deadline.IsZero() || provider.deadline.Before(started.Add(handlerTimeout-time.Second)) || provider.deadline.After(started.Add(handlerTimeout+time.Second)) {
		t.Fatalf("provider deadline=%s started=%s", provider.deadline, started)
	}
}

func TestClientDisablesRedirectsForInternalCredential(t *testing.T) {
	client, err := NewClient("http://control-reader:8756", strings.Repeat("a", 64), &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if client.http.CheckRedirect == nil {
		t.Fatal("internal control reader client follows redirects")
	}
	if err := client.http.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error=%v", err)
	}
	if client.http.Timeout != 20*time.Second {
		t.Fatalf("internal client timeout=%s", client.http.Timeout)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("internal client transport=%T proxy=%v", client.http.Transport, ok && transport.Proxy != nil)
	}
}

func TestClientRequiresExactProblemResponse(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		contentType string
		body        string
		wantInvalid bool
	}{
		{name: "valid", status: http.StatusUnprocessableEntity, contentType: "application/json", body: `{"code":"invalid_request"}`, wantInvalid: true},
		{name: "wrong status", status: http.StatusUnauthorized, contentType: "application/json", body: `{"code":"invalid_request"}`},
		{name: "content type", status: http.StatusUnprocessableEntity, contentType: "text/plain", body: `{"code":"invalid_request"}`},
		{name: "trailing JSON", status: http.StatusUnprocessableEntity, contentType: "application/json", body: `{"code":"invalid_request"}{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient("http://control-reader:8756", strings.Repeat("a", 64), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: test.status,
					Header:     http.Header{"Content-Type": []string{test.contentType}},
					Body:       io.NopCloser(strings.NewReader(test.body)),
				}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			err = client.Check(context.Background(), "primary")
			if errors.Is(err, ErrInvalidRequest) != test.wantInvalid {
				t.Fatalf("Check() error=%v want invalid=%t", err, test.wantInvalid)
			}
		})
	}
}

type readerTestStore struct {
	deliveryHeld       bool
	activityStarts     int
	activityFinishes   int
	session            core.Session
	sandbox            core.Sandbox
	sandboxInsideFence *core.Sandbox
	inFence            bool
	fences             int
	sandboxCalls       int
	executionCalls     int
}

func (s *readerTestStore) Session(_ context.Context, id string) (core.Session, error) {
	if s.session.ID == "" || s.session.ID != id {
		return core.Session{}, postgres.ErrNotFound
	}
	return s.session, nil
}

func (s *readerTestStore) Sandbox(_ context.Context, id string) (core.Sandbox, error) {
	s.sandboxCalls++
	if s.sandbox.ID == "" || s.sandbox.ID != id {
		return core.Sandbox{}, postgres.ErrNotFound
	}
	if s.inFence && s.sandboxInsideFence != nil {
		return *s.sandboxInsideFence, nil
	}
	return s.sandbox, nil
}

func (s *readerTestStore) WithSessionFence(_ context.Context, _ string, run func() error) error {
	s.fences++
	s.inFence = true
	defer func() { s.inFence = false }()
	return run()
}

type readerTestRuntimes struct {
	profile   string
	files     core.SandboxFileReader
	execution core.Execution
	commands  core.SandboxCommandExecutor
	status    core.SandboxStatusReader
}

func (r readerTestRuntimes) ResolveSandbox(_ context.Context, profile core.SandboxProfileRef) (core.SandboxRuntime, error) {
	if profile.Name != r.profile {
		return core.SandboxRuntime{}, errors.New("foreign profile")
	}
	return core.SandboxRuntime{SandboxProfile: profile, Files: r.files, Execution: r.execution, Commands: r.commands, Status: r.status}, nil
}

type readerTestFiles struct {
	contents []byte
	calls    int
	path     string
	session  core.Session
	sandbox  core.Sandbox
}

func (r *readerTestFiles) ReadSandboxFile(_ context.Context, session core.Session, sandbox core.Sandbox, path string) ([]byte, error) {
	r.calls++
	r.path, r.session, r.sandbox = path, session, sandbox
	return append([]byte(nil), r.contents...), nil
}

type readerHandlerTransport struct{ handler http.Handler }

func (t readerHandlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	t.handler.ServeHTTP(response, request)
	return response.Result(), nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type readerTestProvider struct {
	defaultConnection string
	defaultModel      string
	modelConnection   string
	checked           string
	deadline          time.Time
}

func (p *readerTestProvider) DefaultConnection() (string, error) { return p.defaultConnection, nil }

func (p *readerTestProvider) DefaultModel(connection string) (string, error) {
	p.modelConnection = connection
	return p.defaultModel, nil
}

func (p *readerTestProvider) Check(ctx context.Context, connection string) error {
	p.checked = connection
	p.deadline, _ = ctx.Deadline()
	return nil
}

func (r *readerTestFiles) WriteSandboxFile(_ context.Context, session core.Session, sandbox core.Sandbox, path string, contents []byte, ifAbsent bool) error {
	r.calls++
	r.path, r.session, r.sandbox = path, session, sandbox
	r.contents = append([]byte(nil), contents...)
	return nil
}

func TestFileWritesUseAuthenticatedOwnershipAndCleanupFence(t *testing.T) {
	session := core.Session{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", SessionID: session.ID, OwnershipNonce: strings.Repeat("a", 64)}
	files := &readerTestFiles{}
	store := &readerTestStore{session: session, sandbox: owned}
	service := Service{Store: store, Runtimes: readerTestRuntimes{profile: session.SandboxProfile, files: files}}
	handler, err := NewHandler(strings.Repeat("b", 64), service)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient("http://control-reader.test:8756", strings.Repeat("b", 64), &http.Client{Transport: readerHandlerTransport{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("instructions\n"), 5000)
	if err := client.WriteFile(context.Background(), owned.ID, "~/.config/agent0/access.json", want, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(files.contents, want) || files.session != session || files.sandbox != owned || store.fences != 1 {
		t.Fatal("write lost exact bytes or ownership fence")
	}
	for _, name := range []string{"../escape", "nested/../file"} {
		if err := client.WriteFile(context.Background(), owned.ID, name, nil, false); !errors.Is(err, ErrInvalidFilePath) {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	store.session.CleanupState = core.CleanupRequested
	if err := client.WriteFile(context.Background(), owned.ID, "SOUL.md", nil, false); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cleanup write: %v", err)
	}
	if files.calls != 1 {
		t.Fatal("rejected write reached provider")
	}
}

func (s *readerTestStore) BeginSandboxActivity(_ context.Context, sessionID string) error {
	if !s.inFence || sessionID != s.session.ID {
		panic("activity outside exact Session fence")
	}
	s.activityStarts++
	return nil
}
func (s *readerTestStore) FinishSandboxActivity(ctx context.Context, sessionID string) error {
	if !s.inFence || sessionID != s.session.ID || ctx.Err() != nil {
		panic("activity completion lost fence or cancellation protection")
	}
	s.activityFinishes++
	return nil
}

func (s *readerTestStore) SandboxDeliveryHeld(context.Context, string) (bool, error) {
	return s.deliveryHeld, nil
}
