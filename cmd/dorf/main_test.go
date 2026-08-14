package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
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
