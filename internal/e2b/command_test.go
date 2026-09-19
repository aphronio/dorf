package e2b

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	process "github.com/aphronio/dorf/internal/e2b/gen/process"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type stopProcessClient struct {
	fakeProcessClient
	listError error
	pending   int
	lists     int
}

func (c *stopProcessClient) List(ctx context.Context, _ *connect.Request[process.ListRequest]) (*connect.Response[process.ListResponse], error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.lists++
	if c.listError != nil {
		return nil, c.listError
	}
	result := &process.ListResponse{}
	if c.lists <= c.pending {
		result.Processes = []*process.ProcessInfo{{Pid: 42}}
	}
	return connect.NewResponse(result), nil
}
func TestRunStopsLostObservationAndConfirmsAbsence(t *testing.T) {
	for _, test := range []struct {
		name      string
		pid       uint32
		listError error
		stopped   bool
	}{
		{name: "observed-start", pid: 42, stopped: true},
		{name: "unknown-start", stopped: false},
		{name: "stop-unconfirmed", pid: 42, listError: errors.New("unavailable"), stopped: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := &fakeStartStream{err: context.Canceled}
			if test.pid != 0 {
				stream.messages = []*process.StartResponse{startEvent(test.pid)}
			}
			rpc := &stopProcessClient{fakeProcessClient: fakeProcessClient{stream: stream}, listError: test.listError, pending: 1}
			result, err := (&Executor{process: rpc}).Run(t.Context(), provider.RunRequest{Args: []string{"sleep", "30"}, Timeout: time.Minute})
			if err == nil || result.Stopped != test.stopped || rpc.startCalls != 1 {
				t.Fatalf("stopped=%t err=%v starts=%d", result.Stopped, err, rpc.startCalls)
			}
			if test.pid != 0 && (rpc.signalCalls != 1 || rpc.signalPID != test.pid) {
				t.Fatal("did not stop exact observed PID")
			}
			if test.stopped && rpc.lists != 2 {
				t.Fatal("signal acknowledgement treated as termination")
			}
			if test.pid == 0 && rpc.signalCalls != 0 {
				t.Fatal("guessed process identity")
			}
		})
	}
}
func TestRunAlreadyCancelledDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rpc := &fakeProcessClient{}
	result, err := (&Executor{process: rpc}).Run(ctx, provider.RunRequest{Args: []string{"true"}, Timeout: time.Second})
	if !errors.Is(err, context.Canceled) || !result.Stopped || rpc.startCalls != 0 {
		t.Fatal("cancelled command started")
	}
}

func TestRunPreservesStdinAndReportsConfirmedProcessTimeout(t *testing.T) {
	input := []byte{'a', 0, 0xff}
	rpc := &stopProcessClient{fakeProcessClient: fakeProcessClient{stream: &fakeStartStream{
		messages: []*process.StartResponse{startEvent(42)},
		err:      connect.NewError(connect.CodeDeadlineExceeded, errors.New("process deadline")),
	}}}
	result, err := (&Executor{process: rpc}).Run(t.Context(), provider.RunRequest{
		Args: []string{"python3", "-c", "import sys; sys.stdin.read()"}, Stdin: input, Timeout: time.Second,
	})
	if !errors.Is(err, provider.ErrCommandTimeout) || !result.Stopped || rpc.signalPID != 42 {
		t.Fatalf("stopped=%t signal=%d error=%v", result.Stopped, rpc.signalPID, err)
	}
	// Stdin delivery is asynchronous when a timeout interrupts observation.
	// Successful completion below establishes the input contract deterministically.
	rpc = &stopProcessClient{fakeProcessClient: fakeProcessClient{stream: &fakeStartStream{
		messages: []*process.StartResponse{startEvent(42), endEvent(0, true, "exited", "")},
	}}}
	result, err = (&Executor{process: rpc}).Run(t.Context(), provider.RunRequest{Args: []string{"consume"}, Stdin: input, Timeout: time.Second})
	if err != nil || !result.Stopped || !bytes.Equal(rpc.stdin, input) || rpc.closedPID != 42 {
		t.Fatalf("stdin completion failed: %v", err)
	}
}

func TestConvenienceExecUsesSameStopConfirmation(t *testing.T) {
	rpc := &stopProcessClient{fakeProcessClient: fakeProcessClient{stream: &fakeStartStream{
		messages: []*process.StartResponse{startEvent(42)}, err: context.Canceled,
	}}, pending: 1}
	scope := &scopedAccess{ctx: t.Context(), executor: &Executor{process: rpc}}
	_, err := scope.Exec(t.Context(), provider.Ownership{}, nil, "sleep", "30")
	if !errors.Is(err, context.Canceled) || rpc.signalPID != 42 || rpc.lists != 2 {
		t.Fatalf("convenience execution bypassed stop confirmation: %v", err)
	}
}

func TestRunDrainsCappedOutputThroughNonzeroExit(t *testing.T) {
	payload := []byte(strings.Repeat("x", provider.MaxCommandOutputBytes+1))
	rpc := &fakeProcessClient{stream: &fakeStartStream{messages: []*process.StartResponse{
		startEvent(42), stdoutEvent(payload), stderrEvent(payload), stdoutEvent([]byte("tail")), endEvent(17, true, "exited", ""),
	}}}
	result, err := (&Executor{process: rpc}).Run(t.Context(), provider.RunRequest{
		Args: []string{"noisy"}, Timeout: time.Second, MaxOutputBytes: provider.MaxCommandOutputBytes,
	})
	want := string(payload[:provider.MaxCommandOutputBytes])
	if err != nil || !result.Stopped || result.ExitCode != 17 || !result.Truncated || result.Stdout != want || result.Stderr != want {
		t.Fatalf("capped output lost exit or bounds: stdout=%d stderr=%d truncated=%t stopped=%t exit=%d err=%v", len(result.Stdout), len(result.Stderr), result.Truncated, result.Stopped, result.ExitCode, err)
	}
}
