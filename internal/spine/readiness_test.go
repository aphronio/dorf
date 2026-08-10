package spine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
	policy "github.com/aphronio/dorf/internal/review"
)

func TestRevisionReadinessRejectsMissingTamperedAndRowMismatchedEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, evidence.Store, *Check, *Evidence)
		want   string
	}{
		{
			name: "missing blob",
			mutate: func(t *testing.T, store evidence.Store, _ *Check, record *Evidence) {
				t.Helper()
				if err := os.Remove(evidencePath(store.Root, record.Digest)); err != nil {
					t.Fatal(err)
				}
			},
			want: "unavailable or invalid",
		},
		{
			name: "tampered blob",
			mutate: func(t *testing.T, store evidence.Store, _ *Check, record *Evidence) {
				t.Helper()
				path := evidencePath(store.Root, record.Digest)
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "independent rehash",
		},
		{
			name: "blob versus row",
			mutate: func(t *testing.T, store evidence.Store, check *Check, record *Evidence) {
				t.Helper()
				observation := CommandObservation{Command: "go vet ./...", StartedAt: check.StartedAt, FinishedAt: check.FinishedAt}
				contents, err := commandArtifact(check.ID, check.Revision, observation)
				if err != nil {
					t.Fatal(err)
				}
				blob, err := store.Put(contents)
				if err != nil {
					t.Fatal(err)
				}
				record.Digest, record.ByteSize = blob.Digest, blob.ByteSize
			},
			want: "observation artifact facts",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, jobID, revision, declared, check, record := readinessFixture(t)
			test.mutate(t, store, &check, &record)
			results, err := VerifyRevisionEvidence(jobID, revision, declared, []Check{check}, []Evidence{record}, store)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(results) != 1 || results[0].Verified {
				t.Fatalf("results=%#v err=%v want=%q", results, err, test.want)
			}
		})
	}
}

func TestReviewReadinessRequiresExplicitDecisionAndSettledSelectedRuns(t *testing.T) {
	store, jobID, revision, declared, check, record := readinessFixture(t)
	job := Job{ID: jobID, Revision: revision, Branch: "dorf/readiness"}
	withoutPlan := AssessReviewReadiness(job, declared, []Check{check}, []Evidence{record}, store, nil, nil, nil, nil)
	if withoutPlan.Ready || !strings.Contains(withoutPlan.Reason, "no explicit persisted") {
		t.Fatalf("missing plan readiness=%#v", withoutPlan)
	}
	noReview := ReviewPlanRecord{JobID: jobID, Revision: revision, Plan: policy.ReviewPlan{Decision: "no-review"}}
	explicit := AssessReviewReadiness(job, declared, []Check{check}, []Evidence{record}, store, &noReview, nil, nil, nil)
	if !explicit.Ready || !strings.Contains(explicit.Reason, "explicitly selected no agent review") {
		t.Fatalf("explicit no-review readiness=%#v", explicit)
	}
	pending := AgentRun{ID: "agent-run-pending", JobID: jobID, MessageID: "message-pending", Role: "implement", State: AgentRunPending}
	lateInput := AssessReviewReadiness(job, declared, []Check{check}, []Evidence{record}, store, &noReview, nil, nil, []AgentRun{pending})
	if lateInput.Ready || !strings.Contains(lateInput.Reason, "not terminal") {
		t.Fatalf("late input satisfied readiness: %#v", lateInput)
	}
	failedMessage := MessageView{Message: Message{ID: "message-failed", JobID: jobID, Sequence: 2, Intent: MessageFollow}}
	recoveryMessage := MessageView{Message: Message{ID: "message-recovery", JobID: jobID, Sequence: 3, Intent: MessageFollow}}
	failedRun := AgentRun{ID: "agent-run-failed", JobID: jobID, MessageID: failedMessage.ID, Role: "implement", InputRevision: revision, State: AgentRunFailed, TurnOutcome: "failed"}
	recoveryRun := AgentRun{ID: "agent-run-recovery", JobID: jobID, MessageID: recoveryMessage.ID, Role: "implement", InputRevision: revision, State: AgentRunCompleted, TurnOutcome: "completed"}
	recoveryEvidence := gitObservationEvidence(t, store, job, recoveryRun, time.Now().UTC().Truncate(time.Microsecond))
	recovered := AssessReviewReadiness(job, declared, []Check{check}, []Evidence{record, recoveryEvidence}, store, &noReview, nil, []MessageView{failedMessage, recoveryMessage}, []AgentRun{failedRun, recoveryRun})
	if !recovered.Ready {
		t.Fatalf("later successful observed Follow did not recover old failure: %#v", recovered)
	}
	selected := ReviewPlanRecord{JobID: jobID, Revision: revision, Plan: policy.ReviewPlan{Decision: "selected", Roles: []policy.Role{policy.RoleCriticalBoundary}}}
	incomplete := AssessReviewReadiness(job, declared, []Check{check}, []Evidence{record}, store, &selected, nil, nil, nil)
	if incomplete.Ready || !strings.Contains(incomplete.Reason, "has not returned a feedback Message") {
		t.Fatalf("incomplete selected readiness=%#v", incomplete)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	requestFromID := ReviewRequestFromID(revision, string(policy.RoleCriticalBoundary))
	requestID := ReviewRequestMessageID(jobID, revision, string(policy.RoleCriticalBoundary))
	runID := AgentRunID(requestID)
	feedbackID := MessageID(jobID, MessageFromAgent, runID)
	run := ReviewRunView{
		AgentRun: AgentRun{ID: runID, JobID: jobID, MessageID: requestID, InputRevision: revision, Role: string(policy.RoleCriticalBoundary), State: AgentRunCompleted, TurnOutcome: "completed", TurnID: "turn-review", Harness: "codex", ThreadID: "thread-review", Capability: ReviewReadOnlyCapability, StartedAt: now, FinishedAt: now.Add(time.Second)},
		Request:  Message{ID: requestID, JobID: jobID, FromKind: MessageFromWorkflow, FromID: requestFromID, Sequence: 2, Input: "Review the exact Revision.", Intent: MessageFollow},
	}
	observed, err := (Service{Evidence: store}).reviewEvidence(run, ReviewCheckoutObservation{Revision: revision, Tree: strings.Repeat("c", 40)})
	if err != nil {
		t.Fatal(err)
	}
	foreign := MessageView{Message: Message{ID: feedbackID, JobID: jobID, FromKind: MessageFromAgent, FromID: "agent-run-foreign", Sequence: 3, Input: "Foreign feedback.", Intent: MessageFollow}}
	wrong := AssessReviewReadiness(job, declared, []Check{check}, []Evidence{record, observed}, store, &selected, []ReviewRunView{run}, []MessageView{foreign}, nil)
	if wrong.Ready || !strings.Contains(wrong.Reason, "has not returned a feedback Message") {
		t.Fatalf("foreign feedback Message satisfied readiness: %#v", wrong)
	}
	feedback := MessageView{Message: Message{ID: feedbackID, JobID: jobID, FromKind: MessageFromAgent, FromID: runID, Sequence: 3, Input: "Consider simplifying the boundary.", Intent: MessageFollow}}
	implementation := AgentRun{ID: AgentRunID(feedback.ID), JobID: jobID, MessageID: feedback.ID, Role: "implement", InputRevision: revision, State: AgentRunCompleted, TurnOutcome: "completed"}
	handled := AssessReviewReadiness(job, declared, []Check{check}, []Evidence{record, observed}, store, &selected, []ReviewRunView{run}, []MessageView{feedback}, []AgentRun{implementation})
	if handled.Ready || !strings.Contains(handled.Reason, "no valid Git observation") {
		t.Fatalf("feedback without Git observation satisfied readiness: %#v", handled)
	}
	gitEvidence := gitObservationEvidence(t, store, job, implementation, now.Add(2*time.Second))
	settled := AssessReviewReadiness(job, declared, []Check{check}, []Evidence{record, observed, gitEvidence}, store, &selected, []ReviewRunView{run}, []MessageView{feedback}, []AgentRun{implementation})
	if !settled.Ready || !strings.Contains(settled.Reason, "returned feedback") {
		t.Fatalf("settled selected readiness=%#v", settled)
	}
}

func gitObservationEvidence(t *testing.T, store evidence.Store, job Job, run AgentRun, started time.Time) Evidence {
	t.Helper()
	observation := RevisionObservation{
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
	return Evidence{
		ID: EvidenceID(run.ID, "git-revision"), Digest: blob.Digest, ByteSize: blob.ByteSize,
		MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: "git-revision",
		AgentRunID: run.ID, Revision: job.Revision, StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt,
	}
}

func readinessFixture(t *testing.T) (evidence.Store, string, string, []DeclaredCheck, Check, Evidence) {
	t.Helper()
	store := evidence.Store{Root: t.TempDir()}
	jobID, revision := "job-readiness", strings.Repeat("a", 40)
	declared := []DeclaredCheck{{Name: "check", Command: "go test ./..."}}
	checkID := CheckID(jobID, revision, "check")
	now := time.Now().UTC().Truncate(time.Microsecond)
	observation := CommandObservation{Command: declared[0].Command, StartedAt: now, FinishedAt: now.Add(time.Second)}
	contents, err := commandArtifact(checkID, revision, observation)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Put(contents)
	if err != nil {
		t.Fatal(err)
	}
	record := Evidence{ID: EvidenceID(checkID, "check-output"), Digest: blob.Digest, ByteSize: blob.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: "check-output", CheckID: checkID, Revision: revision, StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt}
	check := Check{ID: checkID, JobID: jobID, Name: declared[0].Name, Command: declared[0].Command, Revision: revision, State: "passed", EvidenceID: record.ID, StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt}
	return store, jobID, revision, declared, check, record
}

func evidencePath(root, digest string) string {
	return filepath.Join(root, "sha256", digest[:2], digest[2:])
}
