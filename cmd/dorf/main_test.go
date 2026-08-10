package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/spine"
)

func TestWorkflowHistorySortsNaturalFactsAndIncludesRunsAndRevisions(t *testing.T) {
	base := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	entries := workflowHistory(
		spine.Job{AdmittedAt: base},
		[]spine.MessageView{{Message: spine.Message{ID: "message-1", Sequence: 1, FromKind: spine.MessageFromHuman, AdmittedAt: base.Add(time.Second)}}},
		[]spine.Action{
			{ID: "action-secret", Kind: spine.ActionSandboxCreate, State: spine.ActionSucceeded, CreatedAt: base.Add(2 * time.Second), SettledAt: base.Add(3 * time.Second)},
			{Kind: spine.ActionGitHubPullRequest, State: spine.ActionSucceeded, Scope: "revision-1", CreatedAt: base.Add(7 * time.Second), SettledAt: base.Add(8 * time.Second)},
		},
		[]spine.AgentRun{{ID: "run-secret", MessageID: "message-1", Role: "implement", State: spine.AgentRunCompleted, InputRevision: "revision-0", StartedAt: base.Add(4 * time.Second), FinishedAt: base.Add(5 * time.Second)}},
		[]spine.Revision{
			{Generation: 0, OID: "revision-0", ObservedAt: base},
			{Generation: 1, OID: "revision-1", ComparisonBase: "revision-0", ObservedAt: base.Add(6 * time.Second)},
		},
		nil, nil, []spine.Evidence{{ID: "evidence-secret", Kind: "git-revision", Revision: "revision-1", FinishedAt: base.Add(6500 * time.Millisecond)}}, &spine.GitHubProposal{Number: 42, ProposedRevision: "revision-1"}, nil,
	)
	for i := 1; i < len(entries); i++ {
		if entries[i].At.Before(entries[i-1].At) {
			t.Fatalf("history is not chronological: %#v", entries)
		}
	}
	var story strings.Builder
	for _, entry := range entries {
		story.WriteString(entry.Kind + " " + entry.Detail + "\n")
	}
	for _, want := range []string{"Revision starting Revision revision-0 accepted", "AgentRun implementation started for Message 1 at Revision revision-0", "AgentRun implementation completed for Message 1 at Revision revision-0", "Revision generation 1 observed Revision revision-1 from revision-0", "Proposal #42 recorded for Revision revision-1"} {
		if !strings.Contains(story.String(), want) {
			t.Fatalf("history is missing %q:\n%s", want, story.String())
		}
	}
	for _, plumbing := range []string{"action-secret", "run-secret", "message-1", "evidence-secret"} {
		if strings.Contains(story.String(), plumbing) {
			t.Fatalf("human history leaked plumbing %q:\n%s", plumbing, story.String())
		}
	}
}
