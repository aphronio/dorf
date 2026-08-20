package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	policy "github.com/aphronio/dorf/internal/review"
)

func readyFacts() Snapshot {
	job := coding.Job{Job: core.Job{ID: "job-1", AdmissionOpen: true}, Revision: "rev-1"}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID}
	action := func(kind core.ActionKind) core.Action {
		return core.Action{ID: core.ScopedActionID(job.ID, kind, sandbox.ID), Kind: kind, State: core.ActionSucceeded, Scope: sandbox.ID}
	}
	return Snapshot{
		Job:         job,
		MainSandbox: sandbox,
		Sandboxes:   []core.Sandbox{sandbox},
		Actions: []core.Action{
			action(core.ActionSandboxCreate),
			action(gitworkspace.ActionRepositoryClone),
			action(core.ActionRouteCreate),
		},
		ReviewPlans: []coding.ReviewPlanRecord{{JobID: job.ID, Revision: job.Revision}},
	}
}

func factDelivery(message core.Message, run core.AgentRun) core.Delivery {
	return core.Delivery{Message: message, AgentRun: run}
}

func TestCodingAgentInputKeepsReviewFeedbackOpaque(t *testing.T) {
	job := coding.Job{Branch: "dorf/feedback", Revision: strings.Repeat("a", 40)}
	message := core.Message{FromKind: core.MessageFromAgent, FromID: "review-run-1", Input: "Reviewer prose that the implementation agent must interpret."}
	reviewer := codingAgentInput(job, core.Delivery{Message: message, AgentRun: core.AgentRun{Role: "critical-boundary"}})
	if reviewer != message.Input {
		t.Fatalf("reviewer input was rewritten: %q", reviewer)
	}
	implementation := codingAgentInput(job, core.Delivery{Message: message, AgentRun: core.AgentRun{Role: "implement"}})
	if !strings.HasPrefix(implementation, message.Input+"\n\n") || !strings.Contains(implementation, job.Branch) || !strings.Contains(implementation, job.Revision) {
		t.Fatalf("implementation input is missing the coding contract: %q", implementation)
	}
}

func TestCurrentWorkDependencyOrder(t *testing.T) {
	t.Run("selected reviewer Actions precede its AgentRun and implementation feedback", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans[0].Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		reviewer := coding.ReviewRunView{AgentRun: core.AgentRun{ID: "review-1", JobID: facts.Job.ID, MessageID: "review-request-1", InputRevision: facts.Job.Revision, Role: string(policy.RoleGeneral), State: core.AgentRunPending, Capability: coding.ReviewReadOnlyCapability, SandboxID: coding.ReviewSandboxName("review-1")}}
		reviewer.Sandbox = core.Sandbox{ID: reviewer.SandboxID, JobID: facts.Job.ID}
		facts.Deliveries = []core.Delivery{factDelivery(core.Message{ID: reviewer.MessageID, JobID: facts.Job.ID}, reviewer.AgentRun)}
		facts.Sandboxes = append(facts.Sandboxes, reviewer.Sandbox)
		facts.Delivery = &core.Delivery{Message: core.Message{Sequence: 2}, AgentRun: core.AgentRun{ID: "implement-2", State: core.AgentRunPending}}
		steps := []struct {
			work   WorkKind
			action core.ActionKind
		}{
			{WorkAction, core.ActionSandboxCreate},
			{WorkAction, coding.ActionReviewCheckout},
			{WorkAction, core.ActionRouteCreate},
		}
		for _, step := range steps {
			got := decideCurrentWork(facts)
			wantID := core.ScopedActionID(facts.Job.ID, step.action, reviewer.Sandbox.ID)
			if got.Kind != step.work || got.ActionKind != step.action || got.FactID != wantID || got.Scope != reviewer.Sandbox.ID {
				t.Fatalf("CurrentWork = %#v, want %s Action %s", got, step.action, wantID)
			}
			facts.Actions = append(facts.Actions, core.Action{ID: wantID, JobID: facts.Job.ID, Kind: step.action, State: core.ActionSucceeded, Scope: reviewer.Sandbox.ID})
		}
		if got := decideCurrentWork(facts); got.Kind != WorkRunReviewer || got.FactID != reviewer.ID {
			t.Fatalf("CurrentWork = %#v, want selected reviewer after its exact Actions", got)
		}
	})

	t.Run("review feedback Message enters the ordinary implementation lane", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans[0].Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		requestID := coding.ReviewRequestMessageID(facts.Job.ID, facts.Job.Revision, string(policy.RoleGeneral))
		reviewerID := core.AgentRunID(requestID)
		reviewer := core.AgentRun{ID: reviewerID, JobID: facts.Job.ID, MessageID: requestID, InputRevision: facts.Job.Revision, Role: string(policy.RoleGeneral), State: core.AgentRunCompleted, Capability: coding.ReviewReadOnlyCapability, SandboxID: coding.ReviewSandboxName(reviewerID)}
		feedback := core.Message{ID: core.MessageID(facts.Job.ID, core.MessageFromAgent, reviewerID), JobID: facts.Job.ID, FromKind: core.MessageFromAgent, FromID: reviewerID, Sequence: 2, Intent: core.MessageFollow}
		facts.Deliveries = []core.Delivery{factDelivery(core.Message{ID: requestID, JobID: facts.Job.ID}, reviewer), factDelivery(feedback, core.AgentRun{MessageID: feedback.ID, Role: "implement"})}
		facts.Sandboxes = append(facts.Sandboxes, core.Sandbox{ID: reviewer.SandboxID, JobID: facts.Job.ID})
		facts.Delivery = &core.Delivery{Message: feedback, AgentRun: core.AgentRun{ID: core.AgentRunID(feedback.ID), State: core.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkDeliverMessage || got.FactID != facts.Delivery.AgentRun.ID {
			t.Fatalf("CurrentWork = %#v, want ordinary feedback Message delivery", got)
		}
	})

	t.Run("message precedes Git observation", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans = nil
		facts.Delivery = &core.Delivery{Message: core.Message{Sequence: 2}, AgentRun: core.AgentRun{ID: "implement-2", State: core.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkDeliverMessage || got.FactID != "implement-2" {
			t.Fatalf("CurrentWork = %#v, want Message delivery", got)
		}
	})

	t.Run("submitting Follow is reconciled before review", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans = nil
		message := core.Message{ID: "message-2", JobID: facts.Job.ID, Sequence: 2, Intent: core.MessageFollow}
		run := core.AgentRun{ID: core.AgentRunID(message.ID), JobID: facts.Job.ID, MessageID: message.ID, Role: "implement", State: core.AgentRunSubmitting}
		facts.Deliveries = []core.Delivery{factDelivery(message, run)}
		facts.Delivery = &core.Delivery{Message: message, AgentRun: run}
		if got := decideCurrentWork(facts); got.Kind != WorkDeliverMessage || got.FactID != run.ID {
			t.Fatalf("CurrentWork = %#v, want submitting Follow reconciliation", got)
		}
	})

	t.Run("started publication settles before a later Message", func(t *testing.T) {
		facts := readyFacts()
		facts.Actions = append(facts.Actions,
			core.Action{Kind: coding.ActionRepositoryPush, State: core.ActionUnsettled, Scope: facts.Job.Revision},
			core.Action{Kind: coding.ActionGitHubPullRequest, State: core.ActionUnsettled, Scope: facts.Job.Revision},
		)
		facts.Delivery = &core.Delivery{Message: core.Message{Sequence: 2}, AgentRun: core.AgentRun{ID: "implement-2", State: core.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkPublishProposal {
			t.Fatalf("CurrentWork = %#v, want started publication reconciliation", got)
		}
	})

	t.Run("ready exact Revision observes its Proposal", func(t *testing.T) {
		facts := readyFacts()
		facts.Proposal = &coding.Proposal{JobID: facts.Job.ID, Number: 12, URL: "https://example.test/12", ProposedRevision: facts.Job.Revision}
		if got := decideCurrentWork(facts); got.Kind != WorkObserveProposal {
			t.Fatalf("CurrentWork = %#v, want Proposal observation", got)
		}
	})
}

func TestDownstreamFactsWaitForCodingPrerequisites(t *testing.T) {
	facts := readyFacts()
	if !codingPrerequisitesComplete(facts) {
		t.Fatal("complete infrastructure was not ready for coding facts")
	}
	facts = readyFacts()
	facts.Actions = facts.Actions[1:]
	if codingPrerequisitesComplete(facts) {
		t.Fatal("missing Sandbox creation exposed downstream coding facts")
	}
}

func TestUnchangedObservationMeaningComesFromItsMessages(t *testing.T) {
	tests := []struct {
		name      string
		from      core.MessageFromKind
		proposal  bool
		attention bool
	}{
		{name: "initial human request", from: core.MessageFromHuman, attention: true},
		{name: "review feedback", from: core.MessageFromAgent},
		{name: "current Proposal feedback", from: core.MessageFromHuman, proposal: true},
		{name: "workflow feedback", from: core.MessageFromWorkflow, attention: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := readyFacts()
			message := core.Message{ID: "message-1", JobID: facts.Job.ID, Sequence: 1, FromKind: test.from}
			run := core.AgentRun{ID: "run-1", JobID: facts.Job.ID, MessageID: message.ID, Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision}
			facts.Deliveries = []core.Delivery{factDelivery(message, run)}
			facts.Evidence = []core.Evidence{{ID: "e-git", Kind: "git-revision", AgentRunID: run.ID, Revision: facts.Job.Revision}}
			if test.proposal {
				facts.Proposal = &coding.Proposal{JobID: facts.Job.ID, Number: 12, URL: "https://example.test/12", ProposedRevision: facts.Job.Revision}
			}
			id, _ := unchangedAttention(facts)
			if (id != "") != test.attention {
				t.Fatalf("unchangedAttention ID = %q, want attention=%t", id, test.attention)
			}
		})
	}
}

func TestUnchangedReviewFeedbackIgnoresReviewerRequestMessage(t *testing.T) {
	facts := readyFacts()
	facts.Deliveries = []core.Delivery{
		factDelivery(core.Message{ID: "initial", Sequence: 1, FromKind: core.MessageFromHuman, Intent: core.MessageFollow}, core.AgentRun{ID: "initial-run", MessageID: "initial", Role: "implement", State: core.AgentRunCompleted, InputRevision: "rev-0"}),
		factDelivery(core.Message{ID: "review-request", JobID: facts.Job.ID, Sequence: 2, FromKind: core.MessageFromWorkflow, Intent: core.MessageFollow}, core.AgentRun{ID: "review-run", JobID: facts.Job.ID, MessageID: "review-request", Role: "general", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision, Capability: coding.ReviewReadOnlyCapability, SandboxID: "review-sandbox"}),
		factDelivery(core.Message{ID: "review-feedback", Sequence: 3, FromKind: core.MessageFromAgent, Intent: core.MessageFollow}, core.AgentRun{ID: "feedback-run", MessageID: "review-feedback", Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision}),
	}
	facts.Sandboxes = append(facts.Sandboxes, core.Sandbox{ID: "review-sandbox", JobID: facts.Job.ID})
	facts.Evidence = []core.Evidence{
		{Kind: "git-revision", AgentRunID: "initial-run", Revision: facts.Job.Revision},
		{Kind: "review-observation", AgentRunID: "review-run", Revision: facts.Job.Revision},
		{Kind: "git-revision", AgentRunID: "feedback-run", Revision: facts.Job.Revision},
	}
	if id, detail := unchangedAttention(facts); id != "" {
		t.Fatalf("review feedback was treated as unchanged implementation attention: %s %s", id, detail)
	}
	if got := decideCurrentWork(facts); got.Kind != WorkPublishProposal {
		t.Fatalf("CurrentWork = %#v, want unchanged review feedback to continue to publication", got)
	}
}

func TestRevisionCandidateIsLatestBatchBoundary(t *testing.T) {
	facts := readyFacts()
	facts.Deliveries = []core.Delivery{
		factDelivery(core.Message{ID: "message-1", Sequence: 1, Intent: core.MessageFollow}, core.AgentRun{ID: "run-1", MessageID: "message-1", Role: "implement", State: core.AgentRunCompleted}),
		factDelivery(core.Message{ID: "message-2", Sequence: 2, Intent: core.MessageFollow}, core.AgentRun{ID: "run-2", MessageID: "message-2", Role: "implement", State: core.AgentRunCompleted}),
	}
	_, latestTurnStart := latestImplementationRuns(facts)
	if got := revisionCandidate(facts, latestTurnStart); got == nil || got.ID != "run-2" {
		t.Fatalf("revisionCandidate = %#v, want latest batch AgentRun", got)
	}
	facts.Evidence = []core.Evidence{{Kind: "git-revision", AgentRunID: "run-2"}}
	if got := revisionCandidate(facts, latestTurnStart); got != nil {
		t.Fatalf("revisionCandidate = %#v after latest observation; earlier batch member must not be observed again", got)
	}
}

func TestTerminalTargetSteerFallbackBecomesLatestRevisionBoundary(t *testing.T) {
	facts := readyFacts()
	facts.ReviewPlans = nil
	facts.Deliveries = []core.Delivery{
		factDelivery(core.Message{ID: "initial", Sequence: 1, Intent: core.MessageFollow}, core.AgentRun{ID: "run-initial", MessageID: "initial", Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-old", TurnOutcome: "completed"}),
		factDelivery(core.Message{ID: "fallback", Sequence: 2, Intent: core.MessageSteer, TargetTurnID: "turn-old"}, core.AgentRun{ID: "run-fallback", MessageID: "fallback", Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-new", TurnOutcome: "completed"}),
	}
	facts.Evidence = []core.Evidence{{Kind: "git-revision", AgentRunID: "run-initial", Revision: facts.Job.Revision}}

	if got := decideCurrentWork(facts); got.Kind != WorkObserveRevision || got.FactID != "run-fallback" {
		t.Fatalf("CurrentWork = %#v, want terminal-target steer fallback observation", got)
	}
	facts.Deliveries[1].AgentRun.State = core.AgentRunFailed
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "run-fallback" {
		t.Fatalf("CurrentWork = %#v, want failed terminal-target steer fallback attention", got)
	}
	facts.Deliveries[1].AgentRun.State = core.AgentRunUncertain
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "run-fallback" {
		t.Fatalf("CurrentWork = %#v, want uncertain terminal-target steer fallback attention", got)
	}
	facts.Deliveries[1].AgentRun.TurnID = ""
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "run-fallback" {
		t.Fatalf("CurrentWork = %#v, want unbound uncertain fallback input attention", got)
	}
	facts.Deliveries = append(facts.Deliveries, factDelivery(core.Message{ID: "recovery", Sequence: 3, Intent: core.MessageFollow}, core.AgentRun{ID: "run-recovery", MessageID: "recovery", Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-recovery", TurnOutcome: "completed"}))
	if got := decideCurrentWork(facts); got.Kind != WorkObserveRevision || got.FactID != "run-recovery" {
		t.Fatalf("CurrentWork = %#v, want later successful turn start to recover failed fallback", got)
	}
	facts.Deliveries = facts.Deliveries[:2]

	// A steer bound to its target is handled inside the original Turn and does
	// not create another Git observation boundary.
	facts.Deliveries[1].AgentRun.State = core.AgentRunCompleted
	facts.Deliveries[1].AgentRun.TurnID = "turn-old"
	_, latestTurnStart := latestImplementationRuns(facts)
	if got := revisionCandidate(facts, latestTurnStart); got != nil {
		t.Fatalf("shared-turn steer became a Revision candidate: %#v", got)
	}
}

func TestLatestImplementationInputStateCannotFallThrough(t *testing.T) {
	tests := []struct {
		name     string
		state    core.AgentRunState
		observed bool
		want     WorkKind
	}{
		{name: "pending without delivery candidate", state: core.AgentRunPending, want: WorkAttention},
		{name: "submitting", state: core.AgentRunSubmitting, want: WorkAttention},
		{name: "active without delivery candidate", state: core.AgentRunActive, want: WorkKind("observe-agent-run")},
		{name: "failed", state: core.AgentRunFailed, want: WorkAttention},
		{name: "interrupted", state: core.AgentRunInterrupted, want: WorkAttention},
		{name: "uncertain", state: core.AgentRunUncertain, want: WorkAttention},
		{name: "completed awaits Git observation", state: core.AgentRunCompleted, want: WorkObserveRevision},
		{name: "completed and observed may choose review", state: core.AgentRunCompleted, observed: true, want: WorkChooseReview},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := readyFacts()
			facts.ReviewPlans = nil
			message := core.Message{ID: "message-1", Sequence: 1, Intent: core.MessageFollow}
			run := core.AgentRun{ID: "run-1", MessageID: message.ID, Role: "implement", State: test.state, InputRevision: facts.Job.Revision}
			facts.Deliveries = []core.Delivery{factDelivery(message, run)}
			wantFactID := run.ID
			if test.observed {
				facts.Deliveries[0].AgentRun.InputRevision = "rev-0"
				facts.Evidence = []core.Evidence{{Kind: "git-revision", AgentRunID: run.ID, Revision: facts.Job.Revision}}
				wantFactID = facts.Job.Revision
			}
			got := decideCurrentWork(facts)
			if got.Kind != test.want || got.FactID != wantFactID {
				t.Fatalf("CurrentWork = %#v, want kind %s owned by latest safe fact", got, test.want)
			}
		})
	}
}

func TestRejectedPublicationReadinessBecomesAttention(t *testing.T) {
	facts := readyFacts()
	startedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	facts.Actions = append(facts.Actions, core.Action{
		Kind:      coding.ActionGitHubPullRequest,
		State:     core.ActionUnsettled,
		Scope:     facts.Job.Revision,
		CreatedAt: startedAt,
	})
	tentative := decideCurrentWork(facts)
	if tentative.Kind != WorkPublishProposal {
		t.Fatalf("tentative CurrentWork = %#v, want publication", tentative)
	}
	projection, err := facts.Project(blob.Store{})
	if err != nil {
		t.Fatal(err)
	}
	got := projection.CurrentWork
	if got.Kind != WorkAttention || !strings.HasPrefix(got.Detail, "publication lost exact-Revision readiness: ") {
		t.Fatalf("CurrentWork = %#v, want publication readiness attention", got)
	}
}
