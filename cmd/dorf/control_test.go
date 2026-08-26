package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlclient"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestServeAllowsWildcardOnlyWithExplicitContainerOptIn(t *testing.T) {
	probe, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	address := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	var stdout, stderr strings.Builder
	err = serveCommand(ctx, postgres.Store{}, nil, config.Config{},
		[]string{"--listen", address, "--allow-container-listen"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("explicit container listen %s: %v\nstderr: %s", address, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "http://"+address) {
		t.Fatalf("serve output %q does not report container listener %s", stdout.String(), address)
	}
}

func TestServeRejectsUnsafeListeners(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "wildcard without container opt-in",
			args:    []string{"--listen", "0.0.0.0:8745"},
			wantErr: "--allow-container-listen",
		},
		{
			name:    "specific host interface despite container opt-in",
			args:    []string{"--listen", "192.0.2.10:8745", "--allow-container-listen"},
			wantErr: "exact loopback IP",
		},
		{
			name:    "privileged port despite container opt-in",
			args:    []string{"--listen", "0.0.0.0:1023", "--allow-container-listen"},
			wantErr: "port 1024-65535",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr strings.Builder
			err := serveCommand(context.Background(), postgres.Store{}, nil, config.Config{},
				test.args, io.Discard, &stderr)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("serve args %q error=%v, want %q", test.args, err, test.wantErr)
			}
		})
	}
}

func TestConfiguredControlReaderUsesOnlyCompleteInternalCapability(t *testing.T) {
	t.Run("manual local serve", func(t *testing.T) {
		t.Setenv("DORF_CONTROL_READER_ORIGIN", "")
		t.Setenv("DORF_CONTROL_READER_TOKEN", "")
		reader, err := configuredControlReader(config.Config{}, postgres.Store{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reader.(controlreader.Service); !ok {
			t.Fatalf("manual reader type=%T", reader)
		}
	})

	t.Run("partial capability rejected", func(t *testing.T) {
		t.Setenv("DORF_CONTROL_READER_ORIGIN", "http://control-reader:8756")
		t.Setenv("DORF_CONTROL_READER_TOKEN", "")
		if _, err := configuredControlReader(config.Config{}, postgres.Store{}, nil); err == nil || !strings.Contains(err.Error(), "requires both") {
			t.Fatalf("configuredControlReader() error=%v", err)
		}
	})

	t.Run("Compose capability", func(t *testing.T) {
		t.Setenv("DORF_CONTROL_READER_ORIGIN", "http://control-reader:8756")
		t.Setenv("DORF_CONTROL_READER_TOKEN", strings.Repeat("a", 64))
		reader, err := configuredControlReader(config.Config{}, postgres.Store{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reader.(controlreader.Client); !ok {
			t.Fatalf("Compose reader type=%T", reader)
		}
	})
}

func TestRemoteCLIJourneyRunsBeforeHostDeploymentComposition(t *testing.T) {
	auth := &remoteCLIAuth{client: controlauth.Client{
		ID: "client-1", Name: "laptop",
		CredentialExpiresAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}}
	jobs := &remoteCLIJobs{job: controlapi.DirectJob{Job: controlapi.Job{
		ID: "job-1", Kind: "direct", Goal: "prove remote control", Profile: "default",
		Model: "model-1", Reasoning: "high", InitialMessageID: "message-1", Admission: controlapi.Admission{Open: true},
		Execution: controlapi.State{State: "idle"}, Cleanup: controlapi.State{State: "not_requested"},
		Sandboxes: []controlapi.Sandbox{{ID: "sandbox-1", Name: "main"}},
	}}}
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
		{"job", "list"},
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
	var authJSON strings.Builder
	if err := run(context.Background(), []string{"auth", "status", "--output", "json"}, &authJSON, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	var authStatus authStatusReceipt
	if err := json.Unmarshal([]byte(authJSON.String()), &authStatus); err != nil ||
		authStatus.Deployment != deploymentURL || authStatus.Client.ID != auth.client.ID ||
		authStatus.Principal.ID != controlauth.DeploymentOperatorPrincipalID || authStatus.CredentialSource != "client_config" {
		t.Fatalf("machine auth status=%#v err=%v", authStatus, err)
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

func TestRemoteWorkflowCLIUsesExplicitRoutesAndFencesLocalRepositories(t *testing.T) {
	goal, brief := "  exact coding goal\n", "  exact investigation brief\n"
	goalFile, briefFile := filepath.Join(t.TempDir(), "goal"), filepath.Join(t.TempDir(), "brief")
	if err := os.WriteFile(goalFile, []byte(goal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(briefFile, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	var codingRequest controlapi.AdmitCodingJobRequest
	var investigationRequest controlapi.AdmitInvestigationJobRequest
	var paths, keys []string
	client, err := controlclient.New("https://dorf.example.test", "credential", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		keys = append(keys, request.Header.Get("Idempotency-Key"))
		response := httptest.NewRecorder()
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		switch request.URL.Path {
		case "/v1/workflows/coding/jobs":
			if err := json.NewDecoder(request.Body).Decode(&codingRequest); err != nil {
				return nil, err
			}
			_ = json.NewEncoder(response).Encode(controlapi.CodingJob{
				Job:              controlapi.Job{ID: "coding-job", Kind: controlapi.JobKindCoding, Goal: codingRequest.Goal},
				WorkflowRevision: "coding/v1", Repository: codingRequest.Repository, StartingRevision: codingRequest.Revision,
				Revision: codingRequest.Revision, Branch: "dorf/coding-job", BaseBranch: codingRequest.BaseBranch,
			})
		case "/v1/workflows/codebase-investigation/jobs":
			if err := json.NewDecoder(request.Body).Decode(&investigationRequest); err != nil {
				return nil, err
			}
			_ = json.NewEncoder(response).Encode(controlapi.InvestigationJob{
				Job:              controlapi.Job{ID: "investigation-job", Kind: controlapi.JobKindInvestigation, Goal: investigationRequest.Brief},
				WorkflowRevision: "codebase-investigation/v1",
				Source:           controlapi.InvestigationSource{Kind: "remote", Repository: investigationRequest.Repository, Revision: investigationRequest.Revision},
				Report:           controlapi.InvestigationReport{SandboxID: "sandbox-investigation", Path: "REPORT.md"},
			})
		default:
			return nil, errors.New("unexpected workflow route " + request.URL.Path)
		}
		return response.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	var codingOutput, investigationOutput strings.Builder
	if err := remoteWorkflowCommand(context.Background(), client, "https://dorf.example.test",
		[]string{"run", "coding", "--key", "coding-key", "--goal-file", goalFile, "--repo", "https://github.com/aphronio/dorf.git", "--revision", revision, "--base", "main", "--model", "model"},
		&codingOutput, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if err := remoteWorkflowCommand(context.Background(), client, "https://dorf.example.test",
		[]string{"run", "codebase-investigation", "--key", "investigation-key", "--brief-file", briefFile, "--repo", "https://github.com/aphronio/dorf.git", "--revision", revision, "--model", "model", "--output", "json"},
		&investigationOutput, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if codingRequest.Goal != goal || investigationRequest.Brief != brief || !slices.Equal(paths, []string{"/v1/workflows/coding/jobs", "/v1/workflows/codebase-investigation/jobs"}) ||
		!slices.Equal(keys, []string{"coding-key", "investigation-key"}) {
		t.Fatalf("coding=%#v investigation=%#v paths=%q keys=%q", codingRequest, investigationRequest, paths, keys)
	}
	if !strings.Contains(codingOutput.String(), "repository: https://github.com/aphronio/dorf.git") ||
		!strings.Contains(investigationOutput.String(), `"kind": "codebase-investigation"`) {
		t.Fatalf("coding output=%q investigation output=%q", codingOutput.String(), investigationOutput.String())
	}
	if err := remoteWorkflowCommand(context.Background(), client, "https://dorf.example.test",
		[]string{"run", "codebase-investigation", "--local-repo", "/deployment-only"}, &strings.Builder{}, &strings.Builder{}); err == nil ||
		!strings.Contains(err.Error(), "deployment host") || len(paths) != 2 {
		t.Fatalf("remote local-repo fence err=%v paths=%q", err, paths)
	}
}

func TestPublicJobStatesKeepCleanupTruthSeparateFromExecution(t *testing.T) {
	const privateMarker = "reconciling private provider resource"
	tests := []struct {
		name          string
		cleanup       core.CleanupState
		task          absurd.TaskResultState
		execution     string
		wantExecution string
		wantCleanup   string
		wantCode      string
	}{
		{name: "healthy cleanup preserves idle", cleanup: core.CleanupScheduled, task: absurd.TaskRunning, execution: "idle", wantExecution: "idle", wantCleanup: "running"},
		{name: "healthy cleanup stops active work", cleanup: core.CleanupScheduled, task: absurd.TaskRunning, execution: "running", wantExecution: "stopped", wantCleanup: "running"},
		{name: "healthy cleanup preserves attention", cleanup: core.CleanupScheduled, task: absurd.TaskRunning, execution: "stopped", wantExecution: "stopped", wantCleanup: "running", wantCode: "agent_attention"},
		{name: "failed cleanup", cleanup: core.CleanupScheduled, task: absurd.TaskFailed, execution: "idle", wantExecution: "idle", wantCleanup: "failed", wantCode: "cleanup_failed"},
		{name: "requested cleanup preserves failure", cleanup: core.CleanupRequested, task: absurd.TaskFailed, execution: "idle", wantExecution: "failed", wantCleanup: "requested", wantCode: "execution_failed"},
		{name: "requested cleanup accepts cancellation window", cleanup: core.CleanupRequested, task: absurd.TaskCancelled, execution: "running", wantExecution: "stopped", wantCleanup: "requested"},
		{name: "missing task attachment", cleanup: core.CleanupPending, task: "", execution: "provisioning_sandbox", wantExecution: "failed", wantCleanup: "not_requested", wantCode: "execution_failed"},
		{name: "cancelled execution", cleanup: core.CleanupPending, task: absurd.TaskCancelled, execution: "idle", wantExecution: "failed", wantCleanup: "not_requested", wantCode: "execution_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var inputAttention *controlapi.Attention
			if test.wantCode == "agent_attention" {
				inputAttention = &controlapi.Attention{Code: "agent_attention", Detail: "safe detail"}
			}
			view, err := publicCommonJob(core.Job{
				ID: "job-1", CleanupState: test.cleanup, CleanupAttention: privateMarker,
			}, controlapi.JobKindDirect, test.execution, inputAttention, test.task,
				[]core.Sandbox{{ID: "sandbox-1", JobID: "job-1", Name: "default"}}, "message-1")
			if err != nil {
				t.Fatal(err)
			}
			if view.Execution.State != test.wantExecution || view.Cleanup.State != test.wantCleanup {
				t.Fatalf("execution=%q cleanup=%q, want %s/%s", view.Execution.State, view.Cleanup.State, test.wantExecution, test.wantCleanup)
			}
			if test.wantCode == "" && view.Attention != nil {
				t.Fatalf("healthy cleanup exposed attention %#v", view.Attention)
			}
			if test.wantCode != "" && (view.Attention == nil || view.Attention.Code != test.wantCode || strings.Contains(view.Attention.Detail, privateMarker)) {
				t.Fatalf("cleanup attention=%#v, want fixed %q", view.Attention, test.wantCode)
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
	var guidance strings.Builder
	calls := 0
	_, err := runKeyedMutation(background, "generated-key", true, &guidance, "Admission may have succeeded.", func() (struct{}, error) {
		calls++
		return struct{}{}, context.DeadlineExceeded
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || !strings.Contains(guidance.String(), "--key generated-key") {
		t.Fatalf("ambiguous deadline err=%v calls=%d guidance=%q", err, calls, guidance.String())
	}
	guidance.Reset()
	calls = 0
	_, err = runKeyedMutation(background, "retry-key", true, &guidance, "Admission may have succeeded.", func() (struct{}, error) {
		calls++
		if calls == 1 {
			return struct{}{}, ingress5xx
		}
		return struct{}{}, definitive4xx
	})
	if err != definitive4xx || calls != 2 || !strings.Contains(guidance.String(), "--key retry-key") {
		t.Fatalf("ambiguous first attempt err=%v calls=%d guidance=%q", err, calls, guidance.String())
	}
}

func TestRemoteJobWatchWritesOneSnapshotPerJSONLLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &cancelAtEOF{reader: strings.NewReader("event: snapshot\nid: snapshot-1\ndata: {\"id\":\"job-watch\",\"kind\":\"direct\"}\n\n"), cancel: cancel}
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
	job   controlapi.DirectJob
	key   string
	input controlapi.AdmitJobRequest
}

func (j *remoteCLIJobs) List(_ context.Context, limit int, cursor string) (controlapi.JobList, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return controlapi.JobList{Jobs: []controlapi.JobSummary{{
		ID: j.job.ID, Kind: j.job.Kind, AdmittedAt: time.Date(2026, 8, 26, 11, 0, 0, 0, time.UTC),
	}}}, nil
}

func (j *remoteCLIJobs) AdmitDirect(_ context.Context, key string, input controlapi.AdmitJobRequest) (controlapi.DirectJob, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.key, j.input = key, input
	return j.job, true, nil
}

func (j *remoteCLIJobs) AdmitCoding(context.Context, string, controlapi.AdmitCodingJobRequest) (controlapi.CodingJob, bool, error) {
	return controlapi.CodingJob{}, false, controlapi.ErrInvalidInput
}

func (j *remoteCLIJobs) AdmitInvestigation(context.Context, string, controlapi.AdmitInvestigationJobRequest) (controlapi.InvestigationJob, bool, error) {
	return controlapi.InvestigationJob{}, false, controlapi.ErrInvalidInput
}

func (j *remoteCLIJobs) Get(_ context.Context, id string) (controlapi.JobView, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if id != j.job.ID {
		return nil, controlapi.ErrJobNotFound
	}
	return j.job, nil
}

func (j *remoteCLIJobs) RequestCleanup(_ context.Context, id string) (controlapi.JobView, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if id != j.job.ID {
		return nil, controlapi.ErrJobNotFound
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
