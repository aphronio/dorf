package coding

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/gitworkspace"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

func TestReviewReadinessRequiresExplicitDecisionAndSettledSelectedRuns(t *testing.T) {
	store, jobID, revision := readinessFixture(t)
	job := Job{Job: spine.Job{ID: jobID}, Revision: revision, Branch: "dorf/readiness"}
	withoutPlan := AssessReviewReadiness(job, nil, store, nil, nil, nil)
	if withoutPlan.Ready || !strings.Contains(withoutPlan.Reason, "no explicit persisted") {
		t.Fatalf("missing plan readiness=%#v", withoutPlan)
	}
	noReview := ReviewPlanRecord{JobID: jobID, Revision: revision, Plan: policy.ReviewPlan{Decision: "no-review"}}
	explicit := AssessReviewReadiness(job, nil, store, &noReview, nil, nil)
	if !explicit.Ready || !strings.Contains(explicit.Reason, "explicitly selected no agent review") {
		t.Fatalf("explicit no-review readiness=%#v", explicit)
	}
	pending := spine.AgentRun{ID: "agent-run-pending", JobID: jobID, MessageID: "message-pending", Role: "implement", State: spine.AgentRunPending}
	lateInput := AssessReviewReadiness(job, nil, store, &noReview, nil, []spine.Delivery{{Message: spine.Message{ID: pending.MessageID}, AgentRun: pending}})
	if lateInput.Ready || !strings.Contains(lateInput.Reason, "not terminal") {
		t.Fatalf("late input satisfied readiness: %#v", lateInput)
	}
	failedMessage := spine.Message{ID: "message-failed", JobID: jobID, Sequence: 2, Intent: spine.MessageFollow}
	recoveryMessage := spine.Message{ID: "message-recovery", JobID: jobID, Sequence: 3, Intent: spine.MessageFollow}
	failedRun := spine.AgentRun{ID: "agent-run-failed", JobID: jobID, MessageID: failedMessage.ID, Role: "implement", InputRevision: revision, State: spine.AgentRunFailed, TurnOutcome: "failed"}
	recoveryRun := spine.AgentRun{ID: "agent-run-recovery", JobID: jobID, MessageID: recoveryMessage.ID, Role: "implement", InputRevision: revision, State: spine.AgentRunCompleted, TurnOutcome: "completed"}
	recoveryEvidence := gitObservationEvidence(t, store, job, recoveryRun, time.Now().UTC().Truncate(time.Microsecond))
	recovered := AssessReviewReadiness(job, []spine.Evidence{recoveryEvidence}, store, &noReview, nil, []spine.Delivery{{Message: failedMessage, AgentRun: failedRun}, {Message: recoveryMessage, AgentRun: recoveryRun}})
	if !recovered.Ready {
		t.Fatalf("later successful observed Follow did not recover old failure: %#v", recovered)
	}
	selected := ReviewPlanRecord{JobID: jobID, Revision: revision, Plan: policy.ReviewPlan{Decision: "selected", Roles: []policy.Role{policy.RoleCriticalBoundary}}}
	incomplete := AssessReviewReadiness(job, nil, store, &selected, nil, nil)
	if incomplete.Ready || !strings.Contains(incomplete.Reason, "has not returned a feedback Message") {
		t.Fatalf("incomplete selected readiness=%#v", incomplete)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	requestFromID := ReviewRequestFromID(revision, string(policy.RoleCriticalBoundary))
	requestID := ReviewRequestMessageID(jobID, revision, string(policy.RoleCriticalBoundary))
	runID := spine.AgentRunID(requestID)
	feedbackID := spine.MessageID(jobID, spine.MessageFromAgent, runID)
	run := ReviewRunView{
		AgentRun: spine.AgentRun{ID: runID, JobID: jobID, MessageID: requestID, InputRevision: revision, Role: string(policy.RoleCriticalBoundary), State: spine.AgentRunCompleted, TurnOutcome: "completed", TurnID: "turn-review", Harness: "codex", ThreadID: "thread-review", Capability: ReviewReadOnlyCapability, StartedAt: now, FinishedAt: now.Add(time.Second)},
		Request:  spine.Message{ID: requestID, JobID: jobID, FromKind: spine.MessageFromWorkflow, FromID: requestFromID, Sequence: 2, Input: "Review the exact Revision.", Intent: spine.MessageFollow},
	}
	observed := reviewObservationEvidence(t, store, run, ReviewCheckoutObservation{Revision: revision, Tree: strings.Repeat("c", 40)})
	foreign := spine.Message{ID: feedbackID, JobID: jobID, FromKind: spine.MessageFromAgent, FromID: "agent-run-foreign", Sequence: 3, Input: "Foreign feedback.", Intent: spine.MessageFollow}
	wrong := AssessReviewReadiness(job, []spine.Evidence{observed}, store, &selected, []ReviewRunView{run}, []spine.Delivery{{Message: foreign}})
	if wrong.Ready || !strings.Contains(wrong.Reason, "has not returned a feedback Message") {
		t.Fatalf("foreign feedback Message satisfied readiness: %#v", wrong)
	}
	feedback := spine.Message{ID: feedbackID, JobID: jobID, FromKind: spine.MessageFromAgent, FromID: runID, Sequence: 3, Input: "Consider simplifying the boundary.", Intent: spine.MessageFollow}
	implementation := spine.AgentRun{ID: spine.AgentRunID(feedback.ID), JobID: jobID, MessageID: feedback.ID, Role: "implement", InputRevision: revision, State: spine.AgentRunCompleted, TurnOutcome: "completed"}
	handled := AssessReviewReadiness(job, []spine.Evidence{observed}, store, &selected, []ReviewRunView{run}, []spine.Delivery{{Message: feedback, AgentRun: implementation}})
	if handled.Ready || !strings.Contains(handled.Reason, "no valid Git observation") {
		t.Fatalf("feedback without Git observation satisfied readiness: %#v", handled)
	}
	gitEvidence := gitObservationEvidence(t, store, job, implementation, now.Add(2*time.Second))
	stale := run
	stale.ID = "zz-stale-same-role-run"
	stale.InputRevision = strings.Repeat("f", 40)
	stale.State = spine.AgentRunFailed
	settled := AssessReviewReadiness(job, []spine.Evidence{observed, gitEvidence}, store, &selected, []ReviewRunView{run, stale}, []spine.Delivery{{Message: feedback, AgentRun: implementation}})
	if !settled.Ready || !strings.Contains(settled.Reason, "returned feedback") {
		t.Fatalf("stale same-Role review blocked exact-Revision readiness=%#v", settled)
	}
}

func TestReviewReadinessUsesTerminalTargetSteerFallbackAsLatestTurnStart(t *testing.T) {
	store, jobID, revision := readinessFixture(t)
	job := Job{Job: spine.Job{ID: jobID}, Revision: revision, Branch: "dorf/readiness"}
	plan := ReviewPlanRecord{JobID: jobID, Revision: revision, Plan: policy.ReviewPlan{Decision: "no-review"}}
	message := spine.Message{
		ID: "message-fallback", JobID: jobID, Sequence: 2,
		Intent: spine.MessageSteer, TargetTurnID: "turn-old",
	}
	run := spine.AgentRun{
		ID: "run-fallback", JobID: jobID, MessageID: message.ID, Role: "implement",
		InputRevision: revision, State: spine.AgentRunCompleted, TurnID: "turn-new", TurnOutcome: "completed",
	}
	observed := gitObservationEvidence(t, store, job, run, time.Now().UTC().Truncate(time.Microsecond))
	ready := AssessReviewReadiness(job, []spine.Evidence{observed}, store, &plan, nil, []spine.Delivery{{Message: message, AgentRun: run}})
	if !ready.Ready {
		t.Fatalf("observed terminal-target steer fallback was not ready: %#v", ready)
	}

	missing := AssessReviewReadiness(job, nil, store, &plan, nil, []spine.Delivery{{Message: message, AgentRun: run}})
	if missing.Ready || !strings.Contains(missing.Reason, "no valid Git observation") {
		t.Fatalf("unobserved terminal-target steer fallback satisfied readiness: %#v", missing)
	}
	run.State, run.TurnID, run.TurnOutcome = spine.AgentRunFailed, "", "failed"
	failed := AssessReviewReadiness(job, nil, store, &plan, nil, []spine.Delivery{{Message: message, AgentRun: run}})
	if failed.Ready || !strings.Contains(failed.Reason, "has not completed successfully") {
		t.Fatalf("failed terminal-target steer fallback satisfied readiness: %#v", failed)
	}
}

func gitObservationEvidence(t *testing.T, store blob.Store, job Job, run spine.AgentRun, started time.Time) spine.Evidence {
	t.Helper()
	observation := gitworkspace.Observation{
		ComparisonBase: run.InputRevision, Revision: job.Revision, Tree: strings.Repeat("d", 40), Branch: job.Branch,
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
	return spine.Evidence{
		ID: spine.EvidenceID(run.ID, "git-revision"), Digest: blob.Digest, ByteSize: blob.ByteSize,
		MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: "git-revision",
		AgentRunID: run.ID, Revision: job.Revision, StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt,
	}
}

func reviewObservationEvidence(t *testing.T, store blob.Store, run ReviewRunView, checkout ReviewCheckoutObservation) spine.Evidence {
	t.Helper()
	contents, err := json.Marshal(reviewObservationArtifact{
		AgentRunID: run.ID, Revision: run.InputRevision, Role: run.Role, Capability: run.Capability,
		Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.TurnOutcome, Checkout: checkout,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Put(contents)
	if err != nil {
		t.Fatal(err)
	}
	return spine.Evidence{
		ID: spine.EvidenceID(run.ID, "review-observation"), Digest: stored.Digest, ByteSize: stored.ByteSize,
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
