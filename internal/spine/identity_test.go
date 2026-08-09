package spine

import (
	"strings"
	"testing"
)

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
	if MessageID(jobA, "caller-a") == MessageID(JobID("client-request-41"), "caller-a") {
		t.Fatal("same caller ID shares a Message identity across Jobs")
	}
	if TurnActionID(messageA) != TurnActionID(messageA) || AgentRunID(messageA) != AgentRunID(messageA) {
		t.Fatal("per-input Action or AgentRun identity is not stable")
	}
	revisionA := strings.Repeat("a", 40)
	revisionB := strings.Repeat("b", 40)
	if ScopedActionID(jobA, ActionRepositoryCommit, revisionA) != ScopedActionID(jobA, ActionRepositoryCommit, revisionA) {
		t.Fatal("commit Action identity changed across attempts")
	}
	if ScopedActionID(jobA, ActionRepositoryCommit, revisionA) == ScopedActionID(jobA, ActionRepositoryCommit, revisionB) {
		t.Fatal("distinct parent Revisions share a commit Action identity")
	}
	if CheckID(jobA, revisionA, "check") != CheckID(jobA, revisionA, "check") || CheckID(jobA, revisionA, "check") == CheckID(jobA, revisionB, "check") || CheckID(jobA, revisionA, "check") == CheckID(jobA, revisionA, "smoke") {
		t.Fatal("Check identity is not stable and scoped to semantic name plus exact Revision")
	}
}
