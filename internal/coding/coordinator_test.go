package coding

import (
	"encoding/json"
	"testing"
)

func TestPersistedFactStepResultV1(t *testing.T) {
	encoded, err := json.Marshal(FactStepResultV1{FactID: "action-1"})
	if err != nil || string(encoded) != `{"fact_id":"action-1"}` {
		t.Fatalf("persisted JSON = %s, want exact v1 shape: %v", encoded, err)
	}
}

func TestStepNamesComeFromDurableFactIdentity(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"AgentRun", agentRunStepName("run-1"), "dorf/agent-run/v1/run-1"},
		{"Revision", revisionStepName("run-1"), "dorf/revision/v1/run-1"},
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
