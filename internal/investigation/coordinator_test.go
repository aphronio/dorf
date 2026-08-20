package investigation

import (
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/profile"
)

func TestCodebaseInvestigationProjectsItsOwnDependencyChain(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := core.Job{ID: "job-1", Workflow: Workflow, WorkflowRevision: WorkflowRevision, AdmissionOpen: true}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	run := core.AgentRun{ID: "run-1", JobID: job.ID, Role: "investigate", State: core.AgentRunPending, SandboxID: sandbox.ID}
	delivery := core.Delivery{Message: core.Message{Sequence: 1}, AgentRun: run}
	snapshot := Snapshot{Job: job, MainSandbox: sandbox, Deliveries: []core.Delivery{delivery}, Delivery: delivery, Source: Source{JobID: job.ID, Kind: SourceRemote, Repository: "https://example.test/repo.git", Revision: revision}}

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
	if work := snapshot.Project(); work.Kind != WorkDeliver || work.FactID != run.ID {
		t.Fatalf("delivery work=%#v", work)
	}
	snapshot.Delivery.AgentRun.State = core.AgentRunActive
	if work := snapshot.Project(); work.Kind != WorkObserveAgent {
		t.Fatalf("observation work=%#v", work)
	}
	snapshot.Delivery.AgentRun.State = core.AgentRunCompleted
	if work := snapshot.Project(); work.Kind != WorkRecordDraft {
		t.Fatalf("draft work=%#v", work)
	}
	draft := Draft{JobID: job.ID, AgentRunID: run.ID, ArtifactID: "artifact-draft"}
	snapshot.Drafts = []Draft{draft}
	if work := snapshot.Project(); work.Kind != WorkWaitInput || work.FactID != draft.ArtifactID || !strings.Contains(work.Detail, "follow-up") || !strings.Contains(work.Detail, "cleanup") {
		t.Fatalf("waiting work=%#v", work)
	}
	snapshot.Job.AdmissionOpen = false
	if work := snapshot.Project(); work.Kind != WorkComplete {
		t.Fatalf("closed-admission work=%#v", work)
	}
}

func TestCodebaseInvestigationProjectsRetainedBundleRestore(t *testing.T) {
	revision := strings.Repeat("b", 40)
	job := core.Job{ID: "job-local", Workflow: Workflow, WorkflowRevision: WorkflowRevision, AdmissionOpen: true}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	snapshot := Snapshot{
		Job: job, MainSandbox: sandbox,
		Source:   Source{JobID: job.ID, Kind: SourceGitBundle, Revision: revision, BundleDigest: strings.Repeat("c", 64), BundleByteSize: 42},
		Delivery: core.Delivery{AgentRun: core.AgentRun{ID: "run-local", JobID: job.ID, Role: "investigate", State: core.AgentRunPending, SandboxID: sandbox.ID}},
		Actions:  []core.Action{{Kind: core.ActionSandboxCreate, Scope: sandbox.ID, State: core.ActionSucceeded}},
	}
	work := snapshot.Project()
	if work.Kind != WorkAction || work.ActionKind != ActionRepositoryRestore || work.Description() != "Restoring retained repository" {
		t.Fatalf("work=%#v description=%q", work, work.Description())
	}
}

func TestInvestigationReportKeepsFlexibleMarkdown(t *testing.T) {
	tests := []string{
		"# Finding\n\nSee `internal/workflow/workflow.go:54`.\n",
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
	input := investigationAgentInput(
		Source{Revision: revision},
		core.Delivery{Message: core.Message{Input: "Find one architectural weakness."}, AgentRun: core.AgentRun{Role: "investigate"}},
	)
	for _, required := range []string{
		"current working directory is its root", "repository-relative paths with 1-based line numbers",
		"<path>:<line> or <path>:<start>-<end>", "Do not include absolute Sandbox paths", revision,
	} {
		if !strings.Contains(input, required) {
			t.Fatalf("investigation input lacks %q:\n%s", required, input)
		}
	}
	if strings.Contains(input, "/workspace/job") || strings.Contains(input, "internal/workflow") {
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
