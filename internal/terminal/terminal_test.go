package terminal

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestSandboxPreparationInstallsInstructionsBeforeItCanSucceed(t *testing.T) {
	session := core.Session{ID: "job-instructions", AgentsMD: "Keep replies brief.\n"}
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
	if sandbox.path != "/workspace/job/AGENTS.md" || sandbox.contents != session.AgentsMD || sandbox.owner != ownershipMetadata(owned) {
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

func (*instructionsSandbox) Workspace() string { return "/workspace/job" }
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

func TestHarnessObservationNeverFallsBackFromExactSandbox(t *testing.T) {
	requested := make([]string, 0, 2)
	externals := Externals{Ownership: func(_ context.Context, sandboxID string) (provider.Ownership, error) {
		requested = append(requested, sandboxID)
		return provider.Ownership{}, fmt.Errorf("stop after ownership resolution")
	}}
	session := core.Session{ID: "job-exact-sandbox"}
	for _, test := range []struct {
		sandboxID string
		threadID  string
	}{
		{sandboxID: "sandbox-initial"},
		{sandboxID: "sandbox-history", threadID: "thread-1"},
	} {
		run := core.AgentRun{ID: "run-1", SessionID: session.ID, SandboxID: test.sandboxID, ThreadID: test.threadID}
		execution := core.AgentMessageExecution{Session: session, AgentRun: run, Sandbox: core.Sandbox{ID: test.sandboxID, SessionID: session.ID}}
		operation, err := NewAgentRunOperation(externals, execution)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := operation.History(context.Background(), run); err == nil {
			t.Fatal("observation continued after ownership resolution failure")
		}
	}
	want := []string{"sandbox-initial", "sandbox-history"}
	if fmt.Sprint(requested) != fmt.Sprint(want) {
		t.Fatalf("ownership lookups=%v want=%v", requested, want)
	}
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
