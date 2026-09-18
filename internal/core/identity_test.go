package core

import (
	"testing"
)

func TestStableIdentitiesDoNotContainGoalOrSecrets(t *testing.T) {
	sessionA := SessionID("client-request-40")
	sessionB := SessionID("client-request-40")
	if sessionA != sessionB || sessionA == "" {
		t.Fatalf("session identity is not stable: %q != %q", sessionA, sessionB)
	}
	if ActionID(sessionA, ActionSandboxCreate) != ActionID(sessionA, ActionSandboxCreate) {
		t.Fatal("Sandbox Action identity is not stable")
	}
	if ActionID(sessionA, ActionSandboxCreate) == ActionID(sessionA, ActionRouteCreate) {
		t.Fatal("different effects share an Action identity")
	}
	messageA := MessageID(sessionA, MessageFromHuman, "caller-a")
	messageB := MessageID(sessionA, MessageFromHuman, "caller-b")
	if messageA == messageB || AgentRunID(messageA) == AgentRunID(messageB) {
		t.Fatal("distinct logical inputs share delivery identities")
	}
	if messageA == MessageID(sessionA, MessageFromWorkflow, "caller-a") {
		t.Fatal("different senders share a Message identity")
	}
	if AgentRunID(messageA) != AgentRunID(messageA) {
		t.Fatal("per-input AgentRun identity is not stable")
	}
}
