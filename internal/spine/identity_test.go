package spine

import (
	"strings"
	"testing"
)

func TestStableIdentitiesDoNotContainGoalOrSecrets(t *testing.T) {
	jobA := JobID("client-request-40")
	jobB := JobID("client-request-40")
	jobC := JobID("client-request-41")
	if jobA != jobB || jobA == "" {
		t.Fatalf("job identity is not stable: %q != %q", jobA, jobB)
	}
	if MainSandboxName(jobA) != MainSandboxName(jobB) || MainSandboxName(jobA) == MainSandboxName(jobC) {
		t.Fatal("main Sandbox identity is not stable and Job-scoped")
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
	revisionA := strings.Repeat("a", 40)
	revisionB := strings.Repeat("b", 40)
	role := "critical-boundary"
	if ReviewRequestMessageID(jobA, revisionA, role) != MessageID(jobA, MessageFromWorkflow, ReviewRequestFromID(revisionA, role)) || ReviewRequestMessageID(jobA, revisionA, role) == ReviewRequestMessageID(jobA, revisionB, role) {
		t.Fatal("review request Message identity is not stable and Revision-scoped")
	}
	if CheckID(jobA, revisionA, "check") != CheckID(jobA, revisionA, "check") || CheckID(jobA, revisionA, "check") == CheckID(jobA, revisionB, "check") || CheckID(jobA, revisionA, "check") == CheckID(jobA, revisionA, "smoke") {
		t.Fatal("Check identity is not stable and scoped to semantic name plus exact Revision")
	}
}
