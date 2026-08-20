package coding

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
)

func TestLostClaimCannotRecordReviewFeedback(t *testing.T) {
	claimLost := errors.New("claim lost")
	service := Service{claimCheck: func(context.Context) error { return claimLost }}
	if _, _, err := service.recordReviewFeedback(context.Background(), "run-1", core.HarnessTurn{}, core.Evidence{}); !errors.Is(err, claimLost) {
		t.Fatalf("record review feedback error = %v", err)
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
	if err := validateReviewController(expected, core.HarnessBinding{ControllerID: expected}); err != nil {
		t.Fatal(err)
	}
	err := validateReviewController(expected, core.HarnessBinding{ControllerID: "foreign"})
	if err == nil || !attentionNeeded(err) {
		t.Fatalf("foreign controller error = %v", err)
	}
	var boundary reviewBoundaryError
	if !errors.As(err, &boundary) {
		t.Fatalf("foreign controller error type = %T", err)
	}
}

func TestReviewEvidenceObservesAgentRunAndExactCheckoutTreeWithoutCopyingFeedback(t *testing.T) {
	blobs := blob.Store{Root: t.TempDir()}
	now := time.Now().UTC().Truncate(time.Microsecond)
	revision := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	post := ReviewCheckoutObservation{Revision: revision, Tree: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	run := ReviewRunView{AgentRun: core.AgentRun{
		ID: "agent-run-review", JobID: "job-1", InputRevision: revision, Role: "critical-boundary",
		Capability: ReviewReadOnlyCapability, Harness: "codex", ThreadID: "thread-1", TurnID: "turn-1",
		TurnOutcome: "completed", State: core.AgentRunCompleted, StartedAt: now, FinishedAt: now.Add(time.Second),
	}}

	record, err := (Service{blobs: blobs}).reviewEvidence(run, post)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != core.EvidenceID(run.ID, "review-observation") || record.AgentRunID != run.ID || record.ActionID != "" {
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
