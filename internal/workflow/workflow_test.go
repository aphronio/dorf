package workflow

import "testing"

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
		{"verification", verifyStepName("job-1", "revision-1"), "dorf/checks-verified/v1/job-1/revision-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("Step name %q, want %q", test.got, test.want)
			}
		})
	}
}
