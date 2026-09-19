package incus

import (
	"context"
	"strings"
	"testing"
)

func TestAdapterExecAttestsExactOwnershipBeforeExecution(t *testing.T) {
	accepted := OwnershipMetadata{
		SessionID:      "job-accepted",
		SandboxID:      "sandbox-1",
		OwnershipNonce: strings.Repeat("a", 64),
	}
	foreign := accepted
	foreign.SessionID = "job-foreign"
	client := newFakeClient(ownedInstance(accepted))
	factory := &fakeFactory{client: client}
	adapter := Adapter{Sandbox: Sandbox{Config: Config{Workspace: "/workspace"}, ClientFactory: factory}}

	if _, err := adapter.Exec(context.Background(), foreign, nil, "true"); err == nil {
		t.Fatal("Exec accepted foreign ownership metadata")
	}
	if len(client.execCalls) != 0 {
		t.Fatalf("foreign ownership reached Incus Exec: %#v", client.execCalls)
	}
	if factory.opens != 1 || client.inventories != 1 || client.closes != 1 {
		t.Fatal("ownership rejection did not use and close one client")
	}
	if _, err := adapter.Exec(t.Context(), accepted, nil, "true"); err != nil {
		t.Fatal(err)
	}
	if factory.opens != 2 || client.inventories != 2 || client.closes != 2 || len(client.execCalls) != 1 {
		t.Fatal("owned execution did not use one freshly attested client")
	}

	// A resource whose ownership changed must not reuse the previous attestation.
	client.instances[accepted.SandboxID] = ownedInstance(foreign)
	if _, err := adapter.Exec(t.Context(), accepted, nil, "true"); err == nil {
		t.Fatal("Exec reused stale ownership")
	}
	if _, err := adapter.ReadFile(t.Context(), accepted, "/tmp/probe"); err == nil {
		t.Fatal("ReadFile accepted stale ownership")
	}
	if err := adapter.PutFile(t.Context(), accepted, "/tmp/probe", []byte("probe")); err == nil {
		t.Fatal("PutFile accepted stale ownership")
	}
	if len(client.execCalls) != 1 || factory.opens != 5 || client.inventories != 5 || client.closes != 5 {
		t.Fatal("stale ownership reached execution or repeated client setup")
	}
}
