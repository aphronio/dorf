package e2b

import (
	"context"
	"errors"
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
