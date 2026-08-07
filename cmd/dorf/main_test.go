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
		{"terminal failed", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunFailed, Attention: "model rejected input"}, nil, []string{"terminal", "failed", "model rejected input"}},
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
