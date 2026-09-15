package controlreader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestReadFileClientEnforcesFileBoundAndExactLength(t *testing.T) {
	token := strings.Repeat("a", 64)
	tests := []struct {
		name          string
		body          []byte
		contentLength int64
		status        int
		contentType   string
		wantTooLarge  bool
		wantErr       bool
	}{
		{name: "exact limit", body: bytes.Repeat([]byte{'x'}, provider.MaxFileReadBytes), contentLength: provider.MaxFileReadBytes, status: http.StatusOK, contentType: "application/octet-stream"},
		{name: "missing length plus one", body: bytes.Repeat([]byte{'x'}, provider.MaxFileReadBytes+1), contentLength: -1, status: http.StatusOK, contentType: "application/octet-stream", wantTooLarge: true, wantErr: true},
		{name: "lying short length plus one", body: bytes.Repeat([]byte{'x'}, provider.MaxFileReadBytes+1), contentLength: provider.MaxFileReadBytes - 1, status: http.StatusOK, contentType: "application/octet-stream", wantTooLarge: true, wantErr: true},
		{name: "advertised oversize", body: []byte("unused"), contentLength: provider.MaxFileReadBytes + 1, status: http.StatusOK, contentType: "application/octet-stream", wantTooLarge: true, wantErr: true},
		{name: "conflicting short body", body: []byte("short"), contentLength: 6, status: http.StatusOK, contentType: "application/octet-stream", wantErr: true},
		{name: "typed problem", body: []byte("{\"code\":\"file_too_large\"}\n"), contentLength: -1, status: http.StatusConflict, contentType: "application/json", wantTooLarge: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient("http://control-reader.test:8756", token, &http.Client{Transport: fileRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, Header: http.Header{"Content-Type": []string{test.contentType}}, Body: io.NopCloser(bytes.NewReader(test.body)), ContentLength: test.contentLength}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			contents, err := client.ReadFile(context.Background(), "sandbox-1", "result.bin")
			if (err != nil) != test.wantErr || errors.Is(err, ErrFileTooLarge) != test.wantTooLarge {
				t.Fatalf("ReadFile() bytes=%d err=%v, want error=%t too_large=%t", len(contents), err, test.wantErr, test.wantTooLarge)
			}
			if err == nil && !bytes.Equal(contents, test.body) {
				t.Fatal("ReadFile() did not preserve exact-limit bytes")
			}
		})
	}
}

func TestReadFileServiceMapsAndDefendsTheProviderBound(t *testing.T) {
	job, owned := boundedReaderAuthority()
	for _, test := range []struct {
		name     string
		files    core.SandboxFileReader
		wantSent bool
	}{
		{name: "provider sentinel", files: &boundedFiles{err: provider.ErrFileTooLarge}, wantSent: true},
		{name: "custom oversized runtime", files: &boundedFiles{contents: bytes.Repeat([]byte{'x'}, provider.MaxFileReadBytes+1)}, wantSent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := Service{Store: &boundedStore{job: job, sandbox: owned}, Runtimes: boundedRuntimes{profile: job.SandboxProfile, files: test.files}}
			contents, err := service.ReadFile(context.Background(), owned.ID, "result.bin")
			if len(contents) != 0 || errors.Is(err, ErrFileTooLarge) != test.wantSent {
				t.Fatalf("ReadFile() bytes=%d err=%v", len(contents), err)
			}
		})
	}
}

func TestPrivateFileTooLargeRoundTripsThroughHandlerAndClient(t *testing.T) {
	job, owned := boundedReaderAuthority()
	token := strings.Repeat("c", 64)
	handler, err := NewHandler(token, Service{
		Store:    &boundedStore{job: job, sandbox: owned},
		Runtimes: boundedRuntimes{profile: job.SandboxProfile, files: &boundedFiles{err: provider.ErrFileTooLarge}},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient("http://control-reader.test:8756", token, &http.Client{Transport: readerHandlerTransport{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := client.ReadFile(context.Background(), owned.ID, "result.bin")
	if len(contents) != 0 || !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadFile() bytes=%d err=%v", len(contents), err)
	}
}

func TestPrivateFileReadTransferBudgetKeepsFenceFreeDuringSlowWrite(t *testing.T) {
	job, owned := boundedReaderAuthority()
	files := &boundedFiles{contents: []byte("exact"), entered: make(chan struct{}, 16)}
	store := &boundedStore{job: job, sandbox: owned}
	handler, err := NewHandler(strings.Repeat("b", 64), Service{Store: store, Runtimes: boundedRuntimes{profile: job.SandboxProfile, files: files}})
	if err != nil {
		t.Fatal(err)
	}

	var group sync.WaitGroup
	releases := make([]*releaseSignal, 0, provider.MaxConcurrentFileReads+1)
	start := func(ctx context.Context, writer http.ResponseWriter) {
		group.Add(1)
		go func() {
			defer group.Done()
			request := httptest.NewRequest(http.MethodPost, FileReadPath, strings.NewReader(`{"sandbox_id":"sandbox-1","path":"result.bin"}`)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 64))
			request.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(writer, request)
		}()
	}
	for range provider.MaxConcurrentFileReads {
		release := newReleaseSignal()
		releases = append(releases, release)
		start(context.Background(), &blockingFileWriter{header: make(http.Header), entered: make(chan struct{}, 1), release: release.ch})
	}
	t.Cleanup(func() {
		for _, release := range releases {
			release.close()
		}
		group.Wait()
	})
	for range provider.MaxConcurrentFileReads {
		waitSignal(t, files.entered, "initial provider read")
	}
	waitForAtomic(t, &files.calls, provider.MaxConcurrentFileReads, "initial provider reads")

	fenceEntered := make(chan struct{})
	go func() {
		_ = store.WithJobFence(context.Background(), job.ID, func() error { close(fenceEntered); return nil })
	}()
	waitSignal(t, fenceEntered, "same-Job fence while response writer was blocked")

	health := httptest.NewRecorder()
	healthDone := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodPost, HealthPath, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 64))
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(health, request)
		close(healthDone)
	}()
	waitSignal(t, healthDone, "health control while file writers were blocked")
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", health.Code, health.Body.String())
	}

	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	defer cancelQueued()
	queuedDone := make(chan struct{})
	group.Add(1)
	go func() {
		defer group.Done()
		defer close(queuedDone)
		request := httptest.NewRequest(http.MethodPost, FileReadPath, strings.NewReader(`{"sandbox_id":"sandbox-1","path":"result.bin"}`)).WithContext(queuedCtx)
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 64))
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	assertNoSignal(t, files.entered, "queued provider read")
	cancelQueued()
	waitSignal(t, queuedDone, "queued cancellation")

	nextRelease := newReleaseSignal()
	releases = append(releases, nextRelease)
	nextWriter := &blockingFileWriter{header: make(http.Header), entered: make(chan struct{}, 1), release: nextRelease.ch}
	start(context.Background(), nextWriter)
	assertNoSignal(t, files.entered, "fifth live provider read before release")
	releases[0].close()
	waitSignal(t, files.entered, "next provider read after release")
	waitSignal(t, nextWriter.entered, "next blocked response write")
}

type fileRoundTripFunc func(*http.Request) (*http.Response, error)

func (f fileRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type boundedStore struct {
	job     core.Job
	sandbox core.Sandbox
	fence   sync.Mutex
}

func (s *boundedStore) Job(_ context.Context, id string) (core.Job, error) {
	if id != s.job.ID {
		return core.Job{}, postgres.ErrNotFound
	}
	return s.job, nil
}
func (*boundedStore) CodingJob(context.Context, string) (coding.Job, error) {
	return coding.Job{}, postgres.ErrNotFound
}
func (*boundedStore) Proposal(context.Context, string) (*coding.Proposal, error) { return nil, nil }
func (s *boundedStore) Sandbox(_ context.Context, id string) (core.Sandbox, error) {
	if id != s.sandbox.ID {
		return core.Sandbox{}, postgres.ErrNotFound
	}
	return s.sandbox, nil
}
func (*boundedStore) AgentMessageExecution(context.Context, string) (core.AgentMessageExecution, error) {
	return core.AgentMessageExecution{}, postgres.ErrNotFound
}
func (s *boundedStore) WithJobFence(_ context.Context, _ string, run func() error) error {
	s.fence.Lock()
	defer s.fence.Unlock()
	return run()
}
func (*boundedStore) BeginSandboxActivity(context.Context, string) error  { return nil }
func (*boundedStore) FinishSandboxActivity(context.Context, string) error { return nil }

type boundedRuntimes struct {
	profile string
	files   core.SandboxFileReader
}

func (r boundedRuntimes) ResolveSandbox(_ context.Context, profile core.SandboxProfileRef) (core.SandboxRuntime, error) {
	if profile.Name != r.profile {
		return core.SandboxRuntime{}, errors.New("foreign profile")
	}
	return core.SandboxRuntime{SandboxProfile: profile, Files: r.files}, nil
}

type boundedFiles struct {
	contents []byte
	err      error
	entered  chan struct{}
	calls    atomic.Int32
}

func (f *boundedFiles) ReadSandboxFile(context.Context, core.Job, core.Sandbox, string) ([]byte, error) {
	f.calls.Add(1)
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	return append([]byte(nil), f.contents...), f.err
}

type blockingFileWriter struct {
	header  http.Header
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *blockingFileWriter) Header() http.Header { return w.header }
func (*blockingFileWriter) WriteHeader(int)       {}
func (w *blockingFileWriter) Write(contents []byte) (int, error) {
	w.once.Do(func() { w.entered <- struct{}{} })
	<-w.release
	return len(contents), nil
}

type releaseSignal struct {
	ch   chan struct{}
	once sync.Once
}

func newReleaseSignal() *releaseSignal { return &releaseSignal{ch: make(chan struct{})} }
func (s *releaseSignal) close()        { s.once.Do(func() { close(s.ch) }) }

func boundedReaderAuthority() (core.Job, core.Sandbox) {
	job := core.Job{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	return job, core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
}

func waitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func assertNoSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
		t.Fatalf("unexpected %s", what)
	case <-time.After(50 * time.Millisecond):
	}
}

func waitForAtomic(t *testing.T, value *atomic.Int32, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for value.Load() != int32(want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: got %d want %d", what, value.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

var _ Store = (*boundedStore)(nil)
var _ core.SandboxRuntimeResolver = boundedRuntimes{}
var _ core.SandboxFileReader = (*boundedFiles)(nil)
