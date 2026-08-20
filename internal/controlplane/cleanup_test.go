package controlplane

import (
	"testing"

	"github.com/aphronio/dorf/internal/spine"
)

func TestCleanupActionOrderIsExactAndStable(t *testing.T) {
	targets := cleanupTargets([]spine.Sandbox{
		{ID: "sandbox-b", JobID: "job-1"},
		{ID: "sandbox-a", JobID: "job-1"},
	})
	want := []struct {
		sandbox string
		kind    spine.ActionKind
	}{
		{"sandbox-a", spine.ActionRouteRevoke},
		{"sandbox-a", spine.ActionSandboxDelete},
		{"sandbox-b", spine.ActionRouteRevoke},
		{"sandbox-b", spine.ActionSandboxDelete},
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
