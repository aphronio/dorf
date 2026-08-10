package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/evidence"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestOutcomeGrammarExposesOneExactBoundary(t *testing.T) {
	for _, args := range [][]string{nil, {"job-only"}, {"job", "accepted", "extra"}} {
		err := outcomeCommand(context.Background(), postgres.Store{}, nil, githubapi.Client{}, args, io.Discard)
		if err == nil || err.Error() != "outcome requires: JOB_ID <accepted|rejected|abandoned>" {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestInspectionExplainsQueuedActiveTerminalBlockedAndUncertain(t *testing.T) {
	tests := []struct {
		name string
		view spine.MessageView
		all  []spine.MessageView
		want []string
	}{
		{"active", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunActive, Harness: "codex", ThreadID: "thread-1", TurnID: "turn-1"}, nil, []string{"active harness turn", "harness=codex", "thread=thread-1", "turn=turn-1"}},
		{"terminal failed", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunFailed, TurnID: "turn-failed", Attention: "model rejected input"}, nil, []string{"terminal", "failed", "model rejected input"}},
		{"cleanup no submit", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunFailed, Attention: "cleanup closed delivery after native history proved no turn was submitted"}, nil, []string{"cleanup closed", "no harness acceptance", "history proved"}},
		{"uncertain", spine.MessageView{Message: spine.Message{Sequence: 1}, State: spine.AgentRunUncertain, Attention: "two native turns appeared"}, nil, []string{"genuinely uncertain", "stopped without resubmission", "two native turns"}},
		{"blocked queued", spine.MessageView{Message: spine.Message{Sequence: 2}, State: spine.AgentRunPending, BlockingSeq: 1, BlockingReason: "failed: native failure"}, nil, []string{"queued", "blocked by sequence 1", "native failure"}},
		{"waiting active", spine.MessageView{Message: spine.Message{Sequence: 2}, State: spine.AgentRunPending}, []spine.MessageView{{Message: spine.Message{Sequence: 1}, State: spine.AgentRunActive}}, []string{"queued", "waiting behind sequence 1", "active"}},
		{"accepted failure", spine.MessageView{Message: spine.Message{Sequence: 1, Intent: spine.MessageFollow}, State: spine.AgentRunFailed, TurnID: "turn-failed", TurnOutcome: "failed", Delivered: true}, nil, []string{"harness turn failed", "turn-failed", "outcome=failed"}},
		{"accepted steer", spine.MessageView{Message: spine.Message{Sequence: 2, Intent: spine.MessageSteer, TargetTurnID: "turn-active"}, State: spine.AgentRunCompleted, TurnID: "turn-active", Delivered: true}, nil, []string{"steer accepted", "turn-active"}},
		{"terminal-race steer", spine.MessageView{Message: spine.Message{Sequence: 2, Intent: spine.MessageSteer, TargetTurnID: "turn-original"}, State: spine.AgentRunCompleted, TurnID: "turn-fallback", TurnOutcome: "completed", Delivered: true}, nil, []string{"target became terminal", "turn-fallback", "requested steer target=turn-original"}},
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

func TestInspectionDistinguishesAutomaticContinuationFromExternalAuthority(t *testing.T) {
	job := spine.Job{WorkflowPhase: "reviewing", CleanupState: spine.CleanupPending}
	run := taskResultView{State: absurd.TaskSleeping}
	if got := continuationFor(job, nil, run, taskResultView{}); got.Mode != "self-advancing" || got.Actor != "admitted Dorf worker" {
		t.Fatalf("review continuation=%#v", got)
	}
	job.WorkflowPhase = "published"
	if got := continuationFor(job, nil, run, taskResultView{}); got.Mode != "external-authority" || !strings.Contains(got.Detail, "merge or close") {
		t.Fatalf("published continuation=%#v", got)
	}
	if got := continuationFor(job, nil, taskResultView{State: absurd.TaskFailed}, taskResultView{}); got.Mode != "attention" || !strings.Contains(got.Detail, "stopped observing") {
		t.Fatalf("failed proposal observer continuation=%#v", got)
	}
	outcome := &spine.JobOutcome{Kind: spine.OutcomeRejected}
	job.CleanupState, job.CleanupTaskID = spine.CleanupScheduled, "cleanup-task"
	if got := continuationFor(job, outcome, run, taskResultView{State: absurd.TaskPending}); got.Mode != "automatic-cleanup" || got.Actor != "Dorf cleanup task" {
		t.Fatalf("outcome continuation=%#v", got)
	}
	job.CleanupState = spine.CleanupComplete
	if got := continuationFor(job, outcome, run, taskResultView{State: absurd.TaskCompleted}); got.Mode != "terminal" {
		t.Fatalf("clean continuation=%#v", got)
	}
	job.WorkflowPhase = "publication-blocked"
	want := continuationStatus{Mode: "terminal", Actor: "none", Detail: "exact deterministic cleanup is complete and no GitHub proposal outcome was recorded"}
	if got := continuationFor(job, nil, run, taskResultView{State: absurd.TaskCompleted}); got != want {
		t.Fatalf("clean no-outcome continuation=%#v want=%#v", got, want)
	}
}

func TestInspectionExposesFailedDurableContinuationAsAttention(t *testing.T) {
	job := spine.Job{WorkflowPhase: "reviewing", CleanupState: spine.CleanupPending, AdmissionOpen: true}
	if got := continuationFor(job, nil, taskResultView{State: absurd.TaskFailed}, taskResultView{}); got.Mode != "attention" || !strings.Contains(got.Detail, "Job task is terminal") {
		t.Fatalf("failed Job continuation=%#v", got)
	}
}
