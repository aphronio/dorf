package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/controlreader"
)

func TestWorkerControlReaderIsOptInAndBindsItsFixedPrivatePort(t *testing.T) {
	called := false
	listen := func(network, address string) (net.Listener, error) {
		called = true
		if network != "tcp4" || address != workerControlReaderAddress {
			t.Fatalf("listen %s %s", network, address)
		}
		return net.Listen("tcp4", "127.0.0.1:0")
	}
	reader, err := newWorkerControlReaderWithListen("", controlreader.Service{}, listen)
	if err != nil || reader != nil || called {
		t.Fatalf("absent token reader=%#v called=%t error=%v", reader, called, err)
	}

	token := strings.Repeat("a", 64)
	reader, err = newWorkerControlReaderWithListen(token, controlreader.Service{}, listen)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.close() })
	if !called {
		t.Fatal("listener was not bound")
	}

	served := make(chan error, 1)
	go func() { served <- reader.serve(context.Background()) }()
	client, err := controlreader.NewClient("http://"+reader.listener.Addr().String(), token, nil)
	if err != nil {
		t.Fatal(err)
	}
	healthCtx, cancelHealth := context.WithTimeout(context.Background(), time.Second)
	defer cancelHealth()
	if err := client.Health(healthCtx); err != nil {
		t.Fatalf("health: %v", err)
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err := reader.shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

func TestWorkerControlReaderRejectsInvalidTokenBeforeBinding(t *testing.T) {
	called := false
	reader, err := newWorkerControlReaderWithListen("not-a-token", controlreader.Service{}, func(string, string) (net.Listener, error) {
		called = true
		return nil, errors.New("must not listen")
	})
	if err == nil || reader != nil || called {
		t.Fatalf("reader=%#v called=%t error=%v", reader, called, err)
	}
}

func TestWorkerControlReaderFailureCancelsDurableProcesses(t *testing.T) {
	token := strings.Repeat("b", 64)
	reader, err := newWorkerControlReaderWithListen(token, controlreader.Service{}, func(string, string) (net.Listener, error) {
		return net.Listen("tcp4", "127.0.0.1:0")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.close(); err != nil {
		t.Fatal(err)
	}

	var workerCancelled atomic.Bool
	var recoveryCancelled atomic.Bool
	err = runWorkerProcesses(context.Background(), reader,
		func(ctx context.Context) error {
			<-ctx.Done()
			workerCancelled.Store(true)
			return ctx.Err()
		},
		func(ctx context.Context) error {
			<-ctx.Done()
			recoveryCancelled.Store(true)
			return ctx.Err()
		},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "control reader stopped") {
		t.Fatalf("error=%v", err)
	}
	if !workerCancelled.Load() || !recoveryCancelled.Load() {
		t.Fatalf("worker cancelled=%t recovery cancelled=%t", workerCancelled.Load(), recoveryCancelled.Load())
	}
}

func TestWorkerProcessesWithoutReaderRetainManualLifecycle(t *testing.T) {
	want := errors.New("worker stopped")
	err := runWorkerProcesses(context.Background(), nil,
		func(context.Context) error { return want },
		func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		nil,
	)
	if !errors.Is(err, want) || err.Error() != want.Error() {
		t.Fatalf("error=%v", err)
	}
}

func TestWorkerProcessesGracefullyShutDownReaderWithParent(t *testing.T) {
	token := strings.Repeat("c", 64)
	reader, err := newWorkerControlReaderWithListen(token, controlreader.Service{}, func(string, string) (net.Listener, error) {
		return net.Listen("tcp4", "127.0.0.1:0")
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	wait := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	go func() { done <- runWorkerProcesses(ctx, reader, wait, wait, nil) }()

	client, err := controlreader.NewClient("http://"+reader.listener.Addr().String(), token, nil)
	if err != nil {
		t.Fatal(err)
	}
	healthCtx, cancelHealth := context.WithTimeout(context.Background(), time.Second)
	defer cancelHealth()
	for {
		err = client.Health(healthCtx)
		if err == nil {
			break
		}
		if healthCtx.Err() != nil {
			t.Fatalf("reader did not become healthy: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker processes did not stop")
	}
}
