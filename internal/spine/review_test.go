package spine

import (
	"strings"
	"testing"
)

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
