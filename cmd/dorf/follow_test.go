package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestFollowRendererTailsFactsOperationsAndTruthfulTimers(t *testing.T) {
	now := time.Date(2026, 8, 18, 14, 25, 0, 0, time.UTC)
	job := core.Job{ID: "job-123", AdmissionOpen: true}
	job.SandboxProfile = "e2b"
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	snapshot := followSnapshot{
		Job:          job,
		Presentation: coding.WorkflowDefinition(),
		History:      []historyEntry{{At: now.Add(-11 * time.Minute), Text: "Job admitted"}},
		Operation:    "Implementation agent running",
		AgentRuns:    []core.AgentRun{{Role: "implement", State: core.AgentRunActive, StartedAt: now.Add(-5 * time.Minute)}},
		Sandboxes:    []core.Sandbox{sandbox},
		Actions: []core.Action{{
			Kind: core.ActionSandboxCreate, Scope: sandbox.ID, State: core.ActionSucceeded, SettledAt: now.Add(-10 * time.Minute),
		}},
	}
	var output bytes.Buffer
	renderer := newFollowRenderer(&output)
	renderer.Render(now, snapshot, true)
	for _, want := range []string{
		followHumanTimestamp(now.Add(-11*time.Minute)) + "  Job admitted",
		followTimestamp(now) + "  Current      Implementation agent running",
		followTimestamp(now) + "  Pulse        Implementation agent active 5m0s",
		followTimestamp(now) + "  Pulse        Sandbox primary · E2B · provisioned 10m0s",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("initial follow output is missing %q:\n%s", want, output.String())
		}
	}

	output.Reset()
	renderer.Render(now.Add(2*time.Second), snapshot, false)
	if output.Len() != 0 {
		t.Fatalf("unchanged snapshot emitted output: %q", output.String())
	}

	snapshot.History = append(snapshot.History, historyEntry{At: now.Add(time.Second), Text: "Implementation agent completed"})
	snapshot.Operation = "Inspect implementation checkout"
	snapshot.AgentRuns[0].State = core.AgentRunCompleted
	renderer.Render(now.Add(2*time.Second), snapshot, false)
	if got := output.String(); !strings.Contains(got, "Implementation agent completed") || !strings.Contains(got, "Current      Inspect implementation checkout") || strings.Contains(got, "Pulse") {
		t.Fatalf("changed follow output:\n%s", got)
	}
}

func TestInteractiveFollowLabelsClientDirectedContractTruthfully(t *testing.T) {
	var output bytes.Buffer
	renderer := newFollowRenderer(&output)
	renderer.renderInteractiveHeader(time.Now(), followSnapshot{Job: core.Job{ID: "job-direct"}})
	if got := output.String(); !strings.Contains(got, "Following Job job-direct · client-directed") || strings.Contains(got, "revision") {
		t.Fatalf("interactive direct header:\n%s", got)
	}
}

func followTimestamp(value time.Time) string {
	return value.In(time.Local).Format(time.RFC3339)
}

func followHumanTimestamp(value time.Time) string {
	return value.In(time.Local).Format("15:04")
}

func TestFollowRendererStopsOnActionableFailureWithoutExposingClosedHistoryAsAttention(t *testing.T) {
	now := time.Date(2026, 8, 18, 14, 25, 0, 0, time.UTC)
	snapshot := followSnapshot{
		Job: core.Job{
			ID: "job-123", AdmissionOpen: true,
			Workflow: coding.Workflow, WorkflowRevision: coding.WorkflowRevision,
		},
		Operation: "Clone repository",
		Execution: taskResultView{State: absurd.TaskFailed, LastError: "Could not resolve host: github.com"},
	}
	var output bytes.Buffer
	newFollowRenderer(&output).Render(now, snapshot, false)
	for _, want := range []string{
		"Current      Clone repository",
		followHumanTimestamp(now) + "  Workflow stopped",
		"reason: Could not resolve host: github.com",
		"next: repair the cause, then run dorf retry job-123",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("failure output is missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), followTimestamp(now)+"  Attention") {
		t.Fatalf("attention used machine timestamp/category formatting:\n%s", output.String())
	}
	if !snapshot.followTerminal() {
		t.Fatal("actionable failed execution did not stop the follower")
	}

	output.Reset()
	snapshot.Job.AdmissionOpen = false
	snapshot.Job.CleanupState = core.CleanupComplete
	newFollowRenderer(&output).Render(now, snapshot, false)
	if strings.Contains(output.String(), "Attention") || strings.Contains(output.String(), "dorf retry") {
		t.Fatalf("completed Job exposed historical execution as current attention:\n%s", output.String())
	}

	output.Reset()
	snapshot.Operation = "Complete"
	snapshot.OperationDetail = "admission closed"
	newFollowRenderer(&output).Render(now, snapshot, false)
	if strings.Contains(output.String(), "Operation") {
		t.Fatalf("completed cleanup rendered a fresh derived operation:\n%s", output.String())
	}
}

func TestFollowCompletedWorkflowAttentionOffersCleanupInsteadOfRetry(t *testing.T) {
	now := time.Date(2026, 8, 18, 14, 25, 0, 0, time.UTC)
	snapshot := followSnapshot{
		Job:       core.Job{ID: "job-123", AdmissionOpen: true, CleanupState: core.CleanupPending, WorkflowAttention: "E2B template is unavailable"},
		Execution: taskResultView{State: absurd.TaskCompleted}, NeedsAttention: true,
	}
	var output bytes.Buffer
	newFollowRenderer(&output).Render(now, snapshot, false)
	if got := output.String(); !strings.Contains(got, "Needs attention · E2B template is unavailable") || !strings.Contains(got, "next: run dorf cleanup job-123 to release resources") || strings.Contains(got, "dorf retry") {
		t.Fatalf("completed attention output:\n%s", got)
	}
}

func TestFollowDerivesCleanupProgressAndStopsOnlyOnFailedTask(t *testing.T) {
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	job := core.Job{ID: "job-cleanup", CleanupState: core.CleanupScheduled, CleanupAttention: "reconciling provider-route-revoke"}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	snapshot := followSnapshot{
		Job: job, Presentation: investigation.WorkflowDefinition(), Operation: "Complete",
		Sandboxes: []core.Sandbox{sandbox},
		Actions: []core.Action{{
			Kind: core.ActionRouteRevoke, Scope: sandbox.ID, State: core.ActionUnsettled,
		}},
		Execution: taskResultView{State: absurd.TaskRunning},
	}.withCleanupOperation()

	if snapshot.Operation != "Revoking model access" || snapshot.followTerminal() {
		t.Fatalf("active cleanup snapshot=%#v", snapshot)
	}
	var output bytes.Buffer
	newFollowRenderer(&output).Render(now, snapshot, false)
	if got := output.String(); !strings.Contains(got, "Current      Revoking model access") || strings.Contains(got, "needs attention") || strings.Contains(got, "dorf retry") {
		t.Fatalf("active cleanup output:\n%s", got)
	}

	snapshot.Actions[0].State = core.ActionSucceeded
	snapshot = snapshot.withCleanupOperation()
	if snapshot.Operation != "Deleting Sandbox" {
		t.Fatalf("post-revoke cleanup operation=%q", snapshot.Operation)
	}
	snapshot.Actions = append(snapshot.Actions, core.Action{Kind: core.ActionSandboxDelete, Scope: sandbox.ID, State: core.ActionSucceeded})
	snapshot = snapshot.withCleanupOperation()
	if snapshot.Operation != "Finalizing cleanup" {
		t.Fatalf("post-delete cleanup operation=%q", snapshot.Operation)
	}

	snapshot.Execution = taskResultView{State: absurd.TaskFailed, LastError: "delete Sandbox: provider unavailable"}
	if !snapshot.followTerminal() {
		t.Fatal("failed cleanup task did not stop the follower")
	}
	output.Reset()
	newFollowRenderer(&output).Render(now, snapshot, false)
	for _, want := range []string{"Cleanup stopped", "reason: delete Sandbox: provider unavailable", "dorf retry " + job.ID} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("failed cleanup output lacks %q:\n%s", want, output.String())
		}
	}
}

func TestInvestigationHistoryIsChronologicalAndIncludesTerminalDuration(t *testing.T) {
	base := time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)
	snapshot := investigation.Snapshot{
		Job: core.Job{AdmittedAt: base, CleanedAt: base.Add(10 * time.Minute)},
		Actions: []core.Action{{
			Kind: core.ActionSandboxCreate, State: core.ActionSucceeded,
			CreatedAt: base.Add(time.Minute), SettledAt: base.Add(2 * time.Minute),
		}},
	}
	deliveries := []core.Delivery{{AgentRun: core.AgentRun{
		Role: "investigate", State: core.AgentRunCompleted,
		StartedAt: base.Add(3 * time.Minute), FinishedAt: base.Add(8 * time.Minute),
	}}}
	history := investigationHistory(snapshot, deliveries)
	if len(history) != 6 {
		t.Fatalf("history entries=%d: %#v", len(history), history)
	}
	for i := 1; i < len(history); i++ {
		if history[i].At.Before(history[i-1].At) {
			t.Fatalf("history is not chronological: %#v", history)
		}
	}
	if got := history[4].Text; got != "Investigator completed · 5m0s" {
		t.Fatalf("terminal AgentRun detail=%q", got)
	}
}

func TestProvisionedSandboxTimeExcludesDeletedSandbox(t *testing.T) {
	job := core.Job{ID: "job-123"}
	main := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	reviewRun := core.AgentRun{ID: "review-run"}
	review := core.Sandbox{ID: coding.ReviewSandboxName(job.ID, reviewRun.ID), JobID: job.ID}
	reviewRun.SandboxID = review.ID
	now := time.Now()
	actions := []core.Action{
		{Kind: core.ActionSandboxCreate, Scope: main.ID, State: core.ActionSucceeded, SettledAt: now.Add(-time.Minute)},
		{Kind: core.ActionSandboxCreate, Scope: review.ID, State: core.ActionSucceeded, SettledAt: now.Add(-time.Minute)},
		{Kind: core.ActionSandboxDelete, Scope: review.ID, State: core.ActionSucceeded, SettledAt: now},
	}
	active := provisionedSandboxes(job, []core.AgentRun{reviewRun}, []core.Sandbox{review, main}, actions)
	if len(active) != 1 || active[0].Label != "primary" {
		t.Fatalf("provisioned Sandboxes=%#v", active)
	}
	if got := actionSettledText(job, []core.AgentRun{reviewRun}, actions[2], actions); got != "Reviewer Sandbox deleted · provisioned 1m0s" {
		t.Fatalf("Sandbox terminal duration=%q", got)
	}
}

func TestInteractiveFollowHeaderShowsLiveClocksWithoutAppendingPulse(t *testing.T) {
	now := time.Date(2026, 8, 18, 14, 25, 0, 0, time.UTC)
	job := core.Job{ID: "job-123", Workflow: investigation.Workflow, WorkflowRevision: investigation.WorkflowRevision, SandboxProfile: "local-codex", AdmittedAt: now.Add(-20 * time.Second)}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	snapshot := followSnapshot{
		Job: job, Profile: core.SandboxProfile{Name: "local-codex", Provider: core.SandboxProviderIncus}, Presentation: investigation.WorkflowDefinition(), Operation: "Investigator running",
		AgentRuns: []core.AgentRun{{Role: "investigate", State: core.AgentRunActive, StartedAt: now.Add(-15 * time.Second)}},
		Sandboxes: []core.Sandbox{sandbox},
		Actions:   []core.Action{{Kind: core.ActionSandboxCreate, Scope: sandbox.ID, State: core.ActionSucceeded, SettledAt: now.Add(-18 * time.Second)}},
	}
	var output bytes.Buffer
	renderer := newFollowRenderer(&output)
	renderer.interactive = true
	renderer.Start(snapshot)
	output.Reset()
	renderer.Render(now, snapshot, true)
	got := output.String()
	for _, want := range []string{
		"Current      Investigator running",
		"Job          elapsed · 20s",
		"AgentRun     Investigator · active 15s",
		"Sandbox      primary · local-codex · Incus · provisioned 18s",
		"History",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("interactive header is missing %q:\n%q", want, got)
		}
	}
	if strings.Contains(got, "Pulse") {
		t.Fatalf("interactive header appended a Pulse line:\n%q", got)
	}

	output.Reset()
	snapshot.Job.CleanupState = core.CleanupComplete
	snapshot.Job.CleanedAt = now.Add(5 * time.Second)
	snapshot.AgentRuns[0].State = core.AgentRunCompleted
	snapshot.Actions = append(snapshot.Actions, core.Action{Kind: core.ActionSandboxDelete, Scope: sandbox.ID, State: core.ActionSucceeded, SettledAt: snapshot.Job.CleanedAt})
	renderer.Render(snapshot.Job.CleanedAt, snapshot, true)
	got = output.String()
	for _, want := range []string{"Complete", "Job          total · 25s", "Cleanup      complete"} {
		if !strings.Contains(got, want) {
			t.Fatalf("completed header is missing %q:\n%q", want, got)
		}
	}
	if strings.Contains(got, "Live ·") || strings.Contains(got, "Current      Complete") || strings.Contains(got, "Following Job") || strings.Contains(got, "Ctrl-C stops following") {
		t.Fatalf("completed header still looks live:\n%q", got)
	}
}

func TestInspectRejectsJSONFollowCombinationBeforeDatabaseAccess(t *testing.T) {
	err := inspect(context.Background(), postgres.Store{}, nil, blob.Store{}, []string{"--json", "--follow", "job-123"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("inspect error=%v", err)
	}
}
