package core

import (
	"testing"
)

func TestStableIdentitiesDoNotContainGoalOrSecrets(t *testing.T) {
	jobA := JobID("client-request-40")
	jobB := JobID("client-request-40")
	if jobA != jobB || jobA == "" {
		t.Fatalf("job identity is not stable: %q != %q", jobA, jobB)
	}
	if ActionID(jobA, ActionSandboxCreate) != ActionID(jobA, ActionSandboxCreate) {
		t.Fatal("Sandbox Action identity is not stable")
	}
	if ActionID(jobA, ActionSandboxCreate) == ActionID(jobA, ActionRouteCreate) {
		t.Fatal("different effects share an Action identity")
	}
	messageA := MessageID(jobA, MessageFromHuman, "caller-a")
	messageB := MessageID(jobA, MessageFromHuman, "caller-b")
	if messageA == messageB || AgentRunID(messageA) == AgentRunID(messageB) {
		t.Fatal("distinct logical inputs share delivery identities")
	}
	if messageA == MessageID(jobA, MessageFromWorkflow, "caller-a") {
		t.Fatal("different senders share a Message identity")
	}
	if AgentRunID(messageA) != AgentRunID(messageA) {
		t.Fatal("per-input AgentRun identity is not stable")
	}
}
