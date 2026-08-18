package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestSandboxForConfigSelectsOneConcreteAdapter(t *testing.T) {
	local, err := sandboxForConfig(config.Config{SandboxProfile: config.SandboxProfileIncus, IncusImage: "dorf", IncusNetwork: "incusbr0", IncusDiskSize: "40GiB", Workspace: "/workspace/job"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := local.(incus.Adapter); !ok {
		t.Fatalf("local adapter = %T", local)
	}
	managed, err := sandboxForConfig(config.Config{
		SandboxProfile: config.SandboxProfileE2B, E2BAPIKey: "test-key", E2BTemplate: "dorf:exact-build",
		E2BGatewayURL: "https://gateway.example/v1", E2BSandboxTimeout: 55 * time.Minute,
		Workspace: "/workspace/job", TurnTimeout: 45 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := managed.(e2b.Adapter); !ok {
		t.Fatalf("managed adapter = %T", managed)
	}
	if _, err := sandboxForConfig(config.Config{SandboxProfile: config.SandboxProfileE2B, E2BTemplate: "dorf:exact-build", E2BSandboxTimeout: time.Minute, Workspace: "/workspace/job", E2BGatewayURL: "https://gateway.example/v1"}); err == nil || !strings.Contains(err.Error(), "E2B_API_KEY") {
		t.Fatalf("missing E2B API key error = %v", err)
	}
	if _, err := sandboxForConfig(config.Config{SandboxProfile: config.SandboxProfileE2B, E2BAPIKey: "test-key", E2BTemplate: "dorf:exact-build", E2BSandboxTimeout: time.Minute, Workspace: "/workspace/job", E2BGatewayURL: "http://gateway.example/v1"}); err == nil {
		t.Fatal("invalid remote Gateway URL was admitted")
	}
}

func TestWorkflowHistorySortsNaturalFactsAndIncludesRunsAndRevisions(t *testing.T) {
	base := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	entries := workflowHistory(workflow.Snapshot{
		Job: spine.Job{AdmittedAt: base},
		Deliveries: []spine.Delivery{{
			Message:  spine.Message{ID: "message-1", Sequence: 1, FromKind: spine.MessageFromHuman, AdmittedAt: base.Add(time.Second)},
			AgentRun: spine.AgentRun{ID: "run-secret", MessageID: "message-1", Role: "implement", State: spine.AgentRunCompleted, InputRevision: "revision-0", StartedAt: base.Add(4 * time.Second), FinishedAt: base.Add(5 * time.Second)},
		}},
		Actions: []spine.Action{
			{ID: "action-secret", Kind: spine.ActionSandboxCreate, State: spine.ActionSucceeded, CreatedAt: base.Add(2 * time.Second), SettledAt: base.Add(3 * time.Second)},
			{Kind: spine.ActionGitHubPullRequest, State: spine.ActionSucceeded, Scope: "revision-1", CreatedAt: base.Add(7 * time.Second), SettledAt: base.Add(8 * time.Second)},
		},
		Revisions: []spine.Revision{
			{Generation: 0, OID: "revision-0", ObservedAt: base},
			{Generation: 1, OID: "revision-1", ComparisonBase: "revision-0", ObservedAt: base.Add(6 * time.Second)},
		},
		Evidence: []spine.Evidence{{ID: "evidence-secret", Kind: "git-revision", Revision: "revision-1", FinishedAt: base.Add(6500 * time.Millisecond)}},
		Proposal: &spine.GitHubProposal{Number: 42, ProposedRevision: "revision-1"},
	})
	for i := 1; i < len(entries); i++ {
		if entries[i].At.Before(entries[i-1].At) {
			t.Fatalf("history is not chronological: %#v", entries)
		}
	}
	wantKinds := []string{"Job", "Revision", "Message", "Action", "Action", "AgentRun", "AgentRun", "Revision", "Evidence", "Action", "Action", "Proposal"}
	if len(entries) != len(wantKinds) {
		t.Fatalf("history has %d entries, want semantic events %v: %#v", len(entries), wantKinds, entries)
	}
	for i, want := range wantKinds {
		if entries[i].Kind != want {
			t.Fatalf("history event %d kind = %q, want %q: %#v", i, entries[i].Kind, want, entries)
		}
	}
	var story strings.Builder
	for _, entry := range entries {
		story.WriteString(entry.Kind + " " + entry.Detail + "\n")
	}
	for _, want := range []string{"revision-0", "revision-1", "Message 1", "#42"} {
		if !strings.Contains(story.String(), want) {
			t.Fatalf("history is missing factual token %q:\n%s", want, story.String())
		}
	}
	for _, plumbing := range []string{"action-secret", "run-secret", "message-1", "evidence-secret"} {
		if strings.Contains(story.String(), plumbing) {
			t.Fatalf("human history leaked plumbing %q:\n%s", plumbing, story.String())
		}
	}
	abandoned := workflowHistory(workflow.Snapshot{
		Job:     spine.Job{AdmittedAt: base},
		Outcome: &spine.JobOutcome{Kind: spine.OutcomeAbandoned, ObservedAt: base.Add(time.Second)},
	})
	last := abandoned[len(abandoned)-1]
	if last.Kind != "Outcome" || !strings.Contains(last.Detail, string(spine.OutcomeAbandoned)) || strings.Contains(last.Detail, "GitHub") {
		t.Fatalf("pre-Proposal abandonment history = %#v", last)
	}
}

func TestTaskResultProjectionPublishesOnlyBoundedFailureMessage(t *testing.T) {
	view := projectTaskResult("task-1", &absurd.TaskResultSnapshot{
		State:   absurd.TaskFailed,
		Failure: json.RawMessage(`{"name":"*errors.errorString","message":"clone repository:\nCould not resolve host github.com","traceback":"secret stack details"}`),
	})
	if view.LastError != "clone repository: Could not resolve host github.com" {
		t.Fatalf("last error = %q", view.LastError)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"traceback", "secret stack details", `"failure"`} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("public task projection exposed %q: %s", hidden, encoded)
		}
	}

	long := boundedTaskError(json.RawMessage(`{"message":"` + strings.Repeat("x", 400) + `"}`))
	if got := len([]rune(long)); got != 320 || !strings.HasSuffix(long, "…") {
		t.Fatalf("bounded error has %d runes", got)
	}
}

func TestRenderWorkflowExecutionAttentionLeadsToTruthfulRepair(t *testing.T) {
	job := spine.Job{ID: "job-123", AdmissionOpen: true}
	execution := taskResultView{TaskID: "task-1", State: absurd.TaskFailed, LastError: "clone repository: DNS failed"}
	var output strings.Builder
	renderWorkflowExecutionAttention(&output, job, execution, "Clone repository")
	want := "  attention: workflow stopped\n" +
		"  operation: Clone repository\n" +
		"  reason: clone repository: DNS failed\n" +
		"  next: repair the cause, then run dorf retry job-123\n"
	if output.String() != want {
		t.Fatalf("attention output:\n%s\nwant:\n%s", output.String(), want)
	}

	output.Reset()
	renderWorkflowExecutionAttention(&output, spine.Job{ID: "job-123"}, execution, "Complete")
	if output.Len() != 0 {
		t.Fatalf("closed Job rendered non-actionable attention: %q", output.String())
	}

	output.Reset()
	renderWorkflowExecutionAttention(&output, job, taskResultView{State: absurd.TaskRunning}, "Clone repository")
	if output.Len() != 0 {
		t.Fatalf("running task rendered failure attention: %q", output.String())
	}
}
