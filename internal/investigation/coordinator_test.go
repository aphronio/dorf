package investigation

import (
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/profile"
)

func TestCodebaseInvestigationProjectsItsOwnDependencyChain(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := core.Job{ID: "job-1", Workflow: Workflow, WorkflowRevision: WorkflowRevision, AdmissionOpen: true}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	message := MessageRecord{MessageID: "message-1", SandboxID: sandbox.ID}
	snapshot := Snapshot{Job: job, MainSandbox: sandbox, Message: message, Source: Source{JobID: job.ID, Kind: SourceRemote, Repository: "https://example.test/repo.git", Revision: revision}}

	steps := []core.ActionKind{core.ActionSandboxCreate, gitworkspace.ActionRepositoryClone, core.ActionRouteCreate}
	for _, want := range steps {
		work := snapshot.Project()
		if work.Kind != WorkAction || work.ActionKind != want || work.Scope != sandbox.ID {
			t.Fatalf("work=%#v want Action %s", work, want)
		}
		if want == gitworkspace.ActionRepositoryClone && work.Description() != "Cloning repository" {
			t.Fatalf("repository clone description=%q", work.Description())
		}
		snapshot.Actions = append(snapshot.Actions, core.Action{Kind: want, Scope: sandbox.ID, State: core.ActionSucceeded})
	}
	if work := snapshot.Project(); work.Kind != WorkWaitAgent || work.FactID != message.MessageID {
		t.Fatalf("Agent Message work=%#v", work)
	}
	snapshot.Message.Outcome = "completed"
	if work := snapshot.Project(); work.Kind != "" || work.Description() != "No current workflow operation" {
		t.Fatalf("open Job with completed investigator projected workflow work=%#v", work)
	}
	snapshot.Message = MessageRecord{}
	if work := snapshot.Project(); work.Kind != "" || work.Description() != "No current workflow operation" {
		t.Fatalf("open Job with no unsettled Message projected workflow work=%#v", work)
	}
	snapshot.Job.AdmissionOpen = false
	if work := snapshot.Project(); work.Kind != WorkComplete {
		t.Fatalf("closed-admission work=%#v", work)
	}
}

func TestTaskAndWakeIdentitiesRemainStable(t *testing.T) {
	if TaskName != "dorf-codebase-investigation-v2" || TaskKey("job-1") != "codebase-investigation:v2:job-1" {
		t.Fatalf("task identity changed: name=%q key=%q", TaskName, TaskKey("job-1"))
	}
	stepName, timeout := wakeOptions(Work{Kind: WorkWaitAgent, FactID: "message-1"}, 2)
	if timeout != time.Second || stepName != "dorf/investigation-agent-wake/v2/message-1/00000000000000000002" {
		t.Fatalf("active investigator wake=%q %s", stepName, timeout)
	}
	stepName, timeout = wakeOptions(Work{}, 3)
	if timeout != 30*time.Second || stepName != "dorf/investigation-wake/v2/00000000000000000003" {
		t.Fatalf("open-idle Message wake=%q %s", stepName, timeout)
	}
}

func TestCodebaseInvestigationProjectsRetainedBundleRestore(t *testing.T) {
	revision := strings.Repeat("b", 40)
	job := core.Job{ID: "job-local", Workflow: Workflow, WorkflowRevision: WorkflowRevision, AdmissionOpen: true}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	snapshot := Snapshot{
		Job: job, MainSandbox: sandbox,
		Source:  Source{JobID: job.ID, Kind: SourceGitBundle, Revision: revision, BundleDigest: strings.Repeat("c", 64), BundleByteSize: 42},
		Message: MessageRecord{MessageID: "message-local", SandboxID: sandbox.ID},
		Actions: []core.Action{{Kind: core.ActionSandboxCreate, Scope: sandbox.ID, State: core.ActionSucceeded}},
	}
	work := snapshot.Project()
	if work.Kind != WorkAction || work.ActionKind != ActionRepositoryRestore || work.Description() != "Restoring retained repository" {
		t.Fatalf("work=%#v description=%q", work, work.Description())
	}
}

func TestCodebaseInvestigationSurfacesSandboxActionAttention(t *testing.T) {
	job := core.Job{ID: "job-attention", Workflow: Workflow, WorkflowRevision: WorkflowRevision, AdmissionOpen: true}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	source := core.ScopedActionID(job.ID, core.ActionSandboxCreate, sandbox.ID)
	job.WorkflowAttentionSource = source
	job.WorkflowAttention = "the exact Sandbox profile artifact is unavailable"
	snapshot := Snapshot{
		Job: job, MainSandbox: sandbox,
		Source:  Source{JobID: job.ID, Kind: SourceRemote, Repository: "https://example.test/repo.git", Revision: strings.Repeat("a", 40)},
		Message: MessageRecord{MessageID: "message-attention", SandboxID: sandbox.ID},
	}
	work := snapshot.Project()
	if work.Kind != WorkAttention || work.FactID != source || work.Detail != job.WorkflowAttention {
		t.Fatalf("work = %#v", work)
	}
}

func TestInvestigationAgentPromptOwnsReportPathAndPortableCitations(t *testing.T) {
	revision := strings.Repeat("a", 40)
	input := AgentPrompt(Source{Revision: revision}, "Find one architectural weakness.")
	for _, required := range []string{ReportPath, revision, "complete Markdown report", "Replace its contents", "repository-relative paths", "1-based line numbers"} {
		if !strings.Contains(input, required) {
			t.Fatalf("investigation input lacks %q:\n%s", required, input)
		}
	}
}

func TestProviderCapabilityAdmissionUsesOnlyOptionalProviderPrimitives(t *testing.T) {
	browser := profile.Capability("browser-workload")
	definition := Definition{Name: "browser-verification", Revision: "1", RequiredProviderCapabilities: []profile.Capability{browser}}
	err := (profile.Runtime{SandboxProfile: "incus"}).Require(definition.Name, definition.Revision, definition.RequiredProviderCapabilities)
	if err == nil || !strings.Contains(err.Error(), string(browser)) {
		t.Fatalf("missing provider capability error=%v", err)
	}
	if definition := WorkflowDefinition(); len(definition.RequiredProviderCapabilities) != 0 {
		t.Fatalf("investigation unexpectedly requires provider capabilities: %v", definition.RequiredProviderCapabilities)
	}
	investigationDefinition := WorkflowDefinition()
	if err := (profile.Runtime{SandboxProfile: "e2b"}).Require(investigationDefinition.Name, investigationDefinition.Revision, investigationDefinition.RequiredProviderCapabilities); err != nil {
		t.Fatal(err)
	}
}
