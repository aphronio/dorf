package workflow

import (
	"testing"

	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

func readyFacts() currentWorkFacts {
	job := spine.Job{ID: "job-1", Revision: "rev-1", AdmissionOpen: true}
	sandbox := spine.Sandbox{ID: spine.MainSandboxName(job.ID), JobID: job.ID}
	action := func(kind spine.ActionKind) spine.Action {
		return spine.Action{ID: spine.ScopedActionID(job.ID, kind, sandbox.ID), Kind: kind, State: spine.ActionSucceeded, Scope: sandbox.ID}
	}
	return currentWorkFacts{
		job:     job,
		sandbox: sandbox,
		actions: []spine.Action{
			action(spine.ActionSandboxCreate),
			action(spine.ActionRepositoryClone),
			action(spine.ActionRouteCreate),
		},
		setup:      &spine.Action{ID: spine.ActionID(job.ID, spine.ActionRepositorySetup), State: spine.ActionSucceeded},
		declared:   []spine.DeclaredCheck{{Name: "check", Command: "go test ./..."}},
		checks:     []spine.Check{{ID: spine.CheckID(job.ID, job.Revision, "check"), Name: "check", Revision: job.Revision, State: "passed", EvidenceID: "e-check"}},
		reviewPlan: &spine.ReviewPlanRecord{JobID: job.ID, Revision: job.Revision},
	}
}

func TestCurrentWorkDependencyOrder(t *testing.T) {
	t.Run("selected reviewer precedes implementation feedback", func(t *testing.T) {
		facts := readyFacts()
		facts.reviewPlan.Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		facts.reviewRuns = []spine.ReviewRunView{{AgentRun: spine.AgentRun{ID: "review-1", Role: string(policy.RoleGeneral), State: spine.AgentRunPending}}}
		facts.delivery = &spine.Delivery{Message: spine.Message{Sequence: 2}, AgentRun: spine.AgentRun{ID: "implement-2", State: spine.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkRunReviewer || got.FactID != "review-1" {
			t.Fatalf("CurrentWork = %#v, want selected reviewer", got)
		}
	})

	t.Run("message precedes Git observation and Checks", func(t *testing.T) {
		facts := readyFacts()
		facts.reviewPlan = nil
		facts.checks = nil
		facts.delivery = &spine.Delivery{Message: spine.Message{Sequence: 2}, AgentRun: spine.AgentRun{ID: "implement-2", State: spine.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkDeliverMessage || got.FactID != "implement-2" {
			t.Fatalf("CurrentWork = %#v, want Message delivery", got)
		}
	})

	t.Run("started publication settles before a later Message", func(t *testing.T) {
		facts := readyFacts()
		facts.actions = append(facts.actions,
			spine.Action{Kind: spine.ActionRepositoryPush, State: spine.ActionUnsettled, Scope: facts.job.Revision},
			spine.Action{Kind: spine.ActionGitHubPullRequest, State: spine.ActionUnsettled, Scope: facts.job.Revision},
		)
		facts.delivery = &spine.Delivery{Message: spine.Message{Sequence: 2}, AgentRun: spine.AgentRun{ID: "implement-2", State: spine.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkPublishProposal {
			t.Fatalf("CurrentWork = %#v, want started publication reconciliation", got)
		}
	})

	t.Run("ready exact Revision observes its Proposal", func(t *testing.T) {
		facts := readyFacts()
		facts.proposal = &spine.GitHubProposal{JobID: facts.job.ID, Number: 12, URL: "https://example.test/12", ProposedRevision: facts.job.Revision}
		if got := decideCurrentWork(facts); got.Kind != WorkObserveProposal {
			t.Fatalf("CurrentWork = %#v, want Proposal observation", got)
		}
	})
}

func TestUnchangedObservationMeaningComesFromItsMessages(t *testing.T) {
	tests := []struct {
		name      string
		from      spine.MessageFromKind
		proposal  bool
		attention bool
	}{
		{name: "initial human request", from: spine.MessageFromHuman, attention: true},
		{name: "review feedback", from: spine.MessageFromAgent},
		{name: "current Proposal feedback", from: spine.MessageFromHuman, proposal: true},
		{name: "failed Check feedback", from: spine.MessageFromWorkflow, attention: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := readyFacts()
			message := spine.Message{ID: "message-1", JobID: facts.job.ID, Sequence: 1, FromKind: test.from}
			run := spine.AgentRun{ID: "run-1", JobID: facts.job.ID, MessageID: message.ID, Role: "implement", State: spine.AgentRunCompleted, InputRevision: facts.job.Revision}
			facts.messages = []spine.MessageView{{Message: message}}
			facts.runs = []spine.AgentRun{run}
			facts.evidence = []spine.Evidence{{ID: "e-git", Kind: "git-revision", AgentRunID: run.ID, Revision: facts.job.Revision}}
			if test.proposal {
				facts.proposal = &spine.GitHubProposal{JobID: facts.job.ID, Number: 12, URL: "https://example.test/12", ProposedRevision: facts.job.Revision}
			}
			id, _ := unchangedAttention(facts)
			if (id != "") != test.attention {
				t.Fatalf("unchangedAttention ID = %q, want attention=%t", id, test.attention)
			}
		})
	}
}

func TestRevisionCandidateIsLatestBatchBoundary(t *testing.T) {
	facts := readyFacts()
	facts.messages = []spine.MessageView{
		{Message: spine.Message{ID: "message-1", Sequence: 1, Intent: spine.MessageFollow}},
		{Message: spine.Message{ID: "message-2", Sequence: 2, Intent: spine.MessageFollow}},
	}
	facts.runs = []spine.AgentRun{
		{ID: "run-1", MessageID: "message-1", Role: "implement", State: spine.AgentRunCompleted},
		{ID: "run-2", MessageID: "message-2", Role: "implement", State: spine.AgentRunCompleted},
	}
	if got := revisionCandidate(facts); got == nil || got.ID != "run-2" {
		t.Fatalf("revisionCandidate = %#v, want latest batch AgentRun", got)
	}
	facts.evidence = []spine.Evidence{{Kind: "git-revision", AgentRunID: "run-2"}}
	if got := revisionCandidate(facts); got != nil {
		t.Fatalf("revisionCandidate = %#v after latest observation; earlier batch member must not be observed again", got)
	}
}

func TestLaterSuccessfulFollowRecoversEarlierFailure(t *testing.T) {
	facts := readyFacts()
	facts.checks = nil
	facts.reviewPlan = nil
	facts.messages = []spine.MessageView{
		{Message: spine.Message{ID: "message-1", Sequence: 1, Intent: spine.MessageFollow}},
		{Message: spine.Message{ID: "message-2", Sequence: 2, Intent: spine.MessageFollow}},
	}
	facts.runs = []spine.AgentRun{
		{ID: "run-1", MessageID: "message-1", Role: "implement", State: spine.AgentRunFailed},
		{ID: "run-2", MessageID: "message-2", Role: "implement", State: spine.AgentRunCompleted, InputRevision: facts.job.Revision},
	}
	if got := decideCurrentWork(facts); got.Kind != WorkObserveRevision || got.FactID != "run-2" {
		t.Fatalf("CurrentWork = %#v, want successful recovery Follow observation", got)
	}
	facts.runs[1].State = spine.AgentRunFailed
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "run-2" {
		t.Fatalf("CurrentWork = %#v, want latest failed Follow attention", got)
	}
}
