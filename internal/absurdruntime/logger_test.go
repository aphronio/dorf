package absurdruntime

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestWorkerLogger(t *testing.T) {
	for _, test := range []struct {
		name      string
		format    string
		arg       any
		want      string
		forbidden string
	}{
		{"event timeout", taskExecutionFailedFormat, fmt.Errorf("timed out waiting for event %q: %w", "wake", &absurd.TimeoutError{}), "task attempt timed out waiting for event", "task execution failed"},
		{"other failure", taskExecutionFailedFormat, errors.New("boom"), "task attempt failed: boom", "task execution failed"},
		{"other log", "[absurd] worker started: %s", "dorf_jobs", "[absurd] worker started: dorf_jobs", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			WorkerLogger(&output).Printf(test.format, test.arg)
			got := output.String()
			if !strings.Contains(got, test.want) || test.forbidden != "" && strings.Contains(got, test.forbidden) {
				t.Fatalf("log = %q", got)
			}
		})
	}
}
