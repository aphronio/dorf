package workflow

import (
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/spine"
)

func TestCodebaseInvestigationProjectsItsOwnDependencyChain(t *testing.T) {
	job := spine.Job{ID: "job-1", Workflow: spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision, AdmissionOpen: true, Revision: strings.Repeat("a", 40)}
	sandbox := spine.Sandbox{ID: spine.MainSandboxName(job.ID), JobID: job.ID}
	run := spine.AgentRun{ID: "run-1", JobID: job.ID, Role: "investigate", State: spine.AgentRunPending, SandboxID: sandbox.ID}
	snapshot := InvestigationSnapshot{Job: job, MainSandbox: sandbox, Delivery: spine.Delivery{AgentRun: run}}

	steps := []spine.ActionKind{spine.ActionSandboxCreate, spine.ActionRepositoryClone, spine.ActionRouteCreate}
	for _, want := range steps {
		work := snapshot.Project()
		if work.Kind != InvestigationWorkAction || work.ActionKind != want || work.Scope != sandbox.ID {
			t.Fatalf("work=%#v want Action %s", work, want)
		}
		if want == spine.ActionRepositoryClone && work.Description() != "Clone repository" {
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
	if work := snapshot.Project(); work.Kind != InvestigationWorkRecordReport {
		t.Fatalf("report work=%#v", work)
	}
	snapshot.Report = &spine.CodebaseInvestigationReport{JobID: job.ID}
	if work := snapshot.Project(); work.Kind != InvestigationWorkComplete {
		t.Fatalf("terminal work=%#v", work)
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
	if err := ConfiguredRuntimeProfile("e2b").Require(CodebaseInvestigationDefinition()); err != nil {
		t.Fatal(err)
	}
}
