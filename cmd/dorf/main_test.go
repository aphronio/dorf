package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestProfileUpdateIsTheOnlyDefinitionMutationCommand(t *testing.T) {
	var stdout, stderr strings.Builder
	if err := profileCommand(context.Background(), postgres.Store{}, config.Config{}, []string{"replace"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), `unsupported profile command "replace"`) {
		t.Fatalf("legacy mutation command error=%v", err)
	}
	if err := profileCommand(context.Background(), postgres.Store{}, config.Config{}, []string{"update"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "profile update requires NAME") {
		t.Fatalf("update command error=%v", err)
	}
}

func TestSetupAutomationApprovalAndSelectionsAreExplicit(t *testing.T) {
	var stderr strings.Builder
	options, err := parseSetupOptions([]string{"--yes", "--provider", "personal-chatgpt", "--profile", "local-codex"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !options.Yes || options.Connection != "personal-chatgpt" || options.ProfileName != "local-codex" {
		t.Fatalf("options=%#v", options)
	}
	if _, err := parseSetupOptions([]string{"--database", "native"}, &stderr); err == nil {
		t.Fatal("removed database selection was accepted")
	}
}

func TestProfileInstallValidatesIdentityBeforeArtifactMutation(t *testing.T) {
	for _, args := range [][]string{
		{"Invalid_Name", "--release", "v1.2.3", "--harness", "codex"},
		{"local", "--release", "v1.2.3", "--harness", "unknown"},
	} {
		var stdout, stderr strings.Builder
		err := installOfficialIncusProfile(context.Background(), postgres.Store{}, args, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "Sandbox profile") {
			t.Fatalf("args=%v error=%v", args, err)
		}
	}
}

func TestSandboxForProfileSelectsOneConcreteAdapter(t *testing.T) {
	local, err := sandboxForProfile(config.Config{Workspace: "/workspace/job"}, spine.SandboxProfile{
		Name: "local", Provider: spine.SandboxProviderIncus, Artifact: strings.Repeat("a", 64),
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := local.(incus.Adapter); !ok {
		t.Fatalf("local adapter = %T", local)
	}
	managedProfile := spine.SandboxProfile{
		Name: "managed", Provider: spine.SandboxProviderE2B, Artifact: "dorf:exact-build",
		E2BGatewayURL: "https://gateway.example/v1", E2BSandboxTimeout: 55 * time.Minute,
	}
	managed, err := sandboxForProfile(config.Config{E2BAPIKey: "test-key", Workspace: "/workspace/job", TurnTimeout: 45 * time.Minute}, managedProfile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := managed.(e2b.Adapter); !ok {
		t.Fatalf("managed adapter = %T", managed)
	}
	if _, err := sandboxForProfile(config.Config{Workspace: "/workspace/job"}, managedProfile); err == nil || !strings.Contains(err.Error(), "E2B_API_KEY") {
		t.Fatalf("missing E2B API key error = %v", err)
	}
	managedProfile.E2BGatewayURL = "http://gateway.example/v1"
	if _, err := sandboxForProfile(config.Config{E2BAPIKey: "test-key", Workspace: "/workspace/job"}, managedProfile); err == nil {
		t.Fatal("invalid remote Gateway URL was admitted")
	}
}

func TestWorkflowHistorySortsNaturalFactsAndIncludesRunsAndRevisions(t *testing.T) {
	base := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	job := spine.Job{ID: "job-1", AdmittedAt: base, SandboxProfile: "e2b"}
	mainSandbox := spine.MainSandboxName(job.ID)
	entries := workflowHistory(workflow.Snapshot{
		Job: job,
		Deliveries: []spine.Delivery{{
			Message:  spine.Message{ID: "message-1", Sequence: 1, FromKind: spine.MessageFromHuman, AdmittedAt: base.Add(time.Second)},
			AgentRun: spine.AgentRun{ID: "run-secret", MessageID: "message-1", Role: "implement", State: spine.AgentRunCompleted, InputRevision: "revision-0", StartedAt: base.Add(4 * time.Second), FinishedAt: base.Add(5 * time.Second)},
		}},
		Actions: []spine.Action{
			{ID: "action-secret", Kind: spine.ActionSandboxCreate, Scope: mainSandbox, State: spine.ActionSucceeded, CreatedAt: base.Add(2 * time.Second), SettledAt: base.Add(3 * time.Second)},
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
	if len(entries) != 12 {
		t.Fatalf("history has %d entries, want 12 human events: %#v", len(entries), entries)
	}
	var story strings.Builder
	for _, entry := range entries {
		story.WriteString(entry.Text + "\n")
	}
	for _, want := range []string{
		"Job admitted",
		"Starting Revision accepted · revision-0",
		"Message 1 received from Human",
		"Creating primary Sandbox · E2B",
		"Primary Sandbox ready · E2B · 1s",
		"Implementation agent started",
		"Implementation agent completed · 1s",
		"Revision generation 1 observed · revision-1",
		"Git revision evidence recorded · Revision revision-1",
		"Creating pull request",
		"Pull request created · 1s",
		"Pull request #42 ready · Revision revision-1",
	} {
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
	if !strings.Contains(last.Text, "Outcome Abandoned") || strings.Contains(last.Text, "GitHub") {
		t.Fatalf("pre-Proposal abandonment history = %#v", last)
	}
}

func TestRenderHistoryGroupsLocalDatesAndHidesFactCategories(t *testing.T) {
	first := time.Date(2026, 8, 18, 15, 31, 57, 0, time.Local)
	entries := []historyEntry{
		{At: first, Text: "Job admitted"},
		{At: first.Add(time.Minute), Text: "Creating primary Sandbox · Incus"},
		{At: first.Add(24 * time.Hour), Text: "Cleanup complete"},
	}
	var output strings.Builder
	renderHistory(&output, entries)
	got := output.String()
	for _, want := range []string{
		"  18 Aug 2026\n    15:31  Job admitted",
		"    15:32  Creating primary Sandbox · Incus",
		"  19 Aug 2026\n    15:31  Cleanup complete",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("human history is missing %q:\n%s", want, got)
		}
	}
	for _, machineCopy := range []string{"2026-08-18T", "Action       ", "AgentRun     "} {
		if strings.Contains(got, machineCopy) {
			t.Fatalf("human history exposed machine copy %q:\n%s", machineCopy, got)
		}
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
