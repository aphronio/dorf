package coding

import (
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestReviewRunsRejectsBrokenFactRelationships(t *testing.T) {
	request := core.Message{ID: "message-review", JobID: "job-1"}
	run := core.AgentRun{
		ID: "run-review", JobID: request.JobID, MessageID: request.ID,
		Role: "general", InputRevision: "revision-1", Capability: ReviewReadOnlyCapability,
		SandboxID: "sandbox-review",
	}
	if _, err := ReviewRuns([]core.Delivery{{Message: request, AgentRun: run}}, nil); err == nil || !strings.Contains(err.Error(), "Job-owned Sandbox") {
		t.Fatalf("missing reviewer Sandbox error = %v", err)
	}
	foreign := core.Sandbox{ID: run.SandboxID, JobID: "job-2"}
	if _, err := ReviewRuns([]core.Delivery{{Message: request, AgentRun: run}}, []core.Sandbox{foreign}); err == nil || !strings.Contains(err.Error(), "Job-owned Sandbox") {
		t.Fatalf("foreign reviewer Sandbox error = %v", err)
	}
	run.MessageID = "another-message"
	if _, err := ReviewRuns([]core.Delivery{{Message: request, AgentRun: run}}, []core.Sandbox{{ID: run.SandboxID, JobID: run.JobID}}); err == nil || !strings.Contains(err.Error(), "input Message relationship") {
		t.Fatalf("mismatched review request error = %v", err)
	}
}
