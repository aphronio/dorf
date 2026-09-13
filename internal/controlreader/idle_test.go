package controlreader

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

type idleReaderExecution struct {
	core.Execution
	store *readerTestStore
	calls int
}

func (e *idleReaderExecution) ReconcileIdleSandboxes(_ context.Context, jobID string) error {
	if e.store.inFence {
		panic("idle reconciliation nested the reader fence")
	}
	if jobID != e.store.job.ID {
		panic("idle reconciliation changed Job")
	}
	e.calls++
	return errors.New("provider temporarily busy")
}

func TestFileReadReconcilesIdleAfterFenceWithoutLosingResult(t *testing.T) {
	job := core.Job{ID: "job-1", SandboxProfile: "profile-1", AdmissionOpen: true, CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	store := &readerTestStore{job: job, sandbox: owned}
	execution := &idleReaderExecution{store: store}
	service := Service{Store: store, Runtimes: readerTestRuntimes{profile: job.SandboxProfile, files: &readerTestFiles{contents: []byte("retained result")}, execution: execution}}
	result, err := service.ReadFile(context.Background(), owned.ID, "result.txt")
	if err != nil || string(result) != "retained result" || execution.calls != 1 || store.activityStarts != 1 || store.activityFinishes != 1 {
		t.Fatalf("read=%q error=%v idle calls=%d", result, err, execution.calls)
	}
}
