package incus

import (
	"context"
	"strings"
	"testing"
)

func TestAdapterExecAttestsExactOwnershipBeforeExecution(t *testing.T) {
	accepted := OwnershipMetadata{
		JobID:          "job-accepted",
		SandboxID:      "sandbox-1",
		OwnershipNonce: strings.Repeat("a", 64),
	}
	foreign := accepted
	foreign.JobID = "job-foreign"
	client := newFakeClient(ownedInstance(accepted))
	adapter := Adapter{Sandbox: Sandbox{ClientFactory: &fakeFactory{client: client}}}

	if _, err := adapter.Exec(context.Background(), foreign, nil, "true"); err == nil {
		t.Fatal("Exec accepted foreign ownership metadata")
	}
	if len(client.execCalls) != 0 {
		t.Fatalf("foreign ownership reached Incus Exec: %#v", client.execCalls)
	}
}
