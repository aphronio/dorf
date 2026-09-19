package incus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

type execServer struct {
	incusclient.InstanceServer
	context   context.Context
	control   *websocket.Conn
	operation *execOperation
	started   chan struct{}
}

func (s *execServer) WithContext(ctx context.Context) incusclient.InstanceServer {
	s.context = ctx
	return s
}
func (s *execServer) ExecInstance(_ string, _ api.InstanceExecPost, args *incusclient.InstanceExecArgs) (incusclient.Operation, error) {
	go args.Control(s.control)
	close(args.DataDone)
	close(s.started)
	return s.operation, nil
}

type execOperation struct {
	incusclient.Operation
	stopped chan struct{}
}

func (o *execOperation) WaitContext(ctx context.Context) error {
	select {
	case <-o.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (o *execOperation) Get() api.Operation {
	return api.Operation{Metadata: map[string]any{"return": 143}}
}

func TestExecCancellationSignalsThenWaitsForObservedExit(t *testing.T) {
	signal := make(chan api.InstanceExecControl, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var message api.InstanceExecControl
		if err := conn.ReadJSON(&message); err == nil {
			signal <- message
		}
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	op := &execOperation{stopped: make(chan struct{})}
	sdk := &execServer{control: conn, operation: op, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := (&sdkClient{server: sdk}).Exec(ctx, "synthetic-vm", nil, 0, "sleep", "60")
		result <- err
	}()
	<-sdk.started
	cancel()
	select {
	case message := <-signal:
		if message.Command != "signal" || message.Signal != 15 {
			t.Fatalf("unexpected signal: %+v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach the remote control socket")
	}
	if sdk.context.Err() != nil {
		t.Fatal("transport closed before remote exit could be confirmed")
	}
	select {
	case <-result:
		t.Fatal("signal acknowledgement was treated as process exit")
	default:
	}
	close(op.stopped)
	select {
	case err := <-result:
		var stopped *commandStoppedError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &stopped) {
			t.Fatalf("lost cancellation or stop proof: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed cancellation did not finish")
	}
}
