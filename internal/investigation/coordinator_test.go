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
	message := core.AgentMessageWork{MessageID: "message-1", SandboxID: sandbox.ID}
	snapshot := Snapshot{Job: job, MainSandbox: sandbox, Messages: []core.AgentMessageWork{message}, Message: message, Source: Source{JobID: job.ID, Kind: SourceRemote, Repository: "https://example.test/repo.git", Revision: revision}}

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
	if work := snapshot.Project(); work.Kind != WorkAgentMessage || work.FactID != message.MessageID || work.Scope != sandbox.ID {
		t.Fatalf("Agent Message work=%#v", work)
	}
	draft := Draft{JobID: job.ID, MessageID: message.MessageID, ArtifactID: "artifact-draft"}
	snapshot.Drafts = []Draft{draft}
	if work := snapshot.Project(); work.Kind != WorkWaitInput || work.FactID != draft.ArtifactID || !strings.Contains(work.Detail, "follow-up") || !strings.Contains(work.Detail, "cleanup") {
		t.Fatalf("waiting work=%#v", work)
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
	options := wakeOptions(Work{Kind: WorkAgentMessage, FactID: "message-1"}, 2)
	if options.StepName != "dorf/investigation-agent-wake/v2/message-1/00000000000000000002" || options.Timeout != time.Second {
		t.Fatalf("active investigator wake=%#v", options)
	}
	options = wakeOptions(Work{Kind: WorkWaitInput}, 3)
	if options.StepName != "dorf/investigation-wake/v2/00000000000000000003" || options.Timeout != idleMessagePollInterval {
		t.Fatalf("idle Message wake=%#v", options)
	}
}

func TestCodebaseInvestigationProjectsRetainedBundleRestore(t *testing.T) {
	revision := strings.Repeat("b", 40)
	job := core.Job{ID: "job-local", Workflow: Workflow, WorkflowRevision: WorkflowRevision, AdmissionOpen: true}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	snapshot := Snapshot{
		Job: job, MainSandbox: sandbox,
		Source:   Source{JobID: job.ID, Kind: SourceGitBundle, Revision: revision, BundleDigest: strings.Repeat("c", 64), BundleByteSize: 42},
		Messages: []core.AgentMessageWork{{MessageID: "message-local", SandboxID: sandbox.ID}},
		Message:  core.AgentMessageWork{MessageID: "message-local", SandboxID: sandbox.ID},
		Actions:  []core.Action{{Kind: core.ActionSandboxCreate, Scope: sandbox.ID, State: core.ActionSucceeded}},
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
		Source:   Source{JobID: job.ID, Kind: SourceRemote, Repository: "https://example.test/repo.git", Revision: strings.Repeat("a", 40)},
		Messages: []core.AgentMessageWork{{MessageID: "message-attention", SandboxID: sandbox.ID}},
		Message:  core.AgentMessageWork{MessageID: "message-attention", SandboxID: sandbox.ID},
	}
	work := snapshot.Project()
	if work.Kind != WorkAttention || work.FactID != source || work.Detail != job.WorkflowAttention {
		t.Fatalf("work = %#v", work)
	}
}

func TestInvestigationReportKeepsFlexibleMarkdown(t *testing.T) {
	tests := []string{
		"# Finding\n\nSee `internal/investigation/coordinator.go:54`.\n",
		"No material issue was found.\n",
	}
	for _, test := range tests {
		report, err := validateInvestigationReport(test)
		if err != nil || report != test {
			t.Fatalf("report=%q err=%v", report, err)
		}
	}
	for _, invalid := range []string{"", " \n\t", strings.Repeat("x", (1<<20)+1)} {
		if _, err := validateInvestigationReport(invalid); err == nil {
			t.Fatalf("accepted invalid output %q", invalid)
		}
	}
}

func TestInvestigationAgentInputRequiresPortableRepositoryCitations(t *testing.T) {
	revision := strings.Repeat("a", 40)
	input := AgentPrompt(Source{Revision: revision}, "Find one architectural weakness.")
	for _, required := range []string{
		"current working directory is its root", "repository-relative paths with 1-based line numbers",
		"<path>:<line> or <path>:<start>-<end>", "Do not include absolute Sandbox paths", revision,
	} {
		if !strings.Contains(input, required) {
			t.Fatalf("investigation input lacks %q:\n%s", required, input)
		}
	}
	if strings.Contains(input, "/workspace/job") {
		t.Fatalf("investigation input contains a Dorf-specific path:\n%s", input)
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

func TestPresentationUsesOptionalCopyWithReadableFallbacks(t *testing.T) {
	definition := WorkflowDefinition()
	if got := definition.OperationLabel(string(WorkWaitInput), "Wait"); got != "Waiting for follow-up or cleanup" {
		t.Fatalf("operation label=%q", got)
	}
	if got := definition.OperationLabel("custom-operation", "Custom operation"); got != "Custom operation" {
		t.Fatalf("operation fallback=%q", got)
	}
	if got := definition.AgentRoleLabel("investigate"); got != "Investigator" {
		t.Fatalf("agent role label=%q", got)
	}
	if got := definition.AgentRoleLabel("security-review"); got != "Security review" {
		t.Fatalf("agent role fallback=%q", got)
	}
	if got := definition.ResultLabel("investigation-draft"); got != "Investigation draft" {
		t.Fatalf("result label=%q", got)
	}
	if got := definition.ResultLabel("architecture-note"); got != "Architecture note" {
		t.Fatalf("result fallback=%q", got)
	}
}
