package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/spine"
)

func TestPersistedWorkflowContractsV1(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"task result", TaskResultV1{JobID: "job-1", Outcome: "accepted"}, `{"job_id":"job-1","outcome":"accepted"}`},
		{"wake", WakeV1{JobID: "job-1", Sequence: 2}, `{"job_id":"job-1","sequence":2}`},
		{"fact step result", FactStepResultV1{FactID: "action-1"}, `{"fact_id":"action-1"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil || string(encoded) != test.want {
				t.Fatalf("persisted JSON = %s, want %s: %v", encoded, test.want, err)
			}
		})
	}
}

func TestWakeEventIsStableAndFIFOScoped(t *testing.T) {
	if WakeEvent("job-a", 2) != WakeEvent("job-a", 2) {
		t.Fatal("same admitted FIFO position did not retain its wake identity")
	}
	if WakeEvent("job-a", 2) == WakeEvent("job-b", 2) {
		t.Fatal("distinct Jobs share a wake event")
	}
	if WakeEvent("job-a", 2) == WakeEvent("job-a", 3) {
		t.Fatal("distinct FIFO positions share an immutable Absurd event")
	}
}

func TestStepNamesComeFromDurableFactIdentity(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"Action", actionStepName("action-1"), "dorf/action/v1/action-1"},
		{"setup", setupStepName("setup-1"), "dorf/setup/v1/setup-1"},
		{"AgentRun", agentRunStepName("run-1"), "dorf/agent-run/v1/run-1"},
		{"Revision", revisionStepName("run-1"), "dorf/revision/v1/run-1"},
		{"Check", checkStepName("check-1"), "dorf/check/v1/check-1"},
		{"ReviewPolicy", reviewPolicyStepName("job-1", "revision-1"), "dorf/review-policy/v1/job-1/revision-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("Step name %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestCleanupRejectsUnsettledPullRequestAction(t *testing.T) {
	err := unresolvedPullRequestAction(spine.Action{State: spine.ActionUnsettled})
	if err == nil || !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("unsettled pull-request Action cleanup error = %v", err)
	}
}
