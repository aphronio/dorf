package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/gitworkspace"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

func readyFacts() Snapshot {
	job := spine.CodingJob{Job: spine.Job{ID: "job-1", AdmissionOpen: true}, Revision: "rev-1"}
	sandbox := spine.Sandbox{ID: spine.MainSandboxName(job.ID), JobID: job.ID}
	action := func(kind spine.ActionKind) spine.Action {
		return spine.Action{ID: spine.ScopedActionID(job.ID, kind, sandbox.ID), Kind: kind, State: spine.ActionSucceeded, Scope: sandbox.ID}
	}
	return Snapshot{
		Job:         job,
		MainSandbox: sandbox,
		Sandboxes:   []spine.Sandbox{sandbox},
		Actions: []spine.Action{
			action(spine.ActionSandboxCreate),
			action(gitworkspace.ActionRepositoryClone),
			action(spine.ActionRouteCreate),
		},
		ReviewPlans: []spine.ReviewPlanRecord{{JobID: job.ID, Revision: job.Revision}},
	}
}

func factDelivery(message spine.Message, run spine.AgentRun) spine.Delivery {
	return spine.Delivery{Message: message, AgentRun: run}
}

func TestCodingAgentInputKeepsReviewFeedbackOpaque(t *testing.T) {
	job := spine.CodingJob{Branch: "dorf/feedback", Revision: strings.Repeat("a", 40)}
	message := spine.Message{FromKind: spine.MessageFromAgent, FromID: "review-run-1", Input: "Reviewer prose that the implementation agent must interpret."}
	reviewer := codingAgentInput(job, spine.Delivery{Message: message, AgentRun: spine.AgentRun{Role: "critical-boundary"}})
	if reviewer != message.Input {
		t.Fatalf("reviewer input was rewritten: %q", reviewer)
	}
	implementation := codingAgentInput(job, spine.Delivery{Message: message, AgentRun: spine.AgentRun{Role: "implement"}})
	if !strings.HasPrefix(implementation, message.Input+"\n\n") || !strings.Contains(implementation, job.Branch) || !strings.Contains(implementation, job.Revision) {
		t.Fatalf("implementation input is missing the coding contract: %q", implementation)
	}
}

func TestCurrentWorkDependencyOrder(t *testing.T) {
	t.Run("selected reviewer Actions precede its AgentRun and implementation feedback", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans[0].Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		reviewer := spine.ReviewRunView{AgentRun: spine.AgentRun{ID: "review-1", JobID: facts.Job.ID, MessageID: "review-request-1", InputRevision: facts.Job.Revision, Role: string(policy.RoleGeneral), State: spine.AgentRunPending, Capability: spine.ReviewReadOnlyCapability, SandboxID: spine.ReviewSandboxName("review-1")}}
		reviewer.Sandbox = spine.Sandbox{ID: reviewer.SandboxID, JobID: facts.Job.ID}
		facts.Deliveries = []spine.Delivery{factDelivery(spine.Message{ID: reviewer.MessageID, JobID: facts.Job.ID}, reviewer.AgentRun)}
		facts.Sandboxes = append(facts.Sandboxes, reviewer.Sandbox)
		facts.Delivery = &spine.Delivery{Message: spine.Message{Sequence: 2}, AgentRun: spine.AgentRun{ID: "implement-2", State: spine.AgentRunPending}}
		steps := []struct {
			work   WorkKind
			action spine.ActionKind
		}{
			{WorkAction, spine.ActionSandboxCreate},
			{WorkAction, coding.ActionReviewCheckout},
			{WorkAction, spine.ActionRouteCreate},
		}
		for _, step := range steps {
			got := decideCurrentWork(facts)
			wantID := spine.ScopedActionID(facts.Job.ID, step.action, reviewer.Sandbox.ID)
			if got.Kind != step.work || got.ActionKind != step.action || got.FactID != wantID || got.Scope != reviewer.Sandbox.ID {
				t.Fatalf("CurrentWork = %#v, want %s Action %s", got, step.action, wantID)
			}
			facts.Actions = append(facts.Actions, spine.Action{ID: wantID, JobID: facts.Job.ID, Kind: step.action, State: spine.ActionSucceeded, Scope: reviewer.Sandbox.ID})
		}
		if got := decideCurrentWork(facts); got.Kind != WorkRunReviewer || got.FactID != reviewer.ID {
			t.Fatalf("CurrentWork = %#v, want selected reviewer after its exact Actions", got)
		}
	})

	t.Run("review feedback Message enters the ordinary implementation lane", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans[0].Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		requestID := spine.ReviewRequestMessageID(facts.Job.ID, facts.Job.Revision, string(policy.RoleGeneral))
		reviewerID := spine.AgentRunID(requestID)
		reviewer := spine.AgentRun{ID: reviewerID, JobID: facts.Job.ID, MessageID: requestID, InputRevision: facts.Job.Revision, Role: string(policy.RoleGeneral), State: spine.AgentRunCompleted, Capability: spine.ReviewReadOnlyCapability, SandboxID: spine.ReviewSandboxName(reviewerID)}
		feedback := spine.Message{ID: spine.MessageID(facts.Job.ID, spine.MessageFromAgent, reviewerID), JobID: facts.Job.ID, FromKind: spine.MessageFromAgent, FromID: reviewerID, Sequence: 2, Intent: spine.MessageFollow}
		facts.Deliveries = []spine.Delivery{factDelivery(spine.Message{ID: requestID, JobID: facts.Job.ID}, reviewer), factDelivery(feedback, spine.AgentRun{MessageID: feedback.ID, Role: "implement"})}
		facts.Sandboxes = append(facts.Sandboxes, spine.Sandbox{ID: reviewer.SandboxID, JobID: facts.Job.ID})
		facts.Delivery = &spine.Delivery{Message: feedback, AgentRun: spine.AgentRun{ID: spine.AgentRunID(feedback.ID), State: spine.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkDeliverMessage || got.FactID != facts.Delivery.AgentRun.ID {
			t.Fatalf("CurrentWork = %#v, want ordinary feedback Message delivery", got)
		}
	})

	t.Run("message precedes Git observation", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans = nil
		facts.Delivery = &spine.Delivery{Message: spine.Message{Sequence: 2}, AgentRun: spine.AgentRun{ID: "implement-2", State: spine.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkDeliverMessage || got.FactID != "implement-2" {
			t.Fatalf("CurrentWork = %#v, want Message delivery", got)
		}
	})

	t.Run("submitting Follow is reconciled before review", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans = nil
		message := spine.Message{ID: "message-2", JobID: facts.Job.ID, Sequence: 2, Intent: spine.MessageFollow}
		run := spine.AgentRun{ID: spine.AgentRunID(message.ID), JobID: facts.Job.ID, MessageID: message.ID, Role: "implement", State: spine.AgentRunSubmitting}
		facts.Deliveries = []spine.Delivery{factDelivery(message, run)}
		facts.Delivery = &spine.Delivery{Message: message, AgentRun: run}
		if got := decideCurrentWork(facts); got.Kind != WorkDeliverMessage || got.FactID != run.ID {
			t.Fatalf("CurrentWork = %#v, want submitting Follow reconciliation", got)
		}
	})

	t.Run("started publication settles before a later Message", func(t *testing.T) {
		facts := readyFacts()
		facts.Actions = append(facts.Actions,
			spine.Action{Kind: coding.ActionRepositoryPush, State: spine.ActionUnsettled, Scope: facts.Job.Revision},
			spine.Action{Kind: coding.ActionGitHubPullRequest, State: spine.ActionUnsettled, Scope: facts.Job.Revision},
		)
		facts.Delivery = &spine.Delivery{Message: spine.Message{Sequence: 2}, AgentRun: spine.AgentRun{ID: "implement-2", State: spine.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkPublishProposal {
			t.Fatalf("CurrentWork = %#v, want started publication reconciliation", got)
		}
	})

	t.Run("ready exact Revision observes its Proposal", func(t *testing.T) {
		facts := readyFacts()
		facts.Proposal = &spine.GitHubProposal{JobID: facts.Job.ID, Number: 12, URL: "https://example.test/12", ProposedRevision: facts.Job.Revision}
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
		from      spine.MessageFromKind
		proposal  bool
		attention bool
	}{
		{name: "initial human request", from: spine.MessageFromHuman, attention: true},
		{name: "review feedback", from: spine.MessageFromAgent},
		{name: "current Proposal feedback", from: spine.MessageFromHuman, proposal: true},
		{name: "workflow feedback", from: spine.MessageFromWorkflow, attention: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := readyFacts()
			message := spine.Message{ID: "message-1", JobID: facts.Job.ID, Sequence: 1, FromKind: test.from}
			run := spine.AgentRun{ID: "run-1", JobID: facts.Job.ID, MessageID: message.ID, Role: "implement", State: spine.AgentRunCompleted, InputRevision: facts.Job.Revision}
			facts.Deliveries = []spine.Delivery{factDelivery(message, run)}
			facts.Evidence = []spine.Evidence{{ID: "e-git", Kind: "git-revision", AgentRunID: run.ID, Revision: facts.Job.Revision}}
			if test.proposal {
				facts.Proposal = &spine.GitHubProposal{JobID: facts.Job.ID, Number: 12, URL: "https://example.test/12", ProposedRevision: facts.Job.Revision}
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
	facts.Deliveries = []spine.Delivery{
		factDelivery(spine.Message{ID: "initial", Sequence: 1, FromKind: spine.MessageFromHuman, Intent: spine.MessageFollow}, spine.AgentRun{ID: "initial-run", MessageID: "initial", Role: "implement", State: spine.AgentRunCompleted, InputRevision: "rev-0"}),
		factDelivery(spine.Message{ID: "review-request", JobID: facts.Job.ID, Sequence: 2, FromKind: spine.MessageFromWorkflow, Intent: spine.MessageFollow}, spine.AgentRun{ID: "review-run", JobID: facts.Job.ID, MessageID: "review-request", Role: "general", State: spine.AgentRunCompleted, InputRevision: facts.Job.Revision, Capability: spine.ReviewReadOnlyCapability, SandboxID: "review-sandbox"}),
		factDelivery(spine.Message{ID: "review-feedback", Sequence: 3, FromKind: spine.MessageFromAgent, Intent: spine.MessageFollow}, spine.AgentRun{ID: "feedback-run", MessageID: "review-feedback", Role: "implement", State: spine.AgentRunCompleted, InputRevision: facts.Job.Revision}),
	}
	facts.Sandboxes = append(facts.Sandboxes, spine.Sandbox{ID: "review-sandbox", JobID: facts.Job.ID})
	facts.Evidence = []spine.Evidence{
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
	facts.Deliveries = []spine.Delivery{
		factDelivery(spine.Message{ID: "message-1", Sequence: 1, Intent: spine.MessageFollow}, spine.AgentRun{ID: "run-1", MessageID: "message-1", Role: "implement", State: spine.AgentRunCompleted}),
		factDelivery(spine.Message{ID: "message-2", Sequence: 2, Intent: spine.MessageFollow}, spine.AgentRun{ID: "run-2", MessageID: "message-2", Role: "implement", State: spine.AgentRunCompleted}),
	}
	_, latestTurnStart := latestImplementationRuns(facts)
	if got := revisionCandidate(facts, latestTurnStart); got == nil || got.ID != "run-2" {
		t.Fatalf("revisionCandidate = %#v, want latest batch AgentRun", got)
	}
	facts.Evidence = []spine.Evidence{{Kind: "git-revision", AgentRunID: "run-2"}}
	if got := revisionCandidate(facts, latestTurnStart); got != nil {
		t.Fatalf("revisionCandidate = %#v after latest observation; earlier batch member must not be observed again", got)
	}
}

func TestTerminalTargetSteerFallbackBecomesLatestRevisionBoundary(t *testing.T) {
	facts := readyFacts()
	facts.ReviewPlans = nil
	facts.Deliveries = []spine.Delivery{
		factDelivery(spine.Message{ID: "initial", Sequence: 1, Intent: spine.MessageFollow}, spine.AgentRun{ID: "run-initial", MessageID: "initial", Role: "implement", State: spine.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-old", TurnOutcome: "completed"}),
		factDelivery(spine.Message{ID: "fallback", Sequence: 2, Intent: spine.MessageSteer, TargetTurnID: "turn-old"}, spine.AgentRun{ID: "run-fallback", MessageID: "fallback", Role: "implement", State: spine.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-new", TurnOutcome: "completed"}),
	}
	facts.Evidence = []spine.Evidence{{Kind: "git-revision", AgentRunID: "run-initial", Revision: facts.Job.Revision}}

	if got := decideCurrentWork(facts); got.Kind != WorkObserveRevision || got.FactID != "run-fallback" {
		t.Fatalf("CurrentWork = %#v, want terminal-target steer fallback observation", got)
	}
	facts.Deliveries[1].AgentRun.State = spine.AgentRunFailed
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "run-fallback" {
		t.Fatalf("CurrentWork = %#v, want failed terminal-target steer fallback attention", got)
	}
	facts.Deliveries[1].AgentRun.State = spine.AgentRunUncertain
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "run-fallback" {
		t.Fatalf("CurrentWork = %#v, want uncertain terminal-target steer fallback attention", got)
	}
	facts.Deliveries[1].AgentRun.TurnID = ""
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "run-fallback" {
		t.Fatalf("CurrentWork = %#v, want unbound uncertain fallback input attention", got)
	}
	facts.Deliveries = append(facts.Deliveries, factDelivery(spine.Message{ID: "recovery", Sequence: 3, Intent: spine.MessageFollow}, spine.AgentRun{ID: "run-recovery", MessageID: "recovery", Role: "implement", State: spine.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-recovery", TurnOutcome: "completed"}))
	if got := decideCurrentWork(facts); got.Kind != WorkObserveRevision || got.FactID != "run-recovery" {
		t.Fatalf("CurrentWork = %#v, want later successful turn start to recover failed fallback", got)
	}
	facts.Deliveries = facts.Deliveries[:2]

	// A steer bound to its target is handled inside the original Turn and does
	// not create another Git observation boundary.
	facts.Deliveries[1].AgentRun.State = spine.AgentRunCompleted
	facts.Deliveries[1].AgentRun.TurnID = "turn-old"
	_, latestTurnStart := latestImplementationRuns(facts)
	if got := revisionCandidate(facts, latestTurnStart); got != nil {
		t.Fatalf("shared-turn steer became a Revision candidate: %#v", got)
	}
}

func TestLatestImplementationInputStateCannotFallThrough(t *testing.T) {
	tests := []struct {
		name     string
		state    spine.AgentRunState
		observed bool
		want     WorkKind
	}{
		{name: "pending without delivery candidate", state: spine.AgentRunPending, want: WorkAttention},
		{name: "submitting", state: spine.AgentRunSubmitting, want: WorkAttention},
		{name: "active without delivery candidate", state: spine.AgentRunActive, want: WorkKind("observe-agent-run")},
		{name: "failed", state: spine.AgentRunFailed, want: WorkAttention},
		{name: "interrupted", state: spine.AgentRunInterrupted, want: WorkAttention},
		{name: "uncertain", state: spine.AgentRunUncertain, want: WorkAttention},
		{name: "completed awaits Git observation", state: spine.AgentRunCompleted, want: WorkObserveRevision},
		{name: "completed and observed may choose review", state: spine.AgentRunCompleted, observed: true, want: WorkChooseReview},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := readyFacts()
			facts.ReviewPlans = nil
			message := spine.Message{ID: "message-1", Sequence: 1, Intent: spine.MessageFollow}
			run := spine.AgentRun{ID: "run-1", MessageID: message.ID, Role: "implement", State: test.state, InputRevision: facts.Job.Revision}
			facts.Deliveries = []spine.Delivery{factDelivery(message, run)}
			wantFactID := run.ID
			if test.observed {
				facts.Deliveries[0].AgentRun.InputRevision = "rev-0"
				facts.Evidence = []spine.Evidence{{Kind: "git-revision", AgentRunID: run.ID, Revision: facts.Job.Revision}}
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
	facts.Actions = append(facts.Actions, spine.Action{
		Kind:      coding.ActionGitHubPullRequest,
		State:     spine.ActionUnsettled,
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
