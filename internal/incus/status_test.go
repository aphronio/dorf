package incus

import (
	"context"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"strings"
	"testing"
)

func TestStatusDoesNotStartOrExecuteAndRejectsForeignOwnership(t *testing.T) {
	owner := provider.Ownership{JobID: "job", SandboxID: "sandbox", OwnershipNonce: strings.Repeat("a", 64)}
	for raw, want := range map[string]string{"Running": "running", "Frozen": "paused", "Stopped": "stopped", "Starting": "unknown"} {
		client := newFakeClient(Instance{Name: owner.SandboxID, Config: ownershipConfig(owner), Status: raw})
		sandbox := Sandbox{ClientFactory: &fakeFactory{client: client}}
		result, err := sandbox.ObserveOwned(context.Background(), owner)
		if err != nil || result.Provider != "incus" || result.State != want || client.starts != 0 || len(client.execCalls) != 0 {
			t.Fatalf("status=%+v err=%v", result, err)
		}
		foreign := owner
		foreign.OwnershipNonce = strings.Repeat("b", 64)
		if _, err := sandbox.ObserveOwned(context.Background(), foreign); err == nil {
			t.Fatal("accepted foreign owner")
		}
	}
}
