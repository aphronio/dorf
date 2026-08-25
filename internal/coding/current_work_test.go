package coding

import (
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	policy "github.com/aphronio/dorf/internal/review"
)

func readyFacts() Snapshot {
	job := Job{Job: core.Job{ID: "job-1", AdmissionOpen: true}, Revision: "rev-1"}
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
		ReviewPlans: []ReviewPlanRecord{{JobID: job.ID, Revision: job.Revision}},
	}
}

func factMessage(message core.Message, run core.AgentRun) MessageRecord {
	outcome := ""
	if run.State == core.AgentRunCompleted || run.State == core.AgentRunFailed || run.State == core.AgentRunInterrupted {
		outcome = run.TurnOutcome
		if outcome == "" {
			outcome = string(run.State)
		}
	}
	return MessageRecord{
		Message: message, SandboxID: run.SandboxID, InputRevision: run.InputRevision,
		ProducerID: run.ID, Outcome: outcome, Attention: run.Attention,
		StartsTurn: message.Intent == core.MessageFollow || message.Intent == core.MessageSteer && run.TurnID != "" && run.TurnID != message.TargetTurnID,
	}
}

func TestCodingAgentPromptKeepsReviewFeedbackOpaque(t *testing.T) {
	job := Job{Branch: "dorf/feedback", Revision: strings.Repeat("a", 40)}
	message := core.Message{FromKind: core.MessageFromAgent, FromID: "review-run-1", Input: "Reviewer prose that the implementation agent must interpret."}
	implementation := AgentPrompt(job, message.Input)
	if !strings.HasPrefix(implementation, message.Input+"\n\n") || !strings.Contains(implementation, job.Branch) || !strings.Contains(implementation, job.Revision) {
		t.Fatalf("implementation input is missing the coding contract: %q", implementation)
	}
}

func TestCurrentWorkDependencyOrder(t *testing.T) {
	t.Run("selected reviewer Actions precede its AgentRun and implementation feedback", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans[0].Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		reviewer := ReviewRunView{ID: "review-1", JobID: facts.Job.ID, MessageID: "review-request-1", InputRevision: facts.Job.Revision, Role: string(policy.RoleGeneral), Capability: ReviewReadOnlyCapability, SandboxID: ReviewSandboxName(facts.Job.ID, "review-1")}
		reviewer.Sandbox = core.Sandbox{ID: reviewer.SandboxID, JobID: facts.Job.ID}
		facts.ReviewRuns = []ReviewRunView{reviewer}
		facts.Sandboxes = append(facts.Sandboxes, reviewer.Sandbox)
		steps := []struct {
			work   WorkKind
			action core.ActionKind
		}{
			{WorkAction, core.ActionSandboxCreate},
			{WorkAction, ActionReviewCheckout},
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
		if got := decideCurrentWork(facts); got.Kind != WorkWaitAgent || got.FactID != reviewer.MessageID {
			t.Fatalf("CurrentWork = %#v, want selected reviewer after its exact Actions", got)
		}
		reviewer.Outcome = "completed"
		facts.ReviewRuns = []ReviewRunView{reviewer}
		if got := decideCurrentWork(facts); got.Kind != WorkRecordReview || got.FactID != reviewer.MessageID {
			t.Fatalf("CurrentWork = %#v, want typed review feedback recording", got)
		}
	})

	t.Run("completed review boundary failure remains durable attention", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans[0].Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		messageID := ReviewRequestMessageID(facts.Job.ID, facts.Job.Revision, string(policy.RoleGeneral))
		runID := core.AgentRunID(messageID)
		sandbox := core.Sandbox{ID: ReviewSandboxName(facts.Job.ID, runID), JobID: facts.Job.ID}
		run := ReviewRunView{ID: runID, JobID: facts.Job.ID, MessageID: messageID, InputRevision: facts.Job.Revision, Role: string(policy.RoleGeneral), Outcome: "completed", Capability: ReviewReadOnlyCapability, SandboxID: sandbox.ID, Sandbox: sandbox}
		facts.ReviewRuns = []ReviewRunView{run}
		facts.Sandboxes = append(facts.Sandboxes, sandbox)
		for _, kind := range []core.ActionKind{core.ActionSandboxCreate, ActionReviewCheckout, core.ActionRouteCreate} {
			facts.Actions = append(facts.Actions, core.Action{ID: core.ScopedActionID(facts.Job.ID, kind, sandbox.ID), JobID: facts.Job.ID, Kind: kind, State: core.ActionSucceeded, Scope: sandbox.ID})
		}
		facts.Job.WorkflowAttentionSource = messageID
		facts.Job.WorkflowAttention = "review Role general returned no feedback text"
		if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != messageID || got.Detail != facts.Job.WorkflowAttention {
			t.Fatalf("CurrentWork=%#v, want durable review attention", got)
		}
	})

	t.Run("review feedback Message enters the ordinary implementation lane", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans[0].Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		requestID := ReviewRequestMessageID(facts.Job.ID, facts.Job.Revision, string(policy.RoleGeneral))
		reviewerID := core.AgentRunID(requestID)
		reviewer := ReviewRunView{ID: reviewerID, JobID: facts.Job.ID, MessageID: requestID, InputRevision: facts.Job.Revision, Role: string(policy.RoleGeneral), Outcome: "completed", Capability: ReviewReadOnlyCapability, SandboxID: ReviewSandboxName(facts.Job.ID, reviewerID)}
		feedback := core.Message{ID: core.MessageID(facts.Job.ID, core.MessageFromAgent, reviewerID), JobID: facts.Job.ID, FromKind: core.MessageFromAgent, FromID: reviewerID, Sequence: 2, Intent: core.MessageFollow}
		facts.ReviewRuns = []ReviewRunView{reviewer}
		facts.Messages = []MessageRecord{factMessage(feedback, core.AgentRun{ID: core.AgentRunID(feedback.ID), MessageID: feedback.ID, Role: "implement", SandboxID: facts.MainSandbox.ID})}
		facts.Sandboxes = append(facts.Sandboxes, core.Sandbox{ID: reviewer.SandboxID, JobID: facts.Job.ID})
		if got := decideCurrentWork(facts); got.Kind != WorkWaitAgent || got.FactID != feedback.ID {
			t.Fatalf("CurrentWork = %#v, want ordinary feedback Message wait", got)
		}
	})

	t.Run("message precedes Git observation", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans = nil
		message := core.Message{ID: "message-implement-2", JobID: facts.Job.ID, Sequence: 2, Intent: core.MessageFollow}
		facts.Messages = []MessageRecord{factMessage(message, core.AgentRun{ID: core.AgentRunID(message.ID), MessageID: message.ID, Role: "implement", SandboxID: facts.MainSandbox.ID})}
		if got := decideCurrentWork(facts); got.Kind != WorkWaitAgent || got.FactID != "message-implement-2" {
			t.Fatalf("CurrentWork = %#v, want active Message wait", got)
		}
	})

	t.Run("submitting Follow is reconciled before review", func(t *testing.T) {
		facts := readyFacts()
		facts.ReviewPlans = nil
		message := core.Message{ID: "message-2", JobID: facts.Job.ID, Sequence: 2, Intent: core.MessageFollow}
		run := core.AgentRun{ID: core.AgentRunID(message.ID), JobID: facts.Job.ID, MessageID: message.ID, Role: "implement", State: core.AgentRunSubmitting}
		facts.Messages = []MessageRecord{factMessage(message, run)}
		if got := decideCurrentWork(facts); got.Kind != WorkWaitAgent || got.FactID != message.ID {
			t.Fatalf("CurrentWork = %#v, want submitting Follow wait", got)
		}
	})

	t.Run("started publication settles before a later Message", func(t *testing.T) {
		facts := readyFacts()
		facts.Actions = append(facts.Actions,
			core.Action{Kind: ActionRepositoryPush, State: core.ActionUnsettled, Scope: facts.Job.Revision},
			core.Action{Kind: ActionGitHubPullRequest, State: core.ActionUnsettled, Scope: facts.Job.Revision},
		)
		message := core.Message{ID: "message-implement-2", JobID: facts.Job.ID, Sequence: 2, Intent: core.MessageFollow}
		facts.Messages = []MessageRecord{factMessage(message, core.AgentRun{ID: core.AgentRunID(message.ID), MessageID: message.ID, Role: "implement", SandboxID: facts.MainSandbox.ID})}
		if got := decideCurrentWork(facts); got.Kind != WorkPublishProposal {
			t.Fatalf("CurrentWork = %#v, want started publication reconciliation", got)
		}
	})

	t.Run("ready exact Revision observes its Proposal", func(t *testing.T) {
		facts := readyFacts()
		facts.Proposal = &Proposal{JobID: facts.Job.ID, Number: 12, URL: "https://example.test/12", ProposedRevision: facts.Job.Revision}
		if got := decideCurrentWork(facts); got.Kind != WorkObserveProposal {
			t.Fatalf("CurrentWork = %#v, want Proposal observation", got)
		}
	})
}

func TestCurrentWorkSurfacesSandboxActionAttention(t *testing.T) {
	facts := readyFacts()
	facts.Actions = nil
	source := core.ScopedActionID(facts.Job.ID, core.ActionSandboxCreate, facts.MainSandbox.ID)
	facts.Job.WorkflowAttentionSource = source
	facts.Job.WorkflowAttention = "the exact Sandbox profile artifact is unavailable"
	work := decideCurrentWork(facts)
	if work.Kind != WorkAttention || work.FactID != source || work.Detail != facts.Job.WorkflowAttention {
		t.Fatalf("CurrentWork = %#v", work)
	}
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
			facts.Messages = []MessageRecord{factMessage(message, run)}
			facts.Evidence = []core.Evidence{{ID: "e-git", Kind: "git-revision", AgentRunID: run.ID, Revision: facts.Job.Revision}}
			if test.proposal {
				facts.Proposal = &Proposal{JobID: facts.Job.ID, Number: 12, URL: "https://example.test/12", ProposedRevision: facts.Job.Revision}
			}
			id, _ := unchangedAttention(facts)
			if (id != "") != test.attention {
				t.Fatalf("unchangedAttention ID = %q, want attention=%t", id, test.attention)
			}
		})
	}
}

func TestUnchangedReviewFeedbackIgnoresSharedTurnSteer(t *testing.T) {
	facts := readyFacts()
	facts.Messages = []MessageRecord{
		factMessage(core.Message{ID: "initial", Sequence: 1, FromKind: core.MessageFromHuman, Intent: core.MessageFollow}, core.AgentRun{ID: "initial-run", MessageID: "initial", Role: "implement", State: core.AgentRunCompleted, InputRevision: "rev-0"}),
		factMessage(core.Message{ID: "active-steer", Sequence: 2, FromKind: core.MessageFromHuman, Intent: core.MessageSteer, TargetTurnID: "turn-1"}, core.AgentRun{ID: "steer-run", MessageID: "active-steer", Role: "implement", State: core.AgentRunCompleted, InputRevision: "rev-0", TurnID: "turn-1"}),
		factMessage(core.Message{ID: "review-feedback", Sequence: 3, FromKind: core.MessageFromAgent, Intent: core.MessageFollow}, core.AgentRun{ID: "feedback-run", MessageID: "review-feedback", Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision}),
	}
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

	fallback := factMessage(
		core.Message{ID: "fallback-steer", Sequence: 1, FromKind: core.MessageFromHuman, Intent: core.MessageSteer, TargetTurnID: "turn-old"},
		core.AgentRun{ID: "fallback-run", MessageID: "fallback-steer", Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-new"},
	)
	facts.Messages = []MessageRecord{fallback}
	facts.Evidence = []core.Evidence{{Kind: "git-revision", AgentRunID: fallback.ProducerID, Revision: facts.Job.Revision}}
	if id, _ := unchangedAttention(facts); id != fallback.Message.ID {
		t.Fatalf("terminal-target fallback attention ID = %q, want %q", id, fallback.Message.ID)
	}
}

func TestRevisionCandidateIsLatestBatchBoundary(t *testing.T) {
	facts := readyFacts()
	facts.Messages = []MessageRecord{
		factMessage(core.Message{ID: "message-1", Sequence: 1, Intent: core.MessageFollow}, core.AgentRun{ID: "run-1", MessageID: "message-1", Role: "implement", State: core.AgentRunCompleted}),
		factMessage(core.Message{ID: "message-2", Sequence: 2, Intent: core.MessageFollow}, core.AgentRun{ID: "run-2", MessageID: "message-2", Role: "implement", State: core.AgentRunCompleted}),
	}
	_, latestTurnStart := latestImplementationRuns(facts)
	if got := revisionCandidate(facts, latestTurnStart); got == nil || got.ProducerID != "run-2" {
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
	facts.Messages = []MessageRecord{
		factMessage(core.Message{ID: "initial", Sequence: 1, Intent: core.MessageFollow}, core.AgentRun{ID: "run-initial", MessageID: "initial", Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-old", TurnOutcome: "completed"}),
		factMessage(core.Message{ID: "fallback", Sequence: 2, Intent: core.MessageSteer, TargetTurnID: "turn-old"}, core.AgentRun{ID: "run-fallback", MessageID: "fallback", Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-new", TurnOutcome: "completed"}),
	}
	facts.Evidence = []core.Evidence{{Kind: "git-revision", AgentRunID: "run-initial", Revision: facts.Job.Revision}}

	if got := decideCurrentWork(facts); got.Kind != WorkObserveRevision || got.FactID != "fallback" {
		t.Fatalf("CurrentWork = %#v, want terminal-target steer fallback observation", got)
	}
	facts.Messages[1].Outcome = "failed"
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "fallback" {
		t.Fatalf("CurrentWork = %#v, want failed terminal-target steer fallback attention", got)
	}
	facts.Messages[1].Outcome = ""
	facts.Messages[1].Attention = "accepted fallback is uncertain"
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "fallback" {
		t.Fatalf("CurrentWork = %#v, want uncertain terminal-target steer fallback attention", got)
	}
	facts.Messages[1].StartsTurn = false
	if got := decideCurrentWork(facts); got.Kind != WorkAttention || got.FactID != "fallback" {
		t.Fatalf("CurrentWork = %#v, want unbound uncertain fallback input attention", got)
	}
	facts.Messages = append(facts.Messages, factMessage(core.Message{ID: "recovery", Sequence: 3, Intent: core.MessageFollow}, core.AgentRun{ID: "run-recovery", MessageID: "recovery", Role: "implement", State: core.AgentRunCompleted, InputRevision: facts.Job.Revision, TurnID: "turn-recovery", TurnOutcome: "completed"}))
	if got := decideCurrentWork(facts); got.Kind != WorkObserveRevision || got.FactID != "recovery" {
		t.Fatalf("CurrentWork = %#v, want later successful turn start to recover failed fallback", got)
	}
	facts.Messages = facts.Messages[:2]

	// A steer bound to its target is handled inside the original Turn and does
	// not create another Git observation boundary.
	facts.Messages[1].Outcome = "completed"
	facts.Messages[1].Attention = ""
	facts.Messages[1].StartsTurn = false
	_, latestTurnStart := latestImplementationRuns(facts)
	if got := revisionCandidate(facts, latestTurnStart); got != nil {
		t.Fatalf("shared-turn steer became a Revision candidate: %#v", got)
	}
}

func TestLatestImplementationMessageCannotFallThrough(t *testing.T) {
	tests := []struct {
		name      string
		outcome   string
		attention string
		observed  bool
		want      WorkKind
	}{
		{name: "nonterminal waits for opaque Core cycle", want: WorkWaitAgent},
		{name: "durable attention", attention: "accepted mutation is uncertain", want: WorkAttention},
		{name: "failed", outcome: "failed", want: WorkAttention},
		{name: "interrupted", outcome: "interrupted", want: WorkAttention},
		{name: "completed awaits Git observation", outcome: "completed", want: WorkObserveRevision},
		{name: "completed and observed may choose review", outcome: "completed", observed: true, want: WorkChooseReview},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := readyFacts()
			facts.ReviewPlans = nil
			message := core.Message{ID: "message-1", Sequence: 1, Intent: core.MessageFollow}
			facts.Messages = []MessageRecord{{Message: message, ProducerID: "run-1", InputRevision: facts.Job.Revision, Outcome: test.outcome, Attention: test.attention, StartsTurn: true}}
			wantFactID := message.ID
			if test.observed {
				facts.Messages[0].InputRevision = "rev-0"
				facts.Evidence = []core.Evidence{{Kind: "git-revision", AgentRunID: "run-1", Revision: facts.Job.Revision}}
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
		Kind:      ActionGitHubPullRequest,
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
