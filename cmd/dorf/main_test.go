package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
)

func TestAbandonGrammarExposesOneExactBoundary(t *testing.T) {
	for _, args := range [][]string{nil, {"job", "extra"}} {
		err := abandon(context.Background(), postgres.Store{}, nil, githubapi.Client{}, args, io.Discard)
		if err == nil || err.Error() != "abandon requires one Job ID" {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestWorkflowHistorySortsNaturalFactsAndIncludesRunsAndRevisions(t *testing.T) {
	base := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	entries := workflowHistory(
		spine.Job{AdmittedAt: base},
		[]spine.MessageView{{Message: spine.Message{Sequence: 1, FromKind: spine.MessageFromHuman, AdmittedAt: base.Add(time.Second)}}},
		[]spine.Action{
			{Kind: spine.ActionSandboxCreate, State: spine.ActionSucceeded, CreatedAt: base.Add(2 * time.Second), SettledAt: base.Add(3 * time.Second)},
			{Kind: spine.ActionGitHubPullRequest, State: spine.ActionSucceeded, Scope: "revision-1", CreatedAt: base.Add(7 * time.Second), SettledAt: base.Add(8 * time.Second)},
		},
		[]spine.AgentRun{{ID: "run-1", MessageID: "message-1", Role: "implement", State: spine.AgentRunCompleted, InputRevision: "revision-0", StartedAt: base.Add(4 * time.Second), FinishedAt: base.Add(5 * time.Second)}},
		[]spine.Revision{
			{Generation: 0, OID: "revision-0", ObservedAt: base},
			{Generation: 1, OID: "revision-1", ComparisonBase: "revision-0", ObservedAt: base.Add(6 * time.Second)},
		},
		nil, nil, nil, &spine.GitHubProposal{Number: 42, ProposedRevision: "revision-1"}, nil,
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
	for _, want := range []string{"Revision starting Revision revision-0 accepted", "AgentRun run-1 started Role=implement", "AgentRun run-1 completed", "Revision generation 1 observed Revision revision-1 from revision-0", "Proposal #42 recorded for Revision revision-1"} {
		if !strings.Contains(story.String(), want) {
			t.Fatalf("history is missing %q:\n%s", want, story.String())
		}
	}
}
