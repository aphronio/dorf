package workflow

import (
	"context"
	"testing"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

type publicationReadinessStub struct {
	assessment spine.ReadinessAssessment
	intentAt   time.Time
}

func (s *publicationReadinessStub) AssessReadiness(_ context.Context, _, _ string, intentAt time.Time) (spine.ReadinessAssessment, error) {
	s.intentAt = intentAt
	return s.assessment, nil
}

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
	t.Run("selected reviewer Actions precede its AgentRun and implementation feedback", func(t *testing.T) {
		facts := readyFacts()
		facts.reviewPlan.Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		reviewer := spine.ReviewRunView{AgentRun: spine.AgentRun{ID: "review-1", JobID: facts.job.ID, Role: string(policy.RoleGeneral), State: spine.AgentRunPending, SandboxID: spine.ReviewSandboxName("review-1")}}
		reviewer.Sandbox = spine.Sandbox{ID: reviewer.SandboxID, JobID: facts.job.ID}
		facts.reviewRuns = []spine.ReviewRunView{reviewer}
		facts.delivery = &spine.Delivery{Message: spine.Message{Sequence: 2}, AgentRun: spine.AgentRun{ID: "implement-2", State: spine.AgentRunPending}}
		steps := []struct {
			work   WorkKind
			action spine.ActionKind
		}{
			{WorkCreateReviewSandbox, spine.ActionSandboxCreate},
			{WorkCheckoutReview, spine.ActionReviewCheckout},
			{WorkCreateReviewRoute, spine.ActionRouteCreate},
		}
		for _, step := range steps {
			got := decideCurrentWork(facts)
			wantID := spine.ScopedActionID(facts.job.ID, step.action, reviewer.Sandbox.ID)
			if got.Kind != step.work || got.FactID != wantID || got.Scope != reviewer.Sandbox.ID {
				t.Fatalf("CurrentWork = %#v, want %s Action %s", got, step.action, wantID)
			}
			facts.actions = append(facts.actions, spine.Action{ID: wantID, JobID: facts.job.ID, Kind: step.action, State: spine.ActionSucceeded, Scope: reviewer.Sandbox.ID})
		}
		if got := decideCurrentWork(facts); got.Kind != WorkRunReviewer || got.FactID != reviewer.ID {
			t.Fatalf("CurrentWork = %#v, want selected reviewer after its exact Actions", got)
		}
	})

	t.Run("review feedback Message enters the ordinary implementation lane", func(t *testing.T) {
		facts := readyFacts()
		facts.reviewPlan.Plan = policy.ReviewPlan{Roles: []policy.Role{policy.RoleGeneral}}
		requestID := spine.ReviewRequestMessageID(facts.job.ID, facts.job.Revision, string(policy.RoleGeneral))
		reviewerID := spine.AgentRunID(requestID)
		facts.reviewRuns = []spine.ReviewRunView{{AgentRun: spine.AgentRun{ID: reviewerID, Role: string(policy.RoleGeneral), State: spine.AgentRunCompleted}}}
		feedback := spine.Message{ID: spine.MessageID(facts.job.ID, spine.MessageFromAgent, reviewerID), JobID: facts.job.ID, FromKind: spine.MessageFromAgent, FromID: reviewerID, Sequence: 2, Intent: spine.MessageFollow}
		facts.messages = []spine.MessageView{{Message: feedback}}
		facts.delivery = &spine.Delivery{Message: feedback, AgentRun: spine.AgentRun{ID: spine.AgentRunID(feedback.ID), State: spine.AgentRunPending}}
		if got := decideCurrentWork(facts); got.Kind != WorkDeliverMessage || got.FactID != facts.delivery.AgentRun.ID {
			t.Fatalf("CurrentWork = %#v, want ordinary feedback Message delivery", got)
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

	t.Run("submitting Follow is reconciled before Checks", func(t *testing.T) {
		facts := readyFacts()
		facts.reviewPlan = nil
		facts.checks = nil
		message := spine.Message{ID: "message-2", JobID: facts.job.ID, Sequence: 2, Intent: spine.MessageFollow}
		run := spine.AgentRun{ID: spine.AgentRunID(message.ID), JobID: facts.job.ID, MessageID: message.ID, Role: "implement", State: spine.AgentRunSubmitting}
		facts.messages = []spine.MessageView{{Message: message}}
		facts.runs = []spine.AgentRun{run}
		facts.delivery = &spine.Delivery{Message: message, AgentRun: run}
		if got := decideCurrentWork(facts); got.Kind != WorkDeliverMessage || got.FactID != run.ID {
			t.Fatalf("CurrentWork = %#v, want submitting Follow reconciliation", got)
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

func TestDownstreamFactsWaitForCodingPrerequisites(t *testing.T) {
	facts := readyFacts()
	if !codingPrerequisitesComplete(facts) {
		t.Fatal("complete infrastructure and setup were not ready for coding facts")
	}
	facts.setup.State = spine.ActionFailed
	if codingPrerequisitesComplete(facts) {
		t.Fatal("failed setup exposed downstream coding facts")
	}
	facts = readyFacts()
	facts.actions = facts.actions[1:]
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

func TestUnchangedReviewFeedbackIgnoresReviewerRequestMessage(t *testing.T) {
	facts := readyFacts()
	facts.messages = []spine.MessageView{
		{Message: spine.Message{ID: "initial", Sequence: 1, FromKind: spine.MessageFromHuman, Intent: spine.MessageFollow}},
		{Message: spine.Message{ID: "review-request", Sequence: 2, FromKind: spine.MessageFromWorkflow, Intent: spine.MessageFollow}},
		{Message: spine.Message{ID: "review-feedback", Sequence: 3, FromKind: spine.MessageFromAgent, Intent: spine.MessageFollow}},
	}
	facts.runs = []spine.AgentRun{
		{ID: "initial-run", MessageID: "initial", Role: "implement", State: spine.AgentRunCompleted, InputRevision: "rev-0"},
		{ID: "review-run", MessageID: "review-request", Role: "general", State: spine.AgentRunCompleted, InputRevision: facts.job.Revision},
		{ID: "feedback-run", MessageID: "review-feedback", Role: "implement", State: spine.AgentRunCompleted, InputRevision: facts.job.Revision},
	}
	facts.evidence = []spine.Evidence{
		{Kind: "git-revision", AgentRunID: "initial-run", Revision: facts.job.Revision},
		{Kind: "review-observation", AgentRunID: "review-run", Revision: facts.job.Revision},
		{Kind: "git-revision", AgentRunID: "feedback-run", Revision: facts.job.Revision},
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

func TestLatestImplementationFollowStateCannotFallThrough(t *testing.T) {
	tests := []struct {
		name     string
		state    spine.AgentRunState
		observed bool
		want     WorkKind
	}{
		{name: "pending without delivery candidate", state: spine.AgentRunPending, want: WorkAttention},
		{name: "submitting", state: spine.AgentRunSubmitting, want: WorkAttention},
		{name: "active without delivery candidate", state: spine.AgentRunActive, want: WorkAttention},
		{name: "failed", state: spine.AgentRunFailed, want: WorkAttention},
		{name: "interrupted", state: spine.AgentRunInterrupted, want: WorkAttention},
		{name: "uncertain", state: spine.AgentRunUncertain, want: WorkAttention},
		{name: "completed awaits Git observation", state: spine.AgentRunCompleted, want: WorkObserveRevision},
		{name: "completed and observed may run Checks", state: spine.AgentRunCompleted, observed: true, want: WorkRunChecks},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := readyFacts()
			facts.checks = nil
			facts.reviewPlan = nil
			message := spine.Message{ID: "message-1", Sequence: 1, Intent: spine.MessageFollow}
			run := spine.AgentRun{ID: "run-1", MessageID: message.ID, Role: "implement", State: test.state, InputRevision: facts.job.Revision}
			facts.messages = []spine.MessageView{{Message: message}}
			facts.runs = []spine.AgentRun{run}
			wantFactID := run.ID
			if test.observed {
				facts.runs[0].InputRevision = "rev-0"
				facts.evidence = []spine.Evidence{{Kind: "git-revision", AgentRunID: run.ID, Revision: facts.job.Revision}}
				wantFactID = spine.CheckID(facts.job.ID, facts.job.Revision, "check")
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
	facts.actions = append(facts.actions, spine.Action{
		Kind:      spine.ActionGitHubPullRequest,
		State:     spine.ActionUnsettled,
		Scope:     facts.job.Revision,
		CreatedAt: startedAt,
	})
	tentative := decideCurrentWork(facts)
	if tentative.Kind != WorkPublishProposal {
		t.Fatalf("tentative CurrentWork = %#v, want publication", tentative)
	}
	readiness := &publicationReadinessStub{assessment: spine.ReadinessAssessment{Reason: "Evidence digest does not match"}}
	got, err := assessPublicationReadiness(context.Background(), readiness, facts, tentative)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != WorkAttention || got.Detail != "publication lost exact-Revision readiness: Evidence digest does not match" {
		t.Fatalf("CurrentWork = %#v, want publication readiness attention", got)
	}
	if readiness.intentAt != startedAt {
		t.Fatalf("publication readiness intent = %s, want PR Action creation %s", readiness.intentAt, startedAt)
	}
}
