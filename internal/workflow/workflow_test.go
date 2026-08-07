package workflow

import (
	"testing"

	"github.com/aphronio/dorf/internal/spine"
)

func TestCleanupEligibilityIncludesObservedAndTerminalAbsurdRuns(t *testing.T) {
	for _, test := range []struct {
		name      string
		jobState  spine.JobState
		taskState string
		want      bool
	}{
		{"observed", spine.JobObserved, "completed", true},
		{"failed", spine.JobRunning, "failed", true},
		{"cancelled", spine.JobRunning, "cancelled", true},
		{"pending", spine.JobRunning, "pending", false},
		{"active", spine.JobRunning, "running", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanupEligible(test.jobState, test.taskState); got != test.want {
				t.Fatalf("cleanupEligible(%q, %q) = %v", test.jobState, test.taskState, got)
			}
		})
	}
}
