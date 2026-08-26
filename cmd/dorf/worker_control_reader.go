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
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const workerControlReaderAddress = "0.0.0.0:8756"

type workerControlReader struct {
	server   *http.Server
	listener net.Listener
}

func controlReaderService(store postgres.Store, tasks *absurd.Client, cfg config.Config) controlreader.Service {
	return controlreader.Service{
		Store:    store,
		Runtimes: profileRuntimeResolver{cfg: cfg, store: store, client: tasks},
		Provider: configuredProviderGateway(cfg),
		Installations: githubapi.Client{
			APIURL: cfg.GitHubAPIURL, Credentials: cfg.GitHubCredentials,
		},
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
		server: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      25 * time.Second,
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
	if err := r.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shut down worker control reader: %w", err)
	}
	return nil
}

func (r *workerControlReader) close() error {
	if err := r.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close worker control reader: %w", err)
	}
	return nil
}

type workerProcessResult struct {
	name string
	err  error
}

// runWorkerProcesses gives durable execution, cleanup recovery, and the
// optional private reader one cancellation boundary. The first process to
// stop cancels the others; the reader is then drained within the container's
// Compose stop grace period.
func runWorkerProcesses(ctx context.Context, reader *workerControlReader, runWorker, recoverCleanup func(context.Context) error, reportReady func()) error {
	if reader == nil {
		return runWorkerWithoutControlReader(ctx, runWorker, recoverCleanup, reportReady)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	const processCount = 3
	results := make(chan workerProcessResult, processCount)
	start := func(name string, run func() error) {
		go func() {
			results <- workerProcessResult{name: name, err: run()}
		}()
	}
	start("durable worker", func() error { return runWorker(runCtx) })
	start("cleanup recovery", func() error { return recoverCleanup(runCtx) })
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

// runWorkerWithoutControlReader retains the manually supervised worker's
// established lifecycle when the deployment-only reader token is absent.
func runWorkerWithoutControlReader(ctx context.Context, runWorker, recoverCleanup func(context.Context) error, reportReady func()) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	recoveryDone := make(chan error, 1)
	go func() {
		err := recoverCleanup(runCtx)
		recoveryDone <- err
		if err != nil && !errors.Is(err, context.Canceled) {
			cancel()
		}
	}()
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- runWorker(runCtx)
	}()
	if reportReady != nil {
		reportReady()
	}
	err := <-workerDone
	cancel()
	recoveryErr := <-recoveryDone
	if recoveryErr != nil && !errors.Is(recoveryErr, context.Canceled) {
		return recoveryErr
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func workerProcessError(result workerProcessResult) error {
	if result.err == nil || errors.Is(result.err, context.Canceled) {
		return nil
	}
	return fmt.Errorf("%s stopped: %w", result.name, result.err)
}
