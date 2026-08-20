package core

import "testing"

func TestCleanupActionOrderIsExactAndStable(t *testing.T) {
	targets := cleanupTargets([]Sandbox{
		{ID: "sandbox-b", JobID: "job-1"},
		{ID: "sandbox-a", JobID: "job-1"},
	})
	want := []struct {
		sandbox string
		kind    ActionKind
	}{
		{"sandbox-a", ActionRouteRevoke},
		{"sandbox-a", ActionSandboxDelete},
		{"sandbox-b", ActionRouteRevoke},
		{"sandbox-b", ActionSandboxDelete},
	}
	if len(targets) != len(want) {
		t.Fatalf("cleanup targets = %d, want %d", len(targets), len(want))
	}
	for i := range want {
		if targets[i].Sandbox.ID != want[i].sandbox || targets[i].Kind != want[i].kind {
			t.Fatalf("cleanup target %d = %#v, want %s %s", i, targets[i], want[i].sandbox, want[i].kind)
		}
	}
}
