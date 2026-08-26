package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlclient"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestRemoteCLIJourneyRunsBeforeHostDeploymentComposition(t *testing.T) {
	auth := &remoteCLIAuth{client: controlauth.Client{
		ID: "client-1", Name: "laptop",
		CredentialExpiresAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}}
	jobs := &remoteCLIJobs{job: controlapi.Job{
		ID: "job-1", Kind: "direct", Goal: "prove remote control", Profile: "default",
		Model: "model-1", Reasoning: "high", InitialMessageID: "message-1", Admission: controlapi.Admission{Open: true},
		Execution: controlapi.State{State: "idle"}, Cleanup: controlapi.State{State: "not_requested"},
		Sandboxes: []controlapi.Sandbox{{ID: "sandbox-1", Name: "main"}},
	}}
	api := controlapi.NewServer(controlapi.Discovery{
		Product: "dorf", Version: "test", Capabilities: []string{"direct_jobs"},
	}, auth, jobs)
	originalTransport := http.DefaultTransport
	var admissionAttempts []string
	var messageAttempts, retryAttempts []string
	http.DefaultTransport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		lostResponse := false
		if request.Method == http.MethodPost && request.URL.Path == "/v1/jobs" {
			admissionAttempts = append(admissionAttempts, request.Header.Get("Idempotency-Key"))
			lostResponse = len(admissionAttempts) == 1
		}
		if request.Method == http.MethodPost && request.URL.Path == "/v1/jobs/job-1/messages" {
			messageAttempts = append(messageAttempts, request.Header.Get("Idempotency-Key"))
			lostResponse = len(messageAttempts) == 1
		}
		if request.Method == http.MethodPost && request.URL.Path == "/v1/jobs/job-1/retries" {
			retryAttempts = append(retryAttempts, request.Header.Get("Idempotency-Key"))
			lostResponse = len(retryAttempts) == 1
		}
		response := httptest.NewRecorder()
		api.Handler.ServeHTTP(response, request)
		if lostResponse {
			return nil, errors.New("simulated lost response after commit")
		}
		return response.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	deploymentURL := "https://dorf.example.test"

	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	// Reaching host configuration would fail, so success proves client-only dispatch happens first.
	t.Setenv("DORF_GITHUB_API_URL", "deliberately-invalid-host-configuration")
	enrollmentFile := filepath.Join(root, "enrollment")
	goalFile := filepath.Join(root, "goal")
	messageFile := filepath.Join(root, "message")
	download := filepath.Join(root, "REPORT.md")
	if err := os.WriteFile(enrollmentFile, []byte("one-time-code\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goalFile, []byte(jobs.job.Goal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(messageFile, []byte("continue with exact evidence"), 0o600); err != nil {
		t.Fatal(err)
	}

	commands := [][]string{
		{"connect", "--name", "laptop", "--enrollment-file", enrollmentFile, deploymentURL},
		{"auth", "status"},
		{"run", "--goal-file", goalFile, "--model", jobs.job.Model},
		{"job", "inspect", jobs.job.ID},
		{"job", "message", "--input-file", messageFile, jobs.job.ID},
		{"job", "message", "inspect", jobs.job.ID, "message-2"},
		{"job", "retry", jobs.job.ID},
		{"job", "evidence", jobs.job.ID},
		{"sandbox", "file", "get", "sandbox-1", "REPORT.md", "--output", download},
		{"job", "cleanup", jobs.job.ID},
	}
	var output strings.Builder
	for _, command := range commands {
		var stdout, stderr strings.Builder
		if err := run(context.Background(), command, &stdout, &stderr); err != nil {
			t.Fatalf("dorf %s: %v\nstderr: %s", strings.Join(command, " "), err, stderr.String())
		}
		output.WriteString(stdout.String())
		output.WriteString(stderr.String())
	}

	auth.mu.Lock()
	code, name, credential := auth.code, auth.name, auth.credential
	auth.mu.Unlock()
	jobs.mu.Lock()
	requestKey, admission := jobs.key, jobs.input
	jobs.mu.Unlock()
	if code != "one-time-code" || name != "laptop" || len(credential) != 43 {
		t.Fatalf("enrollment code=%q name=%q credential length=%d", code, name, len(credential))
	}
	wantAdmission := controlapi.AdmitJobRequest{Goal: jobs.job.Goal, Model: jobs.job.Model, Reasoning: jobs.job.Reasoning}
	if admission != wantAdmission {
		t.Fatalf("Job admission=%#v, want %#v", admission, wantAdmission)
	}
	if requestKey == "" || len(admissionAttempts) != 2 || admissionAttempts[0] != requestKey || admissionAttempts[1] != requestKey {
		t.Fatalf("automatic admission attempts=%q, want the same generated identity twice", admissionAttempts)
	}
	if len(messageAttempts) != 2 || messageAttempts[0] == "" || messageAttempts[0] != messageAttempts[1] ||
		len(retryAttempts) != 2 || retryAttempts[0] == "" || retryAttempts[0] != retryAttempts[1] {
		t.Fatalf("mutation replay keys message=%q retry=%q", messageAttempts, retryAttempts)
	}
	if contents, err := os.ReadFile(download); err != nil || string(contents) != "exact report\x00\n" {
		t.Fatalf("downloaded exact Sandbox file=%q err=%v", contents, err)
	}
	if !strings.Contains(output.String(), "Job job-1 accepted") || !strings.Contains(output.String(), "Message message-2 accepted") ||
		!strings.Contains(output.String(), "Retry scheduled") || !strings.Contains(output.String(), "Cleanup requested for Job job-1") {
		t.Fatalf("remote CLI journey output omitted its Job result:\n%s", output.String())
	}
	for _, secret := range []string{code, credential, requestKey, messageAttempts[0], retryAttempts[0]} {
		if secret != "" && strings.Contains(output.String(), secret) {
			t.Fatalf("remote CLI output leaked %q:\n%s", secret, output.String())
		}
	}

	original, _, found, err := loadClientConfig()
	if err != nil || !found {
		t.Fatalf("load connected Client: found=%t err=%v", found, err)
	}
	if err := run(context.Background(), []string{"connect", "https://other.example.test"}, &strings.Builder{}, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "--enrollment-file") {
		t.Fatalf("non-interactive connect error=%v", err)
	}
	auth.mu.Lock()
	auth.revoked, auth.rejectRedeem = true, true
	auth.mu.Unlock()
	if err := run(context.Background(), []string{"connect", "--name", "laptop", "--enrollment-file", enrollmentFile, "https://other.example.test"}, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("failed Deployment switch unexpectedly succeeded")
	}
	restored, _, found, err := loadClientConfig()
	if err != nil || !found || restored != original {
		t.Fatalf("failed switch did not restore prior connection: found=%t config=%#v err=%v", found, restored, err)
	}
	auth.mu.Lock()
	auth.rejectRedeem = false
	auth.mu.Unlock()
	if err := os.WriteFile(enrollmentFile, []byte("second-one-time-code\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"connect", "--name", "laptop", "--enrollment-file", enrollmentFile, deploymentURL}, &strings.Builder{}, &strings.Builder{}); err != nil {
		t.Fatalf("rotate revoked Client credential: %v", err)
	}
	rotated, _, found, err := loadClientConfig()
	if err != nil || !found || rotated.Credential == original.Credential {
		t.Fatalf("revoked Client credential was not rotated: found=%t err=%v", found, err)
	}
}

func TestPublicJobStatesKeepCleanupTruthSeparateFromExecution(t *testing.T) {
	const privateMarker = "reconciling private provider resource"
	tests := []struct {
		name          string
		cleanup       core.CleanupState
		task          absurd.TaskResultState
		execution     direct.ExecutionState
		wantExecution string
		wantCleanup   string
		wantCode      string
	}{
		{name: "healthy cleanup preserves idle", cleanup: core.CleanupScheduled, task: absurd.TaskRunning, execution: direct.ExecutionIdle, wantExecution: "idle", wantCleanup: "running"},
		{name: "healthy cleanup stops active work", cleanup: core.CleanupScheduled, task: absurd.TaskRunning, execution: direct.ExecutionAwaitingAgent, wantExecution: "stopped", wantCleanup: "running"},
		{name: "healthy cleanup preserves attention", cleanup: core.CleanupScheduled, task: absurd.TaskRunning, execution: direct.ExecutionAttention, wantExecution: "stopped", wantCleanup: "running", wantCode: "agent_attention"},
		{name: "failed cleanup", cleanup: core.CleanupScheduled, task: absurd.TaskFailed, execution: direct.ExecutionIdle, wantExecution: "idle", wantCleanup: "failed", wantCode: "cleanup_failed"},
		{name: "requested cleanup preserves failure", cleanup: core.CleanupRequested, task: absurd.TaskFailed, execution: direct.ExecutionIdle, wantExecution: "failed", wantCleanup: "requested", wantCode: "execution_failed"},
		{name: "requested cleanup accepts cancellation window", cleanup: core.CleanupRequested, task: absurd.TaskCancelled, execution: direct.ExecutionAwaitingAgent, wantExecution: "stopped", wantCleanup: "requested"},
		{name: "missing task attachment", cleanup: core.CleanupPending, task: "", execution: direct.ExecutionProvisioningSandbox, wantExecution: "failed", wantCleanup: "not_requested", wantCode: "execution_failed"},
		{name: "cancelled execution", cleanup: core.CleanupPending, task: absurd.TaskCancelled, execution: direct.ExecutionIdle, wantExecution: "failed", wantCleanup: "not_requested", wantCode: "execution_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			execution, cleanup, attention, err := publicJobStates(core.Job{
				ID: "job-1", CleanupState: test.cleanup, CleanupAttention: privateMarker,
			}, direct.Projection{State: test.execution}, test.task)
			if err != nil {
				t.Fatal(err)
			}
			if execution != test.wantExecution || cleanup != test.wantCleanup {
				t.Fatalf("execution=%q cleanup=%q, want %s/%s", execution, cleanup, test.wantExecution, test.wantCleanup)
			}
			if test.wantCode == "" && attention != nil {
				t.Fatalf("healthy cleanup exposed attention %#v", attention)
			}
			if test.wantCode != "" && (attention == nil || attention.Code != test.wantCode || strings.Contains(attention.Detail, privateMarker)) {
				t.Fatalf("cleanup attention=%#v, want fixed %q", attention, test.wantCode)
			}
		})
	}
}

func TestPublicMessageDeliveryStatesDoNotExposeAgentRunLifecycle(t *testing.T) {
	tests := map[core.AgentRunState]string{
		core.AgentRunPending: "accepted", core.AgentRunSubmitting: "accepted",
		core.AgentRunActive: "running", core.AgentRunUncertain: "running",
		core.AgentRunCompleted: "completed", core.AgentRunFailed: "failed", core.AgentRunInterrupted: "failed",
	}
	for internal, want := range tests {
		got, err := publicMessageDeliveryState(internal)
		if err != nil || got != want {
			t.Fatalf("internal state %q projected as %q, want %q: %v", internal, got, want, err)
		}
	}
	if _, err := publicMessageDeliveryState("future-state"); err == nil {
		t.Fatal("unknown internal delivery state crossed the public API")
	}
}

func TestAdmissionRetryUsesHTTPFailureClass(t *testing.T) {
	background := context.Background()
	ingress5xx := &controlclient.ProblemError{Problem: controlapi.Problem{Status: http.StatusBadGateway}}
	definitive4xx := &controlclient.ProblemError{Problem: controlapi.Problem{Status: http.StatusBadRequest, Retryable: true}}
	if !retryableMutationError(background, ingress5xx) || retryableMutationError(background, definitive4xx) || retryableMutationError(background, context.Canceled) {
		t.Fatal("admission retry must accept every 5xx and reject 4xx or cancellation")
	}
}

func TestRemoteJobWatchWritesOneSnapshotPerJSONLLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &cancelAtEOF{reader: strings.NewReader("event: snapshot\nid: snapshot-1\ndata: {\"id\":\"job-watch\"}\n\n"), cancel: cancel}
	client, err := controlclient.New("https://dorf.example.test", "credential", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/jobs/job-watch/watch" || request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("watch request path=%q accept=%q", request.URL.Path, request.Header.Get("Accept"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if err := remoteJobWatch(ctx, client, []string{"--output", "jsonl", "job-watch"}, &stdout, &stderr); err != nil {
		t.Fatalf("watch: %v stderr=%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"id":"job-watch"`) {
		t.Fatalf("JSONL output=%q", stdout.String())
	}
}

func TestRemoteMessageHumanOutputQuotesHarnessControlBytes(t *testing.T) {
	var output strings.Builder
	renderRemoteMessage(&output, controlapi.Message{
		Delivery: controlapi.State{State: "completed"},
		Result:   &controlapi.MessageResult{Outcome: "completed", Output: "safe\x1b]52;bad\rrewritten"},
	})
	if strings.ContainsRune(output.String(), '\x1b') || !strings.Contains(output.String(), `\x1b`) || !strings.Contains(output.String(), `\r`) {
		t.Fatalf("human Message output did not quote control bytes: %q", output.String())
	}
}

func TestControlAdmissionRejectsValuesPostgresCannotRetain(t *testing.T) {
	for _, input := range []controlapi.AdmitJobRequest{
		{Goal: "contains\x00nul", Model: "model", Reasoning: "high"},
		{Goal: "valid goal", Model: strings.Repeat("m", maxControlModelBytes+1), Reasoning: "high"},
	} {
		if _, _, err := (controlAPIJobs{}).AdmitDirect(context.Background(), "request", input); !errors.Is(err, controlapi.ErrInvalidInput) {
			t.Fatalf("input=%#v error=%v, want invalid input", input, err)
		}
	}
}

type remoteCLIAuth struct {
	mu                     sync.Mutex
	client                 controlauth.Client
	code, name, credential string
	revoked, rejectRedeem  bool
}

type cancelAtEOF struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r *cancelAtEOF) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.cancel()
	}
	return n, err
}

func (r *cancelAtEOF) Close() error { return nil }

func (a *remoteCLIAuth) Redeem(_ context.Context, code, name, credential string) (controlauth.Client, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rejectRedeem {
		return controlauth.Client{}, false, controlauth.ErrEnrollmentUnavailable
	}
	if a.code == code {
		if a.name == name && a.credential == credential {
			return a.client, false, nil
		}
		return controlauth.Client{}, false, controlauth.ErrEnrollmentUnavailable
	}
	a.code, a.name, a.credential = code, name, credential
	a.revoked = false
	return a.client, true, nil
}

func (a *remoteCLIAuth) Authenticate(_ context.Context, credential string) (controlauth.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.revoked || credential == "" || credential != a.credential {
		return controlauth.Client{}, controlauth.ErrUnauthenticated
	}
	return a.client, nil
}

type remoteCLIJobs struct {
	mu    sync.Mutex
	job   controlapi.Job
	key   string
	input controlapi.AdmitJobRequest
}

func (j *remoteCLIJobs) AdmitDirect(_ context.Context, key string, input controlapi.AdmitJobRequest) (controlapi.Job, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.key, j.input = key, input
	return j.job, true, nil
}

func (j *remoteCLIJobs) Get(_ context.Context, id string) (controlapi.Job, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if id != j.job.ID {
		return controlapi.Job{}, controlapi.ErrJobNotFound
	}
	return j.job, nil
}

func (j *remoteCLIJobs) RequestCleanup(_ context.Context, id string) (controlapi.Job, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if id != j.job.ID {
		return controlapi.Job{}, controlapi.ErrJobNotFound
	}
	j.job.Cleanup.State = "requested"
	return j.job, nil
}

func (j *remoteCLIJobs) SendMessage(_ context.Context, jobID, _ string, input controlapi.SendMessageRequest) (controlapi.Message, bool, error) {
	if jobID != j.job.ID {
		return controlapi.Message{}, false, controlapi.ErrJobNotFound
	}
	return controlapi.Message{
		ID: "message-2", JobID: jobID, Sequence: 2, Intent: input.Intent,
		Delivery: controlapi.State{State: "pending"}, AdmittedAt: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
	}, true, nil
}

func (j *remoteCLIJobs) GetMessage(_ context.Context, jobID, messageID string) (controlapi.Message, error) {
	if jobID != j.job.ID || messageID != "message-2" {
		return controlapi.Message{}, controlapi.ErrMessageNotFound
	}
	return controlapi.Message{ID: messageID, JobID: jobID, Sequence: 2, Intent: "follow", Delivery: controlapi.State{State: "completed"}}, nil
}

func (j *remoteCLIJobs) Retry(_ context.Context, jobID, _ string) (controlapi.Retry, bool, error) {
	if jobID != j.job.ID {
		return controlapi.Retry{}, false, controlapi.ErrJobNotFound
	}
	return controlapi.Retry{JobID: jobID, State: "scheduled"}, true, nil
}

func (j *remoteCLIJobs) ReadSandboxFile(_ context.Context, sandboxID, path string) ([]byte, error) {
	if sandboxID != "sandbox-1" || path != "REPORT.md" {
		return nil, controlapi.ErrFileNotFound
	}
	return []byte("exact report\x00\n"), nil
}

func (j *remoteCLIJobs) Evidence(_ context.Context, jobID string) ([]controlapi.Evidence, error) {
	if jobID != j.job.ID {
		return nil, controlapi.ErrJobNotFound
	}
	return []controlapi.Evidence{}, nil
}
