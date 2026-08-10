package spine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
)

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

func TestReviewEvidenceObservesTheAgentRunWithoutCopyingFeedback(t *testing.T) {
	blobs := evidence.Store{Root: t.TempDir()}
	now := time.Now().UTC().Truncate(time.Microsecond)
	run := ReviewRunView{AgentRun: AgentRun{
		ID: "agent-run-review", JobID: "job-1", Revision: "revision-1", Role: "critical-boundary",
		Capability: ReviewReadOnlyCapability, Harness: "codex", ThreadID: "thread-1", TurnID: "turn-1",
		TurnOutcome: "completed", State: AgentRunCompleted, StartedAt: now, FinishedAt: now.Add(time.Second),
	}}

	record, err := (Service{Evidence: blobs}).reviewEvidence(run)
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
	want := reviewObservationArtifact{AgentRunID: run.ID, Revision: run.Revision, Role: run.Role, Capability: run.Capability, Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.TurnOutcome}
	if artifact != want {
		t.Fatalf("review observation = %#v, want %#v", artifact, want)
	}
	if string(contents) == "No material issue found" {
		t.Fatal("review feedback belongs in Message, not observed Evidence")
	}
}
