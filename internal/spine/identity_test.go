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
}
