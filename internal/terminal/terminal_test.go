package terminal

import (
	"context"
	"errors"

	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestSandboxPreparationInstallsInstructionsBeforeItCanSucceed(t *testing.T) {
	session := core.Session{ID: "session-instructions", AgentsMD: "Keep replies brief.\n"}
	owned := core.Sandbox{ID: "sandbox-instructions", SessionID: session.ID, OwnershipNonce: "owned"}
	sandbox := &instructionsSandbox{writeErr: errors.New("temporary file transport failure")}
	externals := Externals{Sandbox: sandbox}
	if _, err := externals.SandboxCreate(context.Background(), session, owned); !errors.Is(err, sandbox.writeErr) {
		t.Fatalf("preparation completed without instructions: %v", err)
	}
	sandbox.writeErr = nil
	if _, err := externals.SandboxCreate(context.Background(), session, owned); err != nil {
		t.Fatal(err)
	}
	if sandbox.path != "/workspace/AGENTS.md" || sandbox.contents != session.AgentsMD || sandbox.owner != ownershipMetadata(owned) {
		t.Fatalf("instructions were not installed in the exact owned workspace: %#v", sandbox)
	}
}

type instructionsSandbox struct {
	provider.Sandbox
	owner          provider.Ownership
	path, contents string
	writeErr       error
	created        bool
}

func (*instructionsSandbox) Workspace() string { return "/workspace" }
func (*instructionsSandbox) ObserveOwned(_ context.Context, owner provider.Ownership) (provider.Status, error) {
	return provider.Status{Provider: "test", State: "running", ProviderID: owner.SandboxID}, nil
}
func (s *instructionsSandbox) ReconcileOwnedCreate(context.Context, provider.Ownership) error {
	s.created = true
	return nil
}
func (s *instructionsSandbox) PutFile(_ context.Context, owner provider.Ownership, path string, contents []byte) error {
	if !s.created {
		return errors.New("workspace is not created")
	}
	s.owner, s.path, s.contents = owner, path, string(contents)
	return s.writeErr
}

func TestSandboxRoutesUseOnlyTheExactConfiguredProfileURL(t *testing.T) {
	adapter := incus.Adapter{Sandbox: incustest.Sandbox(bridgeAddressRunner{}, incus.Config{Network: "dorf0", ProviderGatewayURL: "http://10.42.0.1:8317/v1"})}
	if _, err := adapter.ProviderRouteURL(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "http://127.0.0.1:8317/v1", "http://0.0.0.0:8317/v1", "http://192.0.2.10:8317/v1", "https://gateway.example/v1?token=x"} {
		adapter.Config.ProviderGatewayURL = value
		if _, err := adapter.ProviderRouteURL(context.Background()); err == nil {
			t.Fatalf("accepted unsafe Sandbox route %s", value)
		}
	}
}

type bridgeAddressRunner struct{}

func (bridgeAddressRunner) Run(context.Context, string, []byte, ...string) (incus.Result, error) {
	return incus.Result{Stdout: "10.42.0.1/24\n"}, nil
}
