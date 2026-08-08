package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/evidence"
	"github.com/aphronio/dorf/internal/postgres"
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
		{"resolved loss", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunFailed, NativeTurnID: "turn-failed", Resolution: &spine.MessageResolution{ID: "resolution-1", Decision: spine.ResolutionAcknowledgeLoss, Authority: "owner", ReservedWakeSequence: 3}, Settled: true}, nil, []string{"native turn failed", "resolved=acknowledge-loss", "resolution-1", "wake=3"}},
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

func TestResolveMessageCLIRequiresReasonFileAndRejectsInvalidDecisionBeforeMutation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := resolveMessage(context.Background(), postgres.Store{}, nil, nil, []string{"diagnose", "--job", "job-only"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "requires --job and --message") {
		t.Fatalf("incomplete diagnosis error=%v", err)
	}
	if err := resolveMessage(context.Background(), postgres.Store{}, nil, nil, []string{"resolve", "--job", "job-1", "--message", "message-1", "--decision", "retry", "--authority", "owner", "--dry-run"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "requires a file") {
		t.Fatalf("missing reason-file error=%v", err)
	}
	reasonPath := filepath.Join(t.TempDir(), "reason.txt")
	if err := os.WriteFile(reasonPath, []byte("complete operator reason\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resolveMessage(context.Background(), postgres.Store{}, nil, nil, []string{"resolve", "--job", "job-1", "--message", "message-1", "--decision", "blind-retry", "--authority", "owner", "--reason-file", reasonPath, "--dry-run"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "invalid message resolution decision") {
		t.Fatalf("invalid decision error=%v", err)
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
