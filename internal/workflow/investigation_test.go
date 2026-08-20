package workflow

import (
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/repository"
	"github.com/aphronio/dorf/internal/spine"
)

func TestCodebaseInvestigationProjectsItsOwnDependencyChain(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := spine.Job{ID: "job-1", Workflow: spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision, AdmissionOpen: true}
	sandbox := spine.Sandbox{ID: spine.MainSandboxName(job.ID), JobID: job.ID}
	run := spine.AgentRun{ID: "run-1", JobID: job.ID, Role: "investigate", State: spine.AgentRunPending, SandboxID: sandbox.ID}
	delivery := spine.Delivery{Message: spine.Message{Sequence: 1}, AgentRun: run}
	snapshot := InvestigationSnapshot{Job: job, MainSandbox: sandbox, Deliveries: []spine.Delivery{delivery}, Delivery: delivery, Source: investigation.Source{JobID: job.ID, Kind: investigation.SourceRemote, Repository: "https://example.test/repo.git", Revision: revision}}

	steps := []spine.ActionKind{spine.ActionSandboxCreate, repository.ActionRepositoryClone, spine.ActionRouteCreate}
	for _, want := range steps {
		work := snapshot.Project()
		if work.Kind != InvestigationWorkAction || work.ActionKind != want || work.Scope != sandbox.ID {
			t.Fatalf("work=%#v want Action %s", work, want)
		}
		if want == repository.ActionRepositoryClone && work.Description() != "Cloning repository" {
			t.Fatalf("repository clone description=%q", work.Description())
		}
		snapshot.Actions = append(snapshot.Actions, spine.Action{Kind: want, Scope: sandbox.ID, State: spine.ActionSucceeded})
	}
	if work := snapshot.Project(); work.Kind != InvestigationWorkDeliver || work.FactID != run.ID {
		t.Fatalf("delivery work=%#v", work)
	}
	snapshot.Delivery.AgentRun.State = spine.AgentRunActive
	if work := snapshot.Project(); work.Kind != InvestigationWorkObserveAgent {
		t.Fatalf("observation work=%#v", work)
	}
	snapshot.Delivery.AgentRun.State = spine.AgentRunCompleted
	if work := snapshot.Project(); work.Kind != InvestigationWorkRecordDraft {
		t.Fatalf("draft work=%#v", work)
	}
	draft := investigation.Draft{JobID: job.ID, AgentRunID: run.ID, ArtifactID: "artifact-draft"}
	snapshot.Drafts = []investigation.Draft{draft}
	if work := snapshot.Project(); work.Kind != InvestigationWorkWaitInput || work.FactID != draft.ArtifactID || !strings.Contains(work.Detail, "follow-up") || !strings.Contains(work.Detail, "cleanup") {
		t.Fatalf("waiting work=%#v", work)
	}
	snapshot.Job.AdmissionOpen = false
	if work := snapshot.Project(); work.Kind != InvestigationWorkComplete {
		t.Fatalf("closed-admission work=%#v", work)
	}
}

func TestCodebaseInvestigationProjectsRetainedBundleRestore(t *testing.T) {
	revision := strings.Repeat("b", 40)
	job := spine.Job{ID: "job-local", Workflow: spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision, AdmissionOpen: true}
	sandbox := spine.Sandbox{ID: spine.MainSandboxName(job.ID), JobID: job.ID}
	snapshot := InvestigationSnapshot{
		Job: job, MainSandbox: sandbox,
		Source:   investigation.Source{JobID: job.ID, Kind: investigation.SourceGitBundle, Revision: revision, BundleDigest: strings.Repeat("c", 64), BundleByteSize: 42},
		Delivery: spine.Delivery{AgentRun: spine.AgentRun{ID: "run-local", JobID: job.ID, Role: "investigate", State: spine.AgentRunPending, SandboxID: sandbox.ID}},
		Actions:  []spine.Action{{Kind: spine.ActionSandboxCreate, Scope: sandbox.ID, State: spine.ActionSucceeded}},
	}
	work := snapshot.Project()
	if work.Kind != InvestigationWorkAction || work.ActionKind != investigation.ActionRepositoryRestore || work.Description() != "Restoring retained repository" {
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
		investigation.Source{Revision: revision},
		spine.Delivery{Message: spine.Message{Input: "Find one architectural weakness."}, AgentRun: spine.AgentRun{Role: "investigate"}},
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
	browser := ProviderCapability("browser-workload")
	definition := Definition{Name: "browser-verification", Revision: "1", RequiredProviderCapabilities: []ProviderCapability{browser}}
	err := (RuntimeProfile{SandboxProfile: "incus"}).Require(definition)
	if err == nil || !strings.Contains(err.Error(), string(browser)) {
		t.Fatalf("missing provider capability error=%v", err)
	}
	if definition := CodebaseInvestigationDefinition(); len(definition.RequiredProviderCapabilities) != 0 {
		t.Fatalf("investigation unexpectedly requires provider capabilities: %v", definition.RequiredProviderCapabilities)
	}
	if err := (RuntimeProfile{SandboxProfile: "e2b"}).Require(CodebaseInvestigationDefinition()); err != nil {
		t.Fatal(err)
	}
}

func TestPresentationUsesOptionalCopyWithReadableFallbacks(t *testing.T) {
	definition := Definition{Presentation: Presentation{
		Operations: map[string]string{"observe": "Watching carefully"},
		AgentRoles: map[string]string{"investigate": "Researcher"},
		Results:    map[string]string{"report": "Research brief"},
	}}
	if got := definition.OperationLabel("observe", "Observe"); got != "Watching carefully" {
		t.Fatalf("operation label=%q", got)
	}
	if got := definition.OperationLabel("custom-operation", "Custom operation"); got != "Custom operation" {
		t.Fatalf("operation fallback=%q", got)
	}
	if got := definition.AgentRoleLabel("investigate"); got != "Researcher" {
		t.Fatalf("agent role label=%q", got)
	}
	if got := definition.AgentRoleLabel("security-review"); got != "Security review" {
		t.Fatalf("agent role fallback=%q", got)
	}
	if got := definition.ResultLabel("report"); got != "Research brief" {
		t.Fatalf("result label=%q", got)
	}
	if got := definition.ResultLabel("architecture-note"); got != "Architecture note" {
		t.Fatalf("result fallback=%q", got)
	}
}
