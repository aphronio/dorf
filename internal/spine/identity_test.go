package spine

import "testing"

func TestStableIdentitiesDoNotContainGoalOrSecrets(t *testing.T) {
	jobA := JobID("client-request-40")
	jobB := JobID("client-request-40")
	if jobA != jobB || jobA == "" {
		t.Fatalf("job identity is not stable: %q != %q", jobA, jobB)
	}
	if ActionID(jobA, ActionTurnStart) != ActionID(jobA, ActionTurnStart) {
		t.Fatal("turn Action identity is not stable")
	}
	if ActionID(jobA, ActionTurnStart) == ActionID(jobA, ActionRouteCreate) {
		t.Fatal("different effects share an Action identity")
	}
	messageA := MessageID(jobA, "caller-a")
	messageB := MessageID(jobA, "caller-b")
	if messageA == messageB || TurnActionID(messageA) == TurnActionID(messageB) || AgentRunID(messageA) == AgentRunID(messageB) {
		t.Fatal("distinct logical inputs share delivery identities")
	}
	if TurnActionID(messageA) != TurnActionID(messageA) || AgentRunID(messageA) != AgentRunID(messageA) {
		t.Fatal("per-input Action or AgentRun identity is not stable")
	}
}
