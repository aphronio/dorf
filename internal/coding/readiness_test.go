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
	pending := core.AgentRun{ID: "agent-run-pending", JobID: jobID, MessageID: "message-pending", Role: "implement", State: core.AgentRunPending}
	lateInput := AssessReviewReadiness(job, nil, store, &noReview, nil, []core.Delivery{{Message: core.Message{ID: pending.MessageID}, AgentRun: pending}})
	if lateInput.Ready || !strings.Contains(lateInput.Reason, "not terminal") {
		t.Fatalf("late input satisfied readiness: %#v", lateInput)
	}
	failedMessage := core.Message{ID: "message-failed", JobID: jobID, Sequence: 2, Intent: core.MessageFollow}
	recoveryMessage := core.Message{ID: "message-recovery", JobID: jobID, Sequence: 3, Intent: core.MessageFollow}
	failedRun := core.AgentRun{ID: "agent-run-failed", JobID: jobID, MessageID: failedMessage.ID, Role: "implement", InputRevision: revision, State: core.AgentRunFailed, TurnOutcome: "failed"}
	recoveryRun := core.AgentRun{ID: "agent-run-recovery", JobID: jobID, MessageID: recoveryMessage.ID, Role: "implement", InputRevision: revision, State: core.AgentRunCompleted, TurnOutcome: "completed"}
	recoveryEvidence := gitObservationEvidence(t, store, job, recoveryRun, time.Now().UTC().Truncate(time.Microsecond))
	recovered := AssessReviewReadiness(job, []core.Evidence{recoveryEvidence}, store, &noReview, nil, []core.Delivery{{Message: failedMessage, AgentRun: failedRun}, {Message: recoveryMessage, AgentRun: recoveryRun}})
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
	runID := core.AgentRunID(requestID)
	feedbackID := core.MessageID(jobID, core.MessageFromAgent, runID)
	run := ReviewRunView{
		AgentRun: core.AgentRun{ID: runID, JobID: jobID, MessageID: requestID, InputRevision: revision, Role: string(policy.RoleCriticalBoundary), State: core.AgentRunCompleted, TurnOutcome: "completed", TurnID: "turn-review", Harness: "codex", ThreadID: "thread-review", Capability: ReviewReadOnlyCapability, StartedAt: now, FinishedAt: now.Add(time.Second)},
		Request:  core.Message{ID: requestID, JobID: jobID, FromKind: core.MessageFromWorkflow, FromID: requestFromID, Sequence: 2, Input: "Review the exact Revision.", Intent: core.MessageFollow},
	}
	observed := reviewObservationEvidence(t, store, run, ReviewCheckoutObservation{Revision: revision, Tree: strings.Repeat("c", 40)})
	foreign := core.Message{ID: feedbackID, JobID: jobID, FromKind: core.MessageFromAgent, FromID: "agent-run-foreign", Sequence: 3, Input: "Foreign feedback.", Intent: core.MessageFollow}
	wrong := AssessReviewReadiness(job, []core.Evidence{observed}, store, &selected, []ReviewRunView{run}, []core.Delivery{{Message: foreign}})
	if wrong.Ready || !strings.Contains(wrong.Reason, "has not returned a feedback Message") {
		t.Fatalf("foreign feedback Message satisfied readiness: %#v", wrong)
	}
	feedback := core.Message{ID: feedbackID, JobID: jobID, FromKind: core.MessageFromAgent, FromID: runID, Sequence: 3, Input: "Consider simplifying the boundary.", Intent: core.MessageFollow}
	implementation := core.AgentRun{ID: core.AgentRunID(feedback.ID), JobID: jobID, MessageID: feedback.ID, Role: "implement", InputRevision: revision, State: core.AgentRunCompleted, TurnOutcome: "completed"}
	handled := AssessReviewReadiness(job, []core.Evidence{observed}, store, &selected, []ReviewRunView{run}, []core.Delivery{{Message: feedback, AgentRun: implementation}})
	if handled.Ready || !strings.Contains(handled.Reason, "no valid Git observation") {
		t.Fatalf("feedback without Git observation satisfied readiness: %#v", handled)
	}
	gitEvidence := gitObservationEvidence(t, store, job, implementation, now.Add(2*time.Second))
	stale := run
	stale.ID = "zz-stale-same-role-run"
	stale.InputRevision = strings.Repeat("f", 40)
	stale.State = core.AgentRunFailed
	settled := AssessReviewReadiness(job, []core.Evidence{observed, gitEvidence}, store, &selected, []ReviewRunView{run, stale}, []core.Delivery{{Message: feedback, AgentRun: implementation}})
	if !settled.Ready || !strings.Contains(settled.Reason, "returned feedback") {
		t.Fatalf("stale same-Role review blocked exact-Revision readiness=%#v", settled)
	}
}

func TestReviewReadinessUsesTerminalTargetSteerFallbackAsLatestTurnStart(t *testing.T) {
	store, jobID, revision := readinessFixture(t)
	job := Job{Job: core.Job{ID: jobID}, Revision: revision, Branch: "dorf/readiness"}
	plan := ReviewPlanRecord{JobID: jobID, Revision: revision, Plan: policy.ReviewPlan{Decision: "no-review"}}
	message := core.Message{
		ID: "message-fallback", JobID: jobID, Sequence: 2,
		Intent: core.MessageSteer, TargetTurnID: "turn-old",
	}
	run := core.AgentRun{
		ID: "run-fallback", JobID: jobID, MessageID: message.ID, Role: "implement",
		InputRevision: revision, State: core.AgentRunCompleted, TurnID: "turn-new", TurnOutcome: "completed",
	}
	observed := gitObservationEvidence(t, store, job, run, time.Now().UTC().Truncate(time.Microsecond))
	ready := AssessReviewReadiness(job, []core.Evidence{observed}, store, &plan, nil, []core.Delivery{{Message: message, AgentRun: run}})
	if !ready.Ready {
		t.Fatalf("observed terminal-target steer fallback was not ready: %#v", ready)
	}

	missing := AssessReviewReadiness(job, nil, store, &plan, nil, []core.Delivery{{Message: message, AgentRun: run}})
	if missing.Ready || !strings.Contains(missing.Reason, "no valid Git observation") {
		t.Fatalf("unobserved terminal-target steer fallback satisfied readiness: %#v", missing)
	}
	run.State, run.TurnID, run.TurnOutcome = core.AgentRunFailed, "", "failed"
	failed := AssessReviewReadiness(job, nil, store, &plan, nil, []core.Delivery{{Message: message, AgentRun: run}})
	if failed.Ready || !strings.Contains(failed.Reason, "has not completed successfully") {
		t.Fatalf("failed terminal-target steer fallback satisfied readiness: %#v", failed)
	}
}

func gitObservationEvidence(t *testing.T, store blob.Store, job Job, run core.AgentRun, started time.Time) core.Evidence {
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
	return core.Evidence{
		ID: core.EvidenceID(run.ID, "git-revision"), Digest: blob.Digest, ByteSize: blob.ByteSize,
		MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: "git-revision",
		AgentRunID: run.ID, Revision: job.Revision, StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt,
	}
}

func reviewObservationEvidence(t *testing.T, store blob.Store, run ReviewRunView, checkout ReviewCheckoutObservation) core.Evidence {
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
