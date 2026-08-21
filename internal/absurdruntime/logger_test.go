package absurdruntime

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestWorkerLoggerDescribesEventTimeoutAsAttemptTimeout(t *testing.T) {
	var output bytes.Buffer
	logger := WorkerLogger(&output)

	timeout := fmt.Errorf("timed out waiting for event %q: %w", "wake", &absurd.TimeoutError{})
	logger.Printf(taskExecutionFailedFormat, timeout)

	got := output.String()
	if !strings.Contains(got, "task attempt timed out waiting for event") {
		t.Fatalf("log = %q", got)
	}
	if strings.Contains(got, "task execution failed") {
		t.Fatalf("log retains misleading task failure: %q", got)
	}
}

func TestWorkerLoggerDescribesOtherFailuresAsAttemptFailures(t *testing.T) {
	var output bytes.Buffer
	logger := WorkerLogger(&output)

	logger.Printf(taskExecutionFailedFormat, errors.New("boom"))

	got := output.String()
	if !strings.Contains(got, "task attempt failed: boom") {
		t.Fatalf("log = %q", got)
	}
}

func TestWorkerLoggerPreservesOtherAbsurdLogs(t *testing.T) {
	var output bytes.Buffer
	logger := WorkerLogger(&output)

	logger.Printf("[absurd] worker started: %s", "dorf_jobs")

	got := output.String()
	if !strings.Contains(got, "[absurd] worker started: dorf_jobs") {
		t.Fatalf("log = %q", got)
	}
}
