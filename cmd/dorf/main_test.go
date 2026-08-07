package main

import (
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/spine"
)

func TestInspectionExplainsQueuedActiveTerminalBlockedAndUncertain(t *testing.T) {
	tests := []struct {
		name string
		view spine.MessageView
		all  []spine.MessageView
		want []string
	}{
		{"active", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunActive, NativeTurnID: "turn-1"}, nil, []string{"active native turn", "turn-1"}},
		{"terminal failed", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunFailed, NativeTurnID: "turn-failed", Attention: "model rejected input"}, nil, []string{"terminal", "failed", "model rejected input"}},
		{"cleanup no submit", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunFailed, Attention: "cleanup closed delivery after native history proved no turn was submitted"}, nil, []string{"terminal locally", "before any native turn", "cleanup closed delivery"}},
		{"uncertain", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunUncertain, Attention: "two native turns appeared"}, nil, []string{"genuinely uncertain", "stopped without resubmission", "two native turns"}},
		{"blocked queued", spine.MessageView{Message: spine.Message{Sequence: 2}, State: spine.AgentRunPending, BlockingSeq: 1, BlockingReason: "failed: native failure"}, nil, []string{"queued", "blocked by sequence 1", "native failure"}},
		{"waiting active", spine.MessageView{Message: spine.Message{Sequence: 2}, State: spine.AgentRunPending}, []spine.MessageView{{Message: spine.Message{Sequence: 1}, State: spine.AgentRunActive}}, []string{"queued", "waiting behind sequence 1", "active"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := describeMessage(test.view, test.all)
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Fatalf("description %q is missing %q", got, want)
				}
			}
		})
	}
}

func TestReadinessUsesOnlyObservedChecksForExactCurrentRevision(t *testing.T) {
	job := spine.Job{Revision: strings.Repeat("b", 40), WorkflowPhase: "ready"}
	historical := spine.Check{Revision: strings.Repeat("a", 40), State: "passed"}
	if got := readiness(job, []spine.Check{historical}); !strings.Contains(got, "no observed Check") {
		t.Fatalf("historical Evidence proved readiness: %q", got)
	}
	current := spine.Check{Revision: job.Revision, State: "passed"}
	if got := readiness(job, []spine.Check{historical, current}); !strings.HasPrefix(got, "ready") || !strings.Contains(got, "exact current Revision") {
		t.Fatalf("current readiness=%q", got)
	}
	job.WorkflowPhase = "blocked"
	if got := readiness(job, []spine.Check{current}); !strings.HasPrefix(got, "blocked") {
		t.Fatalf("blocked readiness=%q", got)
	}
}
