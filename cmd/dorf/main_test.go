package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/evidence"
	"github.com/aphronio/dorf/internal/spine"
)

func TestPublicationGrammarParsesJobBeforeFlags(t *testing.T) {
	set := flag.NewFlagSet("publication publish", flag.ContinueOnError)
	revision := set.String("revision", "", "")
	jobID, err := parsePublicationTarget(set, []string{"job-exact", "--revision", strings.Repeat("a", 40)}, "publication publish")
	if err != nil || jobID != "job-exact" || *revision != strings.Repeat("a", 40) {
		t.Fatalf("job=%q revision=%q err=%v", jobID, *revision, err)
	}
	if _, err := parsePublicationTarget(flag.NewFlagSet("publication publish", flag.ContinueOnError), []string{"--revision", strings.Repeat("a", 40), "job-exact"}, "publication publish"); err == nil {
		t.Fatal("flags-before-Job grammar was accepted")
	}
}

func TestInspectionExplainsQueuedActiveTerminalBlockedAndUncertain(t *testing.T) {
	tests := []struct {
		name string
		view spine.MessageView
		all  []spine.MessageView
		want []string
	}{
		{"active", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunActive, NativeTurnID: "turn-1"}, nil, []string{"active native turn", "turn-1"}},
		{"terminal failed", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunFailed, NativeTurnID: "turn-failed", Attention: "model rejected input"}, nil, []string{"terminal", "failed", "model rejected input"}},
		{"cleanup no submit", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunFailed, Attention: "cleanup closed delivery after native history proved no turn was submitted"}, nil, []string{"cleanup closed", "no native acceptance", "history proved"}},
		{"uncertain", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunUncertain, Attention: "two native turns appeared"}, nil, []string{"genuinely uncertain", "stopped without resubmission", "two native turns"}},
		{"blocked queued", spine.MessageView{Message: spine.Message{Sequence: 2}, State: spine.AgentRunPending, BlockingSeq: 1, BlockingReason: "failed: native failure"}, nil, []string{"queued", "blocked by sequence 1", "native failure"}},
		{"waiting active", spine.MessageView{Message: spine.Message{Sequence: 2}, State: spine.AgentRunPending}, []spine.MessageView{{Message: spine.Message{Sequence: 1}, State: spine.AgentRunActive}}, []string{"queued", "waiting behind sequence 1", "active"}},
		{"accepted failure", spine.MessageView{Message: spine.Message{Sequence: 1, Intent: spine.MessageFollow}, State: spine.AgentRunFailed, NativeTurnID: "turn-failed", NativeOutcome: "failed", Delivered: true}, nil, []string{"native turn failed", "turn-failed", "outcome=failed"}},
		{"accepted steer", spine.MessageView{Message: spine.Message{Sequence: 2, Intent: spine.MessageSteer, TargetTurnID: "turn-active"}, State: spine.AgentRunCompleted, NativeTurnID: "turn-active", Delivered: true}, nil, []string{"steer accepted", "turn-active"}},
		{"terminal-race steer", spine.MessageView{Message: spine.Message{Sequence: 2, Intent: spine.MessageSteer, TargetTurnID: "turn-original"}, State: spine.AgentRunCompleted, NativeTurnID: "turn-fallback", NativeOutcome: "completed", Delivered: true}, nil, []string{"target became terminal", "turn-fallback", "requested steer target=turn-original"}},
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

func TestInspectionReadinessDoesNotTrustPersistedReadyPhaseWithoutEvidence(t *testing.T) {
	job := spine.Job{ID: "job-inspect", Revision: strings.Repeat("b", 40), WorkflowPhase: "ready"}
	assessment := spine.AssessReadiness(job, []spine.DeclaredCheck{{Name: "check", Command: "go test ./..."}}, nil, nil, evidence.Store{Root: t.TempDir()})
	if assessment.Ready || assessment.Status != "not_ready" || !strings.Contains(assessment.Reason, "Evidence") {
		t.Fatalf("row-only readiness=%#v", assessment)
	}
	job.WorkflowPhase = "blocked"
	assessment = spine.AssessReadiness(job, nil, nil, nil, evidence.Store{Root: t.TempDir()})
	if assessment.Status != "blocked" {
		t.Fatalf("blocked readiness=%#v", assessment)
	}
}
