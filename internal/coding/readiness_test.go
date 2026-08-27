package coding

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	policy "github.com/aphronio/dorf/internal/review"
)

func TestReviewReadinessRequiresExplicitDecisionAndSettledSelectedRuns(t *testing.T) {
	store, jobID, revision := readinessFixture(t)
	job := Job{Job: core.Job{ID: jobID}, Revision: revision, Branch: "dorf/readiness"}
	withoutPlan := AssessReviewReadiness(job, nil, store, nil, nil, nil)
	if withoutPlan.Ready || !strings.Contains(withoutPlan.Reason, "no explicit persisted") {
		t.Fatalf("missing plan readiness=%#v", withoutPlan)
	}
	noReview := ReviewPlanRecord{JobID: jobID, Revision: revision, Plan: policy.ReviewPlan{Decision: "no-review"}}
	explicit := AssessReviewReadiness(job, nil, store, &noReview, nil, nil)
	if !explicit.Ready || !strings.Contains(explicit.Reason, "explicitly selected no agent review") {
		t.Fatalf("explicit no-review readiness=%#v", explicit)
	}
	pending := MessageRecord{Message: core.Message{ID: "message-pending", JobID: jobID}, ProducerID: "agent-run-pending"}
	lateInput := AssessReviewReadiness(job, nil, store, &noReview, nil, []MessageRecord{pending})
	if lateInput.Ready || !strings.Contains(lateInput.Reason, "not terminal") {
		t.Fatalf("late input satisfied readiness: %#v", lateInput)
	}
	failedMessage := core.Message{ID: "message-failed", JobID: jobID, Sequence: 2, Intent: core.MessageFollow}
	recoveryMessage := core.Message{ID: "message-recovery", JobID: jobID, Sequence: 3, Intent: core.MessageFollow}
	failedRun := core.AgentRun{ID: "agent-run-failed", JobID: jobID, MessageID: failedMessage.ID, Role: "implement", InputRevision: revision, State: core.AgentRunFailed, TurnOutcome: "failed"}
	recoveryRun := core.AgentRun{ID: "agent-run-recovery", JobID: jobID, MessageID: recoveryMessage.ID, Role: "implement", InputRevision: revision, State: core.AgentRunCompleted, TurnOutcome: "completed"}
	failed := factMessage(failedMessage, failedRun)
	recovery := factMessage(recoveryMessage, recoveryRun)
	recoveryEvidence := gitObservationEvidence(t, store, job, recovery, time.Now().UTC().Truncate(time.Microsecond))
	recovered := AssessReviewReadiness(job, []core.Evidence{recoveryEvidence}, store, &noReview, nil, []MessageRecord{failed, recovery})
	if !recovered.Ready {
		t.Fatalf("later successful observed Follow did not recover old failure: %#v", recovered)
	}
	selected := ReviewPlanRecord{JobID: jobID, Revision: revision, Plan: policy.ReviewPlan{Decision: "selected", Roles: []policy.Role{policy.RoleGeneral}}}
	incomplete := AssessReviewReadiness(job, nil, store, &selected, nil, nil)
	if incomplete.Ready || !strings.Contains(incomplete.Reason, "has not returned a feedback Message") {
		t.Fatalf("incomplete selected readiness=%#v", incomplete)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	requestFromID := ReviewRequestFromID(revision, string(policy.RoleGeneral))
	requestID := ReviewRequestMessageID(jobID, revision, string(policy.RoleGeneral))
	runID := core.AgentRunID(requestID)
	feedbackID := core.MessageID(jobID, core.MessageFromAgent, runID)
	run := ReviewRunView{
		ID: runID, JobID: jobID, MessageID: requestID, InputRevision: revision, Role: string(policy.RoleGeneral), Outcome: "completed", TurnID: "turn-review", Harness: "codex", ThreadID: "thread-review", Capability: ReviewReadOnlyCapability, StartedAt: now, FinishedAt: now.Add(time.Second),
		Request: core.Message{ID: requestID, JobID: jobID, FromKind: core.MessageFromWorkflow, FromID: requestFromID, Sequence: 2, Input: "Review the exact Revision.", Intent: core.MessageFollow},
	}
	observed := reviewObservationEvidence(t, store, run, ReviewCheckoutObservation{Revision: revision, Tree: strings.Repeat("c", 40)})
	foreign := core.Message{ID: feedbackID, JobID: jobID, FromKind: core.MessageFromAgent, FromID: "agent-run-foreign", Sequence: 3, Input: "Foreign feedback.", Intent: core.MessageFollow}
	wrong := AssessReviewReadiness(job, []core.Evidence{observed}, store, &selected, []ReviewRunView{run}, []MessageRecord{{Message: foreign, ProducerID: "foreign"}})
	if wrong.Ready || !strings.Contains(wrong.Reason, "has not returned a feedback Message") {
		t.Fatalf("foreign feedback Message satisfied readiness: %#v", wrong)
	}
	feedback := core.Message{ID: feedbackID, JobID: jobID, FromKind: core.MessageFromAgent, FromID: runID, Sequence: 3, Input: "Consider simplifying the boundary.", Intent: core.MessageFollow}
	implementation := core.AgentRun{ID: core.AgentRunID(feedback.ID), JobID: jobID, MessageID: feedback.ID, Role: "implement", InputRevision: revision, State: core.AgentRunCompleted, TurnOutcome: "completed"}
	handledMessage := factMessage(feedback, implementation)
	handled := AssessReviewReadiness(job, []core.Evidence{observed}, store, &selected, []ReviewRunView{run}, []MessageRecord{handledMessage})
	if handled.Ready || !strings.Contains(handled.Reason, "no valid Git observation") {
		t.Fatalf("feedback without Git observation satisfied readiness: %#v", handled)
	}
	gitEvidence := gitObservationEvidence(t, store, job, handledMessage, now.Add(2*time.Second))
	stale := run
	stale.ID = "zz-stale-same-role-run"
	stale.InputRevision = strings.Repeat("f", 40)
	stale.Outcome = "failed"
	settled := AssessReviewReadiness(job, []core.Evidence{observed, gitEvidence}, store, &selected, []ReviewRunView{run, stale}, []MessageRecord{handledMessage})
	if !settled.Ready || !strings.Contains(settled.Reason, "returned feedback") {
		t.Fatalf("stale same-Role review blocked exact-Revision readiness=%#v", settled)
	}
}

func TestReviewReadinessRejectsMalformedPlanBeforeReviewRuns(t *testing.T) {
	job := Job{Job: core.Job{ID: "job-readiness"}, Revision: "revision-readiness"}
	tests := []struct {
		name   string
		plan   policy.ReviewPlan
		reason string
	}{
		{name: "missing decision", reason: "persisted review plan has no final decision"},
		{name: "no-review with Role", plan: policy.ReviewPlan{Decision: "no-review", Roles: []policy.Role{policy.RoleGeneral}}, reason: "persisted review decision and selected Roles disagree"},
		{name: "selected without Role", plan: policy.ReviewPlan{Decision: "selected"}, reason: "persisted review decision and selected Roles disagree"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := ReviewPlanRecord{JobID: job.ID, Revision: job.Revision, Plan: test.plan}
			got := AssessReviewReadiness(job, nil, blob.Store{}, &plan, nil, nil)
			if got != (ReadinessAssessment{Revision: job.Revision, Reason: test.reason}) {
				t.Fatalf("readiness = %#v", got)
			}
		})
	}

	plan := ReviewPlanRecord{JobID: job.ID, Revision: job.Revision, Plan: policy.ReviewPlan{Decision: "selected", Roles: []policy.Role{policy.RoleGeneral, policy.RoleAuthAuthority}}}
	got := AssessReviewReadiness(job, nil, blob.Store{}, &plan, nil, nil)
	wantReason := "selected review Role general has not returned a feedback Message with observed AgentRun Evidence"
	if got.Reason != wantReason {
		t.Fatalf("first selected Role reason = %q, want %q", got.Reason, wantReason)
	}
}

func gitObservationEvidence(t *testing.T, store blob.Store, job Job, message MessageRecord, started time.Time) core.Evidence {
	t.Helper()
	observation := gitworkspace.Observation{
		ComparisonBase: message.InputRevision, Revision: job.Revision, Tree: strings.Repeat("d", 40), Branch: job.Branch,
		StartedAt: started, FinishedAt: started.Add(time.Second),
	}
	contents, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Put(contents)
	if err != nil {
		t.Fatal(err)
	}
	return core.Evidence{
		ID: core.EvidenceID(message.ProducerID, "git-revision"), Digest: blob.Digest, ByteSize: blob.ByteSize,
		MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: "git-revision",
		AgentRunID: message.ProducerID, Revision: job.Revision, StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt,
	}
}

func reviewObservationEvidence(t *testing.T, store blob.Store, run ReviewRunView, checkout ReviewCheckoutObservation) core.Evidence {
	t.Helper()
	contents, err := json.Marshal(reviewObservationPayload{
		AgentRunID: run.ID, Revision: run.InputRevision, Role: run.Role, Capability: run.Capability,
		Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.Outcome, Checkout: checkout,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Put(contents)
	if err != nil {
		t.Fatal(err)
	}
	return core.Evidence{
		ID: core.EvidenceID(run.ID, "review-observation"), Digest: stored.Digest, ByteSize: stored.ByteSize,
		MediaType: "application/vnd.dorf.observation+json", Producer: reviewEvidenceProducer, Kind: "review-observation",
		AgentRunID: run.ID, Revision: run.InputRevision, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
	}
}

func readinessFixture(t *testing.T) (blob.Store, string, string) {
	t.Helper()
	store := blob.Store{Root: t.TempDir()}
	jobID, revision := "job-readiness", strings.Repeat("a", 40)
	return store, jobID, revision
}
