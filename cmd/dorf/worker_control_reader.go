package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const workerControlReaderAddress = "0.0.0.0:8756"

type workerControlReader struct {
	server           *http.Server
	closeCheckpoints func()
	listener         net.Listener
}

func controlReaderService(store postgres.Store, tasks *absurd.Client, cfg config.Config) controlreader.Service {
	return controlreader.Service{
		Checkpoints: &workerCheckpoints{store: store, tasks: tasks, cfg: cfg},
		Workspace:   (profileRuntimeResolver{cfg: cfg, store: store, client: tasks}).workspace,
		ObservationAttention: func(ctx context.Context, session core.Session) (string, error) {
			task, err := fetchTaskResult(ctx, tasks, session.CurrentTaskID)
			if err != nil {
				return "", err
			}
			if failedExecutionTask(task.State) {
				return publicExecutionFailure(task).Code, nil
			}
			return "", nil
		},
		Store:    store,
		Runtimes: profileRuntimeResolver{cfg: cfg, store: store, client: tasks},
		Provider: configuredProviderGateway(cfg),
	}
}

// newWorkerControlReader prepares the worker's private read capability. An
// absent token keeps manually supervised workers unchanged. A present token is
// the Compose-owned opt-in and must be valid before the worker reports ready.
func newWorkerControlReader(token string, service controlreader.Service) (*workerControlReader, error) {
	return newWorkerControlReaderWithListen(token, service, net.Listen)
}

func newWorkerControlReaderWithListen(token string, service controlreader.Service, listen func(string, string) (net.Listener, error)) (*workerControlReader, error) {
	if token == "" {
		return nil, nil
	}
	handler, err := controlreader.NewHandler(token, service)
	if err != nil {
		return nil, err
	}
	listener, err := listen("tcp4", workerControlReaderAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for worker control reader: %w", err)
	}
	return &workerControlReader{
		listener: listener,
		closeCheckpoints: func() {
			if service.Checkpoints != nil {
				service.Checkpoints.Close()
			}
		},
		server: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      provider.CommandTransportTimeout,
			IdleTimeout:       60 * time.Second,
		},
	}, nil
}

func (r *workerControlReader) serve(ctx context.Context) error {
	r.server.BaseContext = func(net.Listener) context.Context { return ctx }
	err := r.server.Serve(r.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("serve worker control reader: %w", err)
	}
	return nil
}

func (r *workerControlReader) shutdown(ctx context.Context) error {
	r.closeCheckpoints()
	if err := r.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shut down worker control reader: %w", err)
	}
	return nil
}

func (r *workerControlReader) close() error {
	r.closeCheckpoints()
	if err := r.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close worker control reader: %w", err)
	}
	return nil
}

type workerProcessResult struct {
	name string
	err  error
}

// runWorkerProcesses gives durable execution and the
// optional private reader one cancellation boundary. The first process to
// stop cancels the others; the reader is then drained within the container's
// Compose stop grace period.
func runWorkerProcesses(ctx context.Context, reader *workerControlReader, runWorker func(context.Context) error, reportReady func()) error {
	if reader == nil {
		if reportReady != nil {
			reportReady()
		}
		err := runWorker(ctx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	const processCount = 2
	results := make(chan workerProcessResult, processCount)
	start := func(name string, run func() error) {
		go func() {
			results <- workerProcessResult{name: name, err: run()}
		}()
	}
	start("durable worker", func() error { return runWorker(runCtx) })
	start("control reader", func() error { return reader.serve(runCtx) })
	if reportReady != nil {
		reportReady()
	}

	first := <-results
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	shutdownErr := reader.shutdown(shutdownCtx)
	stop()
	remaining := make([]workerProcessResult, 0, processCount-1)
	for range processCount - 1 {
		remaining = append(remaining, <-results)
	}

	if err := workerProcessError(first); err != nil {
		return err
	}
	for _, result := range remaining {
		if err := workerProcessError(result); err != nil {
			return err
		}
	}
	return shutdownErr
}

func workerProcessError(result workerProcessResult) error {
	if result.err == nil || errors.Is(result.err, context.Canceled) {
		return nil
	}
	return fmt.Errorf("%s stopped: %w", result.name, result.err)
}
