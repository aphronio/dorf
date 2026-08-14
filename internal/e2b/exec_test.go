package e2b

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	process "github.com/aphronio/dorf/internal/e2b/gen/process"
)

func TestExecPreservesArgvStdinRawOutputAndTerminalStatus(t *testing.T) {
	rpc := &fakeProcessClient{stream: &fakeStartStream{messages: []*process.StartResponse{
		startEvent(42),
		stdoutEvent([]byte{'o', 0, 0xff}),
		keepaliveEvent(),
		stderrEvent([]byte{'e', 0, 0xfe}),
		endEvent(0, true, "exited", ""),
	}}}
	executor := &Executor{process: rpc}
	stdin := []byte{'i', 0, 0xfd}
	var stdout, stderr bytes.Buffer
	result, err := executor.Exec(context.Background(), ExecRequest{
		Argv:           []string{"command", "literal arg", "*.go", "$HOME"},
		Stdin:          stdin,
		Cwd:            "/workspace/job",
		Env:            map[string]string{"EXACT": "a b"},
		ProcessTimeout: 1500 * time.Millisecond,
		Stdout:         &stdout,
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PID != 42 || result.ExitCode != 0 || !result.Exited || result.Status != "exited" {
		t.Fatalf("result = %#v", result)
	}
	if !bytes.Equal(stdout.Bytes(), []byte{'o', 0, 0xff}) || !bytes.Equal(stderr.Bytes(), []byte{'e', 0, 0xfe}) {
		t.Fatalf("stdout=%v stderr=%v", stdout.Bytes(), stderr.Bytes())
	}
	request := rpc.startRequest.Msg
	if request.Process.Cmd != "command" || strings.Join(request.Process.Args, "|") != "literal arg|*.go|$HOME" {
		t.Fatalf("process argv = %#v", request.Process)
	}
	if request.Process.GetCwd() != "/workspace/job" || request.Process.Envs["EXACT"] != "a b" || request.Stdin == nil || !request.GetStdin() {
		t.Fatalf("process config = %#v", request)
	}
	if rpc.startRequest.Header().Get("Connect-Timeout-Ms") != "1500" || rpc.startRequest.Header().Get("Keepalive-Ping-Interval") != "50" {
		t.Fatalf("start headers = %#v", rpc.startRequest.Header())
	}
	if !bytes.Equal(rpc.stdin, stdin) || rpc.stdinPID != 42 || rpc.closedPID != 42 {
		t.Fatalf("stdin=%v stdinPID=%d closedPID=%d", rpc.stdin, rpc.stdinPID, rpc.closedPID)
	}
}

func TestExecReturnsTypedExitAndIndeterminateErrors(t *testing.T) {
	t.Run("completed process wins close stdin race", func(t *testing.T) {
		rpc := &fakeProcessClient{
			stream:     &fakeStartStream{messages: []*process.StartResponse{startEvent(6), endEvent(0, true, "exited", "")}},
			closeError: connect.NewError(connect.CodeNotFound, errors.New("process already exited")),
		}
		result, err := (&Executor{process: rpc}).Exec(context.Background(), ExecRequest{Argv: []string{"consume-and-exit"}, Stdin: []byte("complete input")})
		if err != nil || result.ExitCode != 0 || !result.Exited {
			t.Fatalf("result=%#v error=%v", result, err)
		}
		if !bytes.Equal(rpc.stdin, []byte("complete input")) {
			t.Fatalf("stdin=%q", rpc.stdin)
		}
	})

	t.Run("nonzero terminal event", func(t *testing.T) {
		rpc := &fakeProcessClient{stream: &fakeStartStream{messages: []*process.StartResponse{
			startEvent(7), endEvent(17, true, "exited", "command failed"),
		}}}
		result, err := (&Executor{process: rpc}).Exec(context.Background(), ExecRequest{Argv: []string{"false"}})
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || result.ExitCode != 17 || exitErr.Result.RemoteError != "command failed" {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("EOF before terminal event", func(t *testing.T) {
		rpc := &fakeProcessClient{stream: &fakeStartStream{messages: []*process.StartResponse{startEvent(8)}}}
		result, err := (&Executor{process: rpc}).Exec(context.Background(), ExecRequest{Argv: []string{"sleep", "10"}})
		var indeterminateErr *IndeterminateExecError
		if !errors.As(err, &indeterminateErr) || !errors.Is(err, io.ErrUnexpectedEOF) || result.PID != 8 || indeterminateErr.PID != 8 {
			t.Fatalf("result=%#v error=%v", result, err)
		}
		if rpc.startCalls != 1 {
			t.Fatalf("Start calls = %d, want no replay", rpc.startCalls)
		}
	})

	t.Run("canceled observation", func(t *testing.T) {
		rpc := &fakeProcessClient{stream: &fakeStartStream{messages: []*process.StartResponse{startEvent(9)}, err: context.Canceled}}
		_, err := (&Executor{process: rpc}).Exec(context.Background(), ExecRequest{Argv: []string{"sleep", "10"}})
		var indeterminateErr *IndeterminateExecError
		if !errors.As(err, &indeterminateErr) || !errors.Is(err, context.Canceled) || indeterminateErr.PID != 9 {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("provider process timeout", func(t *testing.T) {
		rpc := &fakeProcessClient{stream: &fakeStartStream{
			messages: []*process.StartResponse{startEvent(10)},
			err:      connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded),
		}}
		_, err := (&Executor{process: rpc}).Exec(context.Background(), ExecRequest{Argv: []string{"sleep", "10"}, ProcessTimeout: 500 * time.Millisecond})
		var timeoutErr *ProcessTimeoutError
		if !errors.As(err, &timeoutErr) || timeoutErr.PID != 10 || timeoutErr.Timeout != 500*time.Millisecond {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("data before start", func(t *testing.T) {
		rpc := &fakeProcessClient{stream: &fakeStartStream{messages: []*process.StartResponse{stdoutEvent([]byte("bad"))}}}
		_, err := (&Executor{process: rpc}).Exec(context.Background(), ExecRequest{Argv: []string{"true"}})
		var indeterminateErr *IndeterminateExecError
		if !errors.As(err, &indeterminateErr) || !strings.Contains(err.Error(), "before start") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestKillSendsOneExactSignal(t *testing.T) {
	rpc := &fakeProcessClient{}
	executor := &Executor{process: rpc}
	if err := executor.Kill(context.Background(), 77); err != nil {
		t.Fatal(err)
	}
	if rpc.signalCalls != 1 || rpc.signalPID != 77 || rpc.signal != process.Signal_SIGNAL_SIGKILL {
		t.Fatalf("signal calls=%d pid=%d signal=%v", rpc.signalCalls, rpc.signalPID, rpc.signal)
	}
}

func TestEnvdConnectionUsesScopedHeadersAndVersionGate(t *testing.T) {
	connection := EnvdConnection{ProviderID: "sandbox-1", Domain: "e2b.app", Version: "0.5.2", accessToken: "scoped-token"}
	baseURL, err := envdBaseURL(connection)
	if err != nil || baseURL != "https://sandbox.e2b.app" {
		t.Fatalf("base URL=%q error=%v", baseURL, err)
	}
	var observed *http.Request
	client := envdHTTPClient{next: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		observed = request
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	}), providerID: connection.ProviderID, accessToken: connection.accessToken}
	request, _ := http.NewRequest(http.MethodPost, baseURL+"/process.Process/Start", nil)
	if _, err := client.Do(request); err != nil {
		t.Fatal(err)
	}
	if observed.Header.Get("E2b-Sandbox-Id") != "sandbox-1" || observed.Header.Get("E2b-Sandbox-Port") != envdPort || observed.Header.Get("X-Access-Token") != "scoped-token" {
		t.Fatalf("envd headers = %#v", observed.Header)
	}
	if rendered := fmt.Sprintf("%v %#v", connection, connection); strings.Contains(rendered, connection.accessToken) {
		t.Fatalf("envd connection formatting leaked scoped token: %s", rendered)
	}
	if _, err := NewExecutor(EnvdConnection{ProviderID: "sandbox-1", Domain: "e2b.app", Version: "0.5.1", accessToken: "token"}, nil); err == nil || !strings.Contains(err.Error(), minimumEnvdVersion) {
		t.Fatalf("old envd error = %v", err)
	}
}

func TestObservationContextKeepsCancellationButHidesDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	hidden := observationContext{Context: ctx}
	if _, ok := hidden.Deadline(); ok {
		t.Fatal("observation context exposed caller deadline to Connect-Go")
	}
	cancel()
	select {
	case <-hidden.Done():
	default:
		t.Fatal("observation context lost caller cancellation")
	}
	if !errors.Is(hidden.Err(), context.Canceled) {
		t.Fatalf("observation error = %v", hidden.Err())
	}
}

type fakeStartStream struct {
	messages []*process.StartResponse
	err      error
	index    int
}

func (s *fakeStartStream) Receive() bool {
	if s.index >= len(s.messages) {
		return false
	}
	s.index++
	return true
}

func (s *fakeStartStream) Msg() *process.StartResponse { return s.messages[s.index-1] }
func (s *fakeStartStream) Err() error                  { return s.err }
func (s *fakeStartStream) Close() error                { return nil }

type fakeProcessClient struct {
	mu           sync.Mutex
	stream       startStream
	startRequest *connect.Request[process.StartRequest]
	startCalls   int
	stdin        []byte
	stdinPID     uint32
	closedPID    uint32
	closeError   error
	signalCalls  int
	signalPID    uint32
	signal       process.Signal
}

func (c *fakeProcessClient) Start(_ context.Context, request *connect.Request[process.StartRequest]) (startStream, error) {
	c.startCalls++
	c.startRequest = request
	return c.stream, nil
}

func (c *fakeProcessClient) SendInput(_ context.Context, request *connect.Request[process.SendInputRequest]) (*connect.Response[process.SendInputResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stdinPID = request.Msg.Process.GetPid()
	c.stdin = append([]byte(nil), request.Msg.Input.GetStdin()...)
	return connect.NewResponse(&process.SendInputResponse{}), nil
}

func (c *fakeProcessClient) CloseStdin(_ context.Context, request *connect.Request[process.CloseStdinRequest]) (*connect.Response[process.CloseStdinResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closedPID = request.Msg.Process.GetPid()
	return connect.NewResponse(&process.CloseStdinResponse{}), c.closeError
}

func (c *fakeProcessClient) SendSignal(_ context.Context, request *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error) {
	c.signalCalls++
	c.signalPID = request.Msg.Process.GetPid()
	c.signal = request.Msg.Signal
	return connect.NewResponse(&process.SendSignalResponse{}), nil
}

func startEvent(pid uint32) *process.StartResponse {
	return &process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: pid}}}}
}

func stdoutEvent(data []byte) *process.StartResponse {
	return &process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: data}}}}}
}

func stderrEvent(data []byte) *process.StartResponse {
	return &process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: data}}}}}
}

func keepaliveEvent() *process.StartResponse {
	return &process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Keepalive{Keepalive: &process.ProcessEvent_KeepAlive{}}}}
}

func endEvent(exitCode int32, exited bool, status, remoteError string) *process.StartResponse {
	var optionalError *string
	if remoteError != "" {
		optionalError = &remoteError
	}
	return &process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{
		ExitCode: exitCode, Exited: exited, Status: status, Error: optionalError,
	}}}}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }
