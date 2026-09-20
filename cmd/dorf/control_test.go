package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"

	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/clientconfig"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlclient"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/hostclientconfig"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
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

func TestHostControlGuidanceDoesNotMaskLocalErrors(t *testing.T) {
	local := errors.New("invalid local arguments")
	if got := sessionControlError(hostclientconfig.HostOrigin, local); !errors.Is(got, local) || got.Error() != local.Error() {
		t.Fatalf("host local error=%v", got)
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

func TestSessionControlTargetPrefersExplicitRemoteThenFallsBackToHost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	paths, err := config.CurrentOperatorPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := hostclientconfig.Save(hostclientconfig.Path(paths.StateDir), hostclientconfig.Config{Credential: "host-secret"}); err != nil {
		t.Fatal(err)
	}
	configured, _, client, err := loadConnectedClient()
	if err != nil || configured.DeploymentURL != hostclientconfig.HostOrigin || !strings.Contains(client.String(), hostclientconfig.HostOrigin) {
		t.Fatalf("host config=%#v client=%v err=%v", configured, client, err)
	}
	remote := clientconfig.Config{DeploymentURL: "https://remote.example.test", Credential: "remote-secret"}
	if err := clientconfig.Save(clientconfig.Path(root), remote); err != nil {
		t.Fatal(err)
	}
	configured, _, client, err = loadConnectedClient()
	if err != nil || configured.DeploymentURL != remote.DeploymentURL || !strings.Contains(client.String(), remote.DeploymentURL) {
		t.Fatalf("remote config=%#v client=%v err=%v", configured, client, err)
	}
}

// Reaching host configuration would fail, so success proves client-only dispatch happens first.

func TestPublicSessionStatesKeepCleanupTruthSeparateFromExecution(t *testing.T) {
	const privateMarker = "reconciling private provider resource"
	tests := []struct {
		name          string
		cleanup       core.CleanupState
		task          absurd.TaskResultState
		execution     string
		wantExecution string
		wantCleanup   string
		wantCode      string
		failure       json.RawMessage
	}{
		{name: "healthy cleanup preserves idle", cleanup: core.CleanupScheduled, task: absurd.TaskRunning, execution: "idle", wantExecution: "idle", wantCleanup: "running"},
		{name: "healthy cleanup preserves attention", cleanup: core.CleanupScheduled, task: absurd.TaskRunning, execution: "stopped", wantExecution: "stopped", wantCleanup: "running", wantCode: "agent_attention"},
		{name: "failed cleanup", failure: json.RawMessage(`{"message":"create Incus instance private-sandbox: Reached maximum number of instances in project private-project"}`), cleanup: core.CleanupScheduled, task: absurd.TaskFailed, execution: "idle", wantExecution: "idle", wantCleanup: "failed", wantCode: "cleanup_failed"},
		{name: "requested cleanup preserves failure", cleanup: core.CleanupRequested, task: absurd.TaskFailed, execution: "idle", wantExecution: "failed", wantCleanup: "requested", wantCode: "execution_failed"},
		{name: "requested cleanup accepts cancellation window", cleanup: core.CleanupRequested, task: absurd.TaskCancelled, execution: "provisioning_sandbox", wantExecution: "stopped", wantCleanup: "requested"},
		{name: "missing task attachment", cleanup: core.CleanupPending, task: "", execution: "provisioning_sandbox", wantExecution: "failed", wantCleanup: "not_requested", wantCode: "execution_failed"},
		{name: "cancelled execution", cleanup: core.CleanupPending, task: absurd.TaskCancelled, execution: "idle", wantExecution: "failed", wantCleanup: "not_requested", wantCode: "execution_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var inputAttention *controlapi.Attention
			if test.wantCode == "agent_attention" {
				inputAttention = &controlapi.Attention{Code: "agent_attention", Detail: "safe detail"}
			}
			view, err := publicSession(core.Session{
				ID: "session-1", CleanupState: test.cleanup, CleanupAttention: privateMarker,
			}, test.execution, inputAttention, taskResultView{State: test.task, failure: test.failure},
				[]core.Sandbox{{ID: "sandbox-1", SessionID: "session-1", Name: "default"}})
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

func TestRemoteSessionWatchWritesOneSnapshotPerJSONLLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &cancelAtEOF{reader: strings.NewReader("event: snapshot\nid: snapshot-1\ndata: {\"id\":\"session-watch\"}\n\n"), cancel: cancel}
	client, err := controlclient.New("https://dorf.example.test", "credential", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/sessions/session-watch/watch" || request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("watch request path=%q accept=%q", request.URL.Path, request.Header.Get("Accept"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if err := remoteSessionWatch(ctx, client, []string{"--output", "jsonl", "session-watch"}, &stdout, &stderr); err != nil {
		t.Fatalf("watch: %v stderr=%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"id":"session-watch"`) {
		t.Fatalf("JSONL output=%q", stdout.String())
	}
}

func TestControlAdmissionRejectsValuesPostgresCannotRetain(t *testing.T) {
	for _, input := range []controlapi.CreateSessionRequest{
		{AgentsMD: "contains\x00nul", Model: "model", Reasoning: "high"},
		{AgentsMD: "valid instructions", Model: strings.Repeat("m", maxControlModelBytes+1), Reasoning: "high"},
	} {
		if _, _, err := (controlAPISessions{}).Create(context.Background(), "", "request", input); !errors.Is(err, controlapi.ErrInvalidInput) {
			t.Fatalf("input=%#v error=%v, want invalid input", input, err)
		}
	}
}

func TestControlAdmissionAllowsServerResolvedModel(t *testing.T) {
	admission, err := newControlSessionAdmission("request", "goal", "", "", "", "")
	if err != nil || admission.Model != "" {
		t.Fatalf("admission=%#v error=%v", admission, err)
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

type remoteCLISessions struct {
	mu      sync.Mutex
	session controlapi.Session
	key     string
	input   controlapi.CreateSessionRequest
}

func (j *remoteCLISessions) List(_ context.Context, limit int, cursor string) (controlapi.SessionList, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return controlapi.SessionList{Sessions: []controlapi.SessionSummary{{
		CreatedByClient: j.session.CreatedByClient, ClientReference: j.session.ClientReference, ID: j.session.ID, AdmittedAt: time.Date(2026, 8, 26, 11, 0, 0, 0, time.UTC),
	}}}, nil
}

func (j *remoteCLISessions) Create(_ context.Context, _ string, key string, input controlapi.CreateSessionRequest) (controlapi.Session, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.key, j.input = key, input
	return j.session, true, nil
}

func (j *remoteCLISessions) Get(_ context.Context, id string) (controlapi.Session, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if id != j.session.ID {
		return controlapi.Session{}, controlapi.ErrSessionNotFound
	}
	return j.session, nil
}

func (j *remoteCLISessions) RequestCleanup(_ context.Context, id string) (controlapi.Session, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if id != j.session.ID {
		return controlapi.Session{}, controlapi.ErrSessionNotFound
	}
	j.session.Cleanup.State = "requested"
	return j.session, nil
}

func (j *remoteCLISessions) Retry(_ context.Context, sessionID, _ string) (controlapi.Retry, bool, error) {
	if sessionID != j.session.ID {
		return controlapi.Retry{}, false, controlapi.ErrSessionNotFound
	}
	return controlapi.Retry{SessionID: sessionID, State: "scheduled"}, true, nil
}

func (j *remoteCLISessions) ReadSandboxFile(_ context.Context, sandboxID, path string) ([]byte, error) {
	if sandboxID != "sandbox-1" || path != "REPORT.md" {
		return nil, controlapi.ErrFileNotFound
	}
	return []byte("exact report\x00\n"), nil
}

func (j *remoteCLISessions) WriteSandboxFile(context.Context, string, string, []byte, bool) error {
	return nil
}

func (f *remoteCLISessions) ExecSandbox(context.Context, string, provider.Command) (provider.CommandResult, error) {
	return provider.CommandResult{}, nil
}

func TestRemoteSessionHumanExecutionLabels(t *testing.T) {
	for _, test := range []struct {
		state     string
		attention *controlapi.Attention
		want      string
	}{
		{"provisioning_sandbox", nil, "Starting"},
		{"connecting_model_access", nil, "Connecting"},
		{"idle", nil, "Idle"},
		{"stopped", nil, "Stopped"},
		{"failed", nil, "Needs attention"},
		{"idle", &controlapi.Attention{Code: "agent_attention"}, "Needs attention"},
	} {
		t.Run(test.state+"/"+test.want, func(t *testing.T) {
			var output strings.Builder
			session := controlapi.Session{Execution: controlapi.State{State: test.state}, Attention: test.attention}
			renderRemoteSession(&output, session)
			if !strings.Contains(output.String(), "  execution: "+test.want+"\n") {
				t.Fatalf("human Session output = %q", output.String())
			}
		})
	}
}

func (f *remoteCLISessions) ReadSandboxStatus(context.Context, string) (provider.Status, error) {
	return provider.Status{Provider: "incus", State: "running"}, nil
}
