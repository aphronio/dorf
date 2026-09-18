package controlapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlclient"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestPublicFileReadRejectsOversizedSessionsResult(t *testing.T) {
	const credential = "dcr_file-bound"
	sessions := &fileBudgetSessions{
		fakeSessions: &fakeSessions{session: controlapi.Session{ID: "job-1", Sandboxes: []controlapi.Sandbox{{ID: "sandbox-1"}}}},
		contents:     bytes.Repeat([]byte{'x'}, provider.MaxFileReadBytes+1),
	}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil).Handler
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, publicFileRequest(context.Background(), credential))
	requireProblem(t, response, http.StatusConflict, "file_too_large")
	if response.Header().Get("Content-Digest") != "" {
		t.Fatal("oversized custom Sessions result committed file headers")
	}
}

func TestPublicFileTooLargeRetainsProblemAndTypedClientError(t *testing.T) {
	const credential = "dcr_file-too-large"
	sessions := &fileBudgetSessions{
		fakeSessions: &fakeSessions{session: controlapi.Session{ID: "job-1", Sandboxes: []controlapi.Sandbox{{ID: "sandbox-1"}}}},
		err:          controlapi.ErrFileTooLarge,
	}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil).Handler
	client, err := controlclient.New("https://dorf.example.test", credential, publicHandlerTransport{handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := client.SandboxFile(context.Background(), "sandbox-1", "result.bin")
	var problem *controlclient.ProblemError
	if len(contents) != 0 || !errors.Is(err, controlapi.ErrFileTooLarge) || !errors.As(err, &problem) || problem.Problem.Code != "file_too_large" || !controlclient.IsServiceError(err) {
		t.Fatalf("SandboxFile() bytes=%d err=%v problem=%#v service=%t", len(contents), err, problem, controlclient.IsServiceError(err))
	}
}

func TestPublicFileReadTransferBudgetIncludesSlowWrite(t *testing.T) {
	const credential = "dcr_file-budget"
	sessions := &fileBudgetSessions{
		fakeSessions: &fakeSessions{session: controlapi.Session{ID: "job-1", Sandboxes: []controlapi.Sandbox{{ID: "sandbox-1"}}}},
		contents:     []byte("exact"), entered: make(chan struct{}, 16),
	}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil).Handler

	var group sync.WaitGroup
	releases := make([]*publicReleaseSignal, 0, provider.MaxConcurrentFileReads+1)
	start := func(ctx context.Context, writer http.ResponseWriter) {
		group.Add(1)
		go func() {
			defer group.Done()
			handler.ServeHTTP(writer, publicFileRequest(ctx, credential))
		}()
	}
	for range provider.MaxConcurrentFileReads {
		release := newPublicReleaseSignal()
		releases = append(releases, release)
		start(context.Background(), &publicBlockingWriter{header: make(http.Header), entered: make(chan struct{}, 1), release: release.ch})
	}
	t.Cleanup(func() {
		for _, release := range releases {
			release.close()
		}
		group.Wait()
	})
	for range provider.MaxConcurrentFileReads {
		waitPublicSignal(t, sessions.entered, "initial Sessions file read")
	}
	waitPublicAtomic(t, &sessions.calls, provider.MaxConcurrentFileReads, "initial Sessions calls")

	me := httptest.NewRecorder()
	meDone := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		handler.ServeHTTP(me, request)
		close(meDone)
	}()
	waitPublicSignal(t, meDone, "non-file control")
	if me.Code != http.StatusOK {
		t.Fatalf("GET /v1/me status=%d body=%s", me.Code, me.Body.String())
	}

	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	defer cancelQueued()
	queuedDone := make(chan struct{})
	group.Add(1)
	go func() {
		defer group.Done()
		defer close(queuedDone)
		handler.ServeHTTP(httptest.NewRecorder(), publicFileRequest(queuedCtx, credential))
	}()
	assertNoPublicSignal(t, sessions.entered, "queued Sessions read")
	cancelQueued()
	waitPublicSignal(t, queuedDone, "queued cancellation")

	nextRelease := newPublicReleaseSignal()
	releases = append(releases, nextRelease)
	nextWriter := &publicBlockingWriter{header: make(http.Header), entered: make(chan struct{}, 1), release: nextRelease.ch}
	start(context.Background(), nextWriter)
	assertNoPublicSignal(t, sessions.entered, "fifth live Sessions read before release")
	releases[0].close()
	waitPublicSignal(t, sessions.entered, "next Sessions read after release")
	waitPublicSignal(t, nextWriter.entered, "next blocked response write")
}

func TestOpenAPIFileReadPublishesBoundAndTypedConflict(t *testing.T) {
	var document map[string]any
	if err := json.Unmarshal(controlapi.OpenAPIDocument(), &document); err != nil {
		t.Fatal(err)
	}
	fileGet := document["paths"].(map[string]any)["/v1/sandboxes/{sandbox}/files"].(map[string]any)["get"].(map[string]any)
	responses := fileGet["responses"].(map[string]any)
	if _, ok := responses["409"]; !ok {
		t.Fatal("file GET omits its explicit 409 Problem response")
	}
	schema := responses["200"].(map[string]any)["content"].(map[string]any)["application/octet-stream"].(map[string]any)["schema"].(map[string]any)
	if schema["maxLength"] != float64(provider.MaxFileReadBytes) {
		t.Fatalf("file maxLength=%v want %d", schema["maxLength"], provider.MaxFileReadBytes)
	}
}

type fileBudgetSessions struct {
	*fakeSessions
	contents []byte
	err      error
	entered  chan struct{}
	calls    atomic.Int32
}

func (j *fileBudgetSessions) ReadSandboxFile(context.Context, string, string) ([]byte, error) {
	j.calls.Add(1)
	if j.entered != nil {
		j.entered <- struct{}{}
	}
	return append([]byte(nil), j.contents...), j.err
}

type publicHandlerTransport struct{ handler http.Handler }

func (t publicHandlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	t.handler.ServeHTTP(response, request)
	return response.Result(), nil
}

func publicFileRequest(ctx context.Context, credential string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sandbox-1/files?path=result.bin", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+credential)
	return request
}

type publicBlockingWriter struct {
	header  http.Header
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *publicBlockingWriter) Header() http.Header { return w.header }
func (*publicBlockingWriter) WriteHeader(int)       {}
func (w *publicBlockingWriter) Write(contents []byte) (int, error) {
	w.once.Do(func() { w.entered <- struct{}{} })
	<-w.release
	return len(contents), nil
}

type publicReleaseSignal struct {
	ch   chan struct{}
	once sync.Once
}

func newPublicReleaseSignal() *publicReleaseSignal {
	return &publicReleaseSignal{ch: make(chan struct{})}
}
func (s *publicReleaseSignal) close() { s.once.Do(func() { close(s.ch) }) }

func waitPublicSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func assertNoPublicSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
		t.Fatalf("unexpected %s", what)
	case <-time.After(50 * time.Millisecond):
	}
}

func waitPublicAtomic(t *testing.T, value *atomic.Int32, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for value.Load() != int32(want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: got %d want %d", what, value.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

var _ controlapi.Sessions = (*fileBudgetSessions)(nil)
