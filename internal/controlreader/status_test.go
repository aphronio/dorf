package controlreader

import (
	"context"
	"errors"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"net/http"
	"strings"
	"testing"
)

type readerStatus struct {
	calls int
	store *readerTestStore
}

func (r *readerStatus) ReadSandboxStatus(_ context.Context, job core.Job, owned core.Sandbox) (provider.Status, error) {
	if !r.store.inFence || job != r.store.job || owned != r.store.sandbox {
		panic("lost custody")
	}
	r.calls++
	return provider.Status{Provider: "e2b", State: "paused"}, nil
}

func TestStatusUsesCustodyWithoutIdleReconciliation(t *testing.T) {
	job := core.Job{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	store := &readerTestStore{job: job, sandbox: owned}
	execution := &idleReaderExecution{store: store}
	status := &readerStatus{store: store}
	handler, err := NewHandler(strings.Repeat("b", 64), Service{Store: store, Runtimes: readerTestRuntimes{profile: job.SandboxProfile, execution: execution, status: status}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient("http://reader.test:8756", strings.Repeat("b", 64), &http.Client{Transport: readerHandlerTransport{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ReadSandboxStatus(context.Background(), owned.ID)
	if err != nil || result.State != "paused" || result.Provider != "e2b" || status.calls != 1 || execution.calls != 0 {
		t.Fatalf("status=%+v err=%v idle=%d", result, err, execution.calls)
	}
	store.job.CleanupState = core.CleanupRequested
	if _, err := client.ReadSandboxStatus(context.Background(), owned.ID); !errors.Is(err, ErrUnavailable) || status.calls != 1 || execution.calls != 0 {
		t.Fatalf("cleanup observation=%v", err)
	}
	store.job.CleanupState = core.CleanupPending
	store.sandbox.OwnershipNonce = ""
	if _, err := client.ReadSandboxStatus(context.Background(), owned.ID); !errors.Is(err, ErrUnavailable) || status.calls != 1 {
		t.Fatalf("foreign observation=%v", err)
	}
}
