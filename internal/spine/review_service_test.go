package spine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
)

func TestLostClaimCannotRecordReviewFeedback(t *testing.T) {
	claimLost := errors.New("claim lost")
	service := Service{claimCheck: func(context.Context) error { return claimLost }}
	if _, _, err := service.recordReviewFeedback(context.Background(), "run-1", HarnessTurn{}, Evidence{}); !errors.Is(err, claimLost) {
		t.Fatalf("record review feedback error = %v", err)
	}
}

func TestReviewRunsRejectsBrokenFactRelationships(t *testing.T) {
	request := Message{ID: "message-review", JobID: "job-1"}
	run := AgentRun{
		ID: "run-review", JobID: request.JobID, MessageID: request.ID,
		Role: "general", InputRevision: "revision-1", Capability: ReviewReadOnlyCapability,
		SandboxID: "sandbox-review",
	}
	if _, err := ReviewRuns([]Delivery{{Message: request, AgentRun: run}}, nil); err == nil || !strings.Contains(err.Error(), "Job-owned Sandbox") {
		t.Fatalf("missing reviewer Sandbox error = %v", err)
	}
	foreign := Sandbox{ID: run.SandboxID, JobID: "job-2"}
	if _, err := ReviewRuns([]Delivery{{Message: request, AgentRun: run}}, []Sandbox{foreign}); err == nil || !strings.Contains(err.Error(), "Job-owned Sandbox") {
		t.Fatalf("foreign reviewer Sandbox error = %v", err)
	}
	run.MessageID = "another-message"
	if _, err := ReviewRuns([]Delivery{{Message: request, AgentRun: run}}, []Sandbox{{ID: run.SandboxID, JobID: run.JobID}}); err == nil || !strings.Contains(err.Error(), "input Message relationship") {
		t.Fatalf("mismatched review request error = %v", err)
	}
}

func TestLostClaimCannotRecordReviewPolicy(t *testing.T) {
	claimLost := errors.New("claim lost")
	service := Service{claimCheck: func(context.Context) error { return claimLost }}
	if err := service.recordReviewPolicy(context.Background(), ReviewPlanRecord{}); !errors.Is(err, claimLost) {
		t.Fatalf("record review policy error = %v", err)
	}
}

func TestReviewHarnessControllerMustMatchDerivedOwner(t *testing.T) {
	expected := ReviewControllerID("run-1", "sandbox-1", "owner-nonce")
	if err := validateReviewController(expected, HarnessBinding{ControllerID: expected}); err != nil {
		t.Fatal(err)
	}
	err := validateReviewController(expected, HarnessBinding{ControllerID: "foreign"})
	if err == nil || !attentionNeeded(err) {
		t.Fatalf("foreign controller error = %v", err)
	}
	var boundary reviewBoundaryError
	if !errors.As(err, &boundary) {
		t.Fatalf("foreign controller error type = %T", err)
	}
}

func TestReviewEvidenceObservesAgentRunAndExactCheckoutTreeWithoutCopyingFeedback(t *testing.T) {
	blobs := evidence.Store{Root: t.TempDir()}
	now := time.Now().UTC().Truncate(time.Microsecond)
	revision := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	post := ReviewCheckoutObservation{Revision: revision, Tree: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	run := ReviewRunView{AgentRun: AgentRun{
		ID: "agent-run-review", JobID: "job-1", InputRevision: revision, Role: "critical-boundary",
		Capability: ReviewReadOnlyCapability, Harness: "codex", ThreadID: "thread-1", TurnID: "turn-1",
		TurnOutcome: "completed", State: AgentRunCompleted, StartedAt: now, FinishedAt: now.Add(time.Second),
	}}

	record, err := (Service{evidence: blobs}).reviewEvidence(run, post)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != EvidenceID(run.ID, "review-observation") || record.AgentRunID != run.ID || record.ActionID != "" || record.CheckID != "" {
		t.Fatalf("review Evidence metadata = %#v", record)
	}
	contents, err := blobs.ReadVerified(record.Digest, record.ByteSize)
	if err != nil {
		t.Fatal(err)
	}
	var artifact reviewObservationArtifact
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatal(err)
	}
	want := reviewObservationArtifact{AgentRunID: run.ID, Revision: run.InputRevision, Role: run.Role, Capability: run.Capability, Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.TurnOutcome, Checkout: post}
	if artifact != want {
		t.Fatalf("review observation = %#v, want %#v", artifact, want)
	}
}
