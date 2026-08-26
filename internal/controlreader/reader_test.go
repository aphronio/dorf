package controlreader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestAuthenticatedClientReadsExactOwnedFile(t *testing.T) {
	job := core.Job{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	store := &readerTestStore{job: job, sandbox: owned}
	files := &readerTestFiles{contents: []byte{0, 1, 255, '\n'}}
	service := Service{Store: store, Runtimes: readerTestRuntimes{profile: job.SandboxProfile, files: files}}
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
	if files.calls != 1 || files.path != "nested/result.bin" || files.job != job || files.sandbox != owned || store.fences != 1 {
		t.Fatalf("provider call=%d path=%q job=%+v sandbox=%+v fences=%d", files.calls, files.path, files.job, files.sandbox, store.fences)
	}
}

func TestAuthenticatedClientPreservesWholeFileBeyondMessageObservationBound(t *testing.T) {
	job := core.Job{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	want := bytes.Repeat([]byte{0xa5}, MaxObservationBytes+1)
	handler, err := NewHandler(strings.Repeat("b", 64), Service{
		Store:    &readerTestStore{job: job, sandbox: owned},
		Runtimes: readerTestRuntimes{profile: job.SandboxProfile, files: &readerTestFiles{contents: want}},
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
	job := core.Job{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}

	t.Run("safe relative path", func(t *testing.T) {
		files := &readerTestFiles{contents: []byte("unused")}
		service := Service{Store: &readerTestStore{job: job, sandbox: owned}, Runtimes: readerTestRuntimes{profile: job.SandboxProfile, files: files}}
		if _, err := service.ReadFile(context.Background(), owned.ID, "../secret"); !errors.Is(err, ErrInvalidFilePath) {
			t.Fatalf("ReadFile() error=%v", err)
		}
		if files.calls != 0 {
			t.Fatal("invalid path reached provider file authority")
		}
	})

	t.Run("cleanup fence", func(t *testing.T) {
		cleaning := job
		cleaning.CleanupState = core.CleanupRequested
		files := &readerTestFiles{contents: []byte("unused")}
		service := Service{Store: &readerTestStore{job: cleaning, sandbox: owned}, Runtimes: readerTestRuntimes{profile: job.SandboxProfile, files: files}}
		if _, err := service.ReadFile(context.Background(), owned.ID, "result.bin"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("ReadFile() error=%v", err)
		}
		if files.calls != 0 {
			t.Fatal("cleanup-fenced read reached provider file authority")
		}
	})

	t.Run("ownership changes under fence", func(t *testing.T) {
		foreign := owned
		foreign.JobID = "job-foreign"
		files := &readerTestFiles{contents: []byte("unused")}
		store := &readerTestStore{job: job, sandbox: owned, sandboxInsideFence: &foreign}
		service := Service{Store: store, Runtimes: readerTestRuntimes{profile: job.SandboxProfile, files: files}}
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
		service := Service{Store: &readerTestStore{job: job, sandbox: unproven}, Runtimes: readerTestRuntimes{profile: job.SandboxProfile, files: files}}
		if _, err := service.ReadFile(context.Background(), owned.ID, "result.bin"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("ReadFile() error=%v", err)
		}
		if files.calls != 0 {
			t.Fatal("unproven Sandbox reached provider file authority")
		}
	})

}

func TestAuthenticatedClientObservesOnlyExactOwnedMessageWithBoundedResult(t *testing.T) {
	job := core.Job{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	delivery := core.Delivery{
		Message:  core.Message{ID: "message-1", JobID: job.ID},
		AgentRun: core.AgentRun{ID: "run-1", JobID: job.ID, MessageID: "message-1", SandboxID: owned.ID, State: core.AgentRunCompleted, TurnOutcome: "completed"},
	}
	observation := &readerTestObservation{result: core.MessageResult{MessageID: "message-1", Outcome: "completed", Output: "exact output"}}
	store := &readerTestStore{job: job, sandbox: owned, execution: core.AgentMessageExecution{
		Job: job, Message: delivery.Message, AgentRun: delivery.AgentRun, Sandbox: owned,
	}}
	service := Service{
		Store:    store,
		Runtimes: readerTestRuntimes{profile: job.SandboxProfile, files: &readerTestFiles{}, execution: observation},
	}
	handler, err := NewHandler(strings.Repeat("d", 64), service)
	if err != nil {
		t.Fatal(err)
	}
	clientHTTP := &http.Client{Transport: readerHandlerTransport{handler: handler}}
	client, err := NewClient("http://control-reader.test:8756", strings.Repeat("d", 64), clientHTTP)
	if err != nil {
		t.Fatal(err)
	}

	result, err := client.ObserveMessage(context.Background(), job.ID, "message-1")
	if err != nil || result != observation.result || observation.jobID != job.ID || observation.messageID != "message-1" || store.fences != 1 || store.executionCalls != 1 {
		t.Fatalf("ObserveMessage()=%+v err=%v call=%q/%q fences=%d aggregate_calls=%d", result, err, observation.jobID, observation.messageID, store.fences, store.executionCalls)
	}

	observation.result.Output = strings.Repeat("x", MaxObservationBytes+1)
	if _, err := client.ObserveMessage(context.Background(), job.ID, "message-1"); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized ObserveMessage() error=%v", err)
	}
	if _, err := client.ObserveMessage(context.Background(), job.ID, "message-foreign"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("foreign ObserveMessage() error=%v", err)
	}

	observation.result = core.MessageResult{MessageID: "message-1"}
	if _, err := client.ObserveMessage(context.Background(), job.ID, "message-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unsettled ObserveMessage() error=%v", err)
	}

	observation.result = core.MessageResult{MessageID: "message-1", Outcome: "failed"}
	if _, err := client.ObserveMessage(context.Background(), job.ID, "message-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("drifted ObserveMessage() error=%v", err)
	}
}

func TestMessageObservationRequiresDurableCompletedOwnershipBeforeProvider(t *testing.T) {
	job := core.Job{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	observation := &readerTestObservation{result: core.MessageResult{MessageID: "message-1", Outcome: "completed"}}
	service := Service{
		Store: &readerTestStore{job: job, execution: core.AgentMessageExecution{
			Job:     job,
			Message: core.Message{ID: "message-1", JobID: "foreign-job"},
			AgentRun: core.AgentRun{
				ID: "run-1", JobID: job.ID, MessageID: "message-1", SandboxID: "sandbox-1", State: core.AgentRunCompleted,
			},
			Sandbox: core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)},
		}},
		Runtimes: readerTestRuntimes{profile: job.SandboxProfile, execution: observation},
	}
	if _, err := service.ObserveMessage(context.Background(), job.ID, "message-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ObserveMessage() error=%v", err)
	}
	if observation.messageID != "" {
		t.Fatal("foreign durable Message reached provider observation")
	}

	pending := service
	pending.Store = &readerTestStore{job: job, execution: core.AgentMessageExecution{
		Job:     job,
		Message: core.Message{ID: "message-1", JobID: job.ID},
		AgentRun: core.AgentRun{
			ID: "run-1", JobID: job.ID, MessageID: "message-1", SandboxID: "sandbox-1", State: core.AgentRunActive,
		},
		Sandbox: core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)},
	}}
	if _, err := pending.ObserveMessage(context.Background(), job.ID, "message-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pending ObserveMessage() error=%v", err)
	}
	if observation.messageID != "" {
		t.Fatal("unsettled durable Message reached provider observation")
	}
}

func TestAuthenticatedClientUsesFixedAdmissionObservations(t *testing.T) {
	provider := &readerTestProvider{defaultConnection: "primary"}
	installations := &readerTestInstallations{installation: "42"}
	handler, err := NewHandler(strings.Repeat("f", 64), Service{Provider: provider, Installations: installations})
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
	if err := client.Check(context.Background(), connection); err != nil || provider.checked != connection {
		t.Fatalf("Check() error=%v checked=%q", err, provider.checked)
	}
	installation, err := client.DiscoverInstallation(context.Background(), "aphronio/dorf")
	if err != nil || installation != "42" || installations.repository != "aphronio/dorf" {
		t.Fatalf("DiscoverInstallation()=%q err=%v repository=%q", installation, err, installations.repository)
	}
}

func TestAuthenticatedClientProvesReaderHealthWithoutExternalAuthority(t *testing.T) {
	handler, err := NewHandler(strings.Repeat("a", 64), Service{})
	if err != nil {
		t.Fatal(err)
	}
	clientHTTP := &http.Client{Transport: readerHandlerTransport{handler: handler}}
	client, err := NewClient("http://control-reader.test:8756", strings.Repeat("a", 64), clientHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	unauthorized, err := NewClient("http://control-reader.test:8756", strings.Repeat("b", 64), clientHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if err := unauthorized.Health(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized Health() error=%v", err)
	}
}

func TestHandlerRejectsOversizedAndUnknownRequestsBeforeAuthority(t *testing.T) {
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

func TestHandlerBoundsEncodedMessageObservation(t *testing.T) {
	job := core.Job{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	delivery := core.Delivery{
		Message:  core.Message{ID: "message-1", JobID: job.ID},
		AgentRun: core.AgentRun{ID: "run-1", JobID: job.ID, MessageID: "message-1", State: core.AgentRunCompleted, TurnOutcome: "completed"},
	}
	owned := core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	delivery.AgentRun.SandboxID = owned.ID
	observation := &readerTestObservation{result: core.MessageResult{
		MessageID: "message-1", Outcome: "completed", Output: strings.Repeat("\x00", MaxObservationBytes/6+1),
	}}
	handler, err := NewHandler(strings.Repeat("a", 64), Service{
		Store: &readerTestStore{job: job, execution: core.AgentMessageExecution{
			Job: job, Message: delivery.Message, AgentRun: delivery.AgentRun, Sandbox: owned,
		}},
		Runtimes: readerTestRuntimes{profile: job.SandboxProfile, execution: observation},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, MessageObservationPath, strings.NewReader(`{"job_id":"job-1","message_id":"message-1"}`))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || response.Body.Len() > maxProblemBytes {
		t.Fatalf("encoded observation status=%d bytes=%d", response.Code, response.Body.Len())
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

func TestClientRequiresExactJSONMessageResponse(t *testing.T) {
	for _, test := range []struct {
		name, contentType, body string
	}{
		{name: "content type", contentType: "text/plain", body: `{"message_id":"message-1","outcome":"completed"}`},
		{name: "trailing JSON", contentType: "application/json", body: `{"message_id":"message-1","outcome":"completed"}{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient("http://control-reader:8756", strings.Repeat("a", 64), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{test.contentType}},
					Body:       io.NopCloser(strings.NewReader(test.body)),
				}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ObserveMessage(context.Background(), "job-1", "message-1"); err == nil {
				t.Fatal("malformed Message response was accepted")
			}
		})
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
	job                core.Job
	sandbox            core.Sandbox
	sandboxInsideFence *core.Sandbox
	inFence            bool
	fences             int
	sandboxCalls       int
	execution          core.AgentMessageExecution
	executionCalls     int
}

func (s *readerTestStore) Job(_ context.Context, id string) (core.Job, error) {
	if s.job.ID == "" || s.job.ID != id {
		return core.Job{}, postgres.ErrNotFound
	}
	return s.job, nil
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

func (s *readerTestStore) WithJobFence(_ context.Context, _ string, run func() error) error {
	s.fences++
	s.inFence = true
	defer func() { s.inFence = false }()
	return run()
}

func (s *readerTestStore) AgentMessageExecution(_ context.Context, messageID string) (core.AgentMessageExecution, error) {
	s.executionCalls++
	if s.execution.Message.ID == "" || s.execution.Message.ID != messageID {
		return core.AgentMessageExecution{}, postgres.ErrNotFound
	}
	return s.execution, nil
}

type readerTestRuntimes struct {
	profile   string
	files     core.SandboxFileReader
	execution core.Execution
}

func (r readerTestRuntimes) ResolveSandbox(_ context.Context, profile string) (core.SandboxRuntime, error) {
	if profile != r.profile {
		return core.SandboxRuntime{}, errors.New("foreign profile")
	}
	return core.SandboxRuntime{SandboxProfile: profile, Files: r.files, Execution: r.execution}, nil
}

type readerTestFiles struct {
	contents []byte
	calls    int
	path     string
	job      core.Job
	sandbox  core.Sandbox
}

func (r *readerTestFiles) ReadSandboxFile(_ context.Context, job core.Job, sandbox core.Sandbox, path string) ([]byte, error) {
	r.calls++
	r.path, r.job, r.sandbox = path, job, sandbox
	return append([]byte(nil), r.contents...), nil
}

type readerTestObservation struct {
	result    core.MessageResult
	jobID     string
	messageID string
}

func (o *readerTestObservation) ObserveSettledAgentMessage(_ context.Context, jobID, messageID string) (core.MessageResult, error) {
	o.jobID, o.messageID = jobID, messageID
	if o.result.MessageID != messageID {
		return core.MessageResult{}, ErrUnavailable
	}
	return o.result, nil
}

func (*readerTestObservation) ExecuteSandboxAction(context.Context, string, string, core.ActionKind) error {
	return nil
}

func (*readerTestObservation) ExecuteSandboxActionEffect(context.Context, string, string, core.ActionKind, core.SandboxActionEffect) error {
	return nil
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
	checked           string
	deadline          time.Time
}

func (p *readerTestProvider) DefaultConnection() (string, error) { return p.defaultConnection, nil }

func (p *readerTestProvider) Check(ctx context.Context, connection string) error {
	p.checked = connection
	p.deadline, _ = ctx.Deadline()
	return nil
}

type readerTestInstallations struct {
	installation string
	repository   string
}

func (i *readerTestInstallations) DiscoverInstallation(_ context.Context, repository string) (string, error) {
	i.repository = repository
	return i.installation, nil
}
