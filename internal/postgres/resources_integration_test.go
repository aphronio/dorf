package postgres_test

import (
	"context"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestSandboxResourceBindingCannotRedirectOwnership(t *testing.T) {
	_, store, session := actionIntegrationSession(t, "resource-binding")
	ctx := context.Background()
	owned, err := store.Sandbox(ctx, core.MainSandboxName(session.ID))
	if err != nil || owned.ResourceID == "" || owned.ProviderID != "" {
		t.Fatalf("initial resource identity=%q provider=%q err=%v", owned.ResourceID, owned.ProviderID, err)
	}
	bind := func(candidate core.Sandbox, providerID string) error {
		return store.WithSessionFence(ctx, session.ID, func() error {
			return store.BindSandboxResource(ctx, candidate, providerID)
		})
	}
	if err := bind(owned, "provider-original"); err != nil {
		t.Fatal(err)
	}
	if err := bind(owned, "provider-original"); err != nil {
		t.Fatalf("lost-acknowledgement retry: %v", err)
	}
	if err := bind(owned, "provider-other"); err == nil {
		t.Fatal("recorded locator was redirected")
	}
	foreign := owned
	foreign.OwnershipNonce = "foreign"
	if err := bind(foreign, "provider-original"); err == nil {
		t.Fatal("foreign ownership was accepted")
	}
	foreign = owned
	foreign.ID = "different-sandbox"
	if err := bind(foreign, "provider-original"); err == nil {
		t.Fatal("a different logical Sandbox was accepted")
	}
	resources, err := store.SandboxResources(ctx, session.ID)
	if err != nil || len(resources) != 1 || resources[0].ID != owned.ResourceID || resources[0].ProviderID != "provider-original" || resources[0].ObservedAt.IsZero() {
		t.Fatalf("resource history count=%d err=%v", len(resources), err)
	}
	current, err := store.Sandbox(ctx, owned.ID)
	if err != nil || current.ID != owned.ID || current.ResourceID != owned.ResourceID || current.ProviderID != "provider-original" || current.OwnershipNonce != owned.OwnershipNonce {
		t.Fatalf("binding changed logical Sandbox identity: %v", err)
	}
}
