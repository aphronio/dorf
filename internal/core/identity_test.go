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
}
