package terminal

import (
	"context"
	"fmt"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestHarnessObservationNeverFallsBackFromExactSandbox(t *testing.T) {
	requested := make([]string, 0, 2)
	externals := Externals{Ownership: func(_ context.Context, sandboxID string) (provider.Ownership, error) {
		requested = append(requested, sandboxID)
		return provider.Ownership{}, fmt.Errorf("stop after ownership resolution")
	}}
	job := core.Job{ID: "job-exact-sandbox"}
	for _, test := range []struct {
		sandboxID string
		threadID  string
	}{
		{sandboxID: "sandbox-initial"},
		{sandboxID: "sandbox-history", threadID: "thread-1"},
	} {
		run := core.AgentRun{ID: "run-1", JobID: job.ID, SandboxID: test.sandboxID, ThreadID: test.threadID}
		execution := core.AgentMessageExecution{Job: job, AgentRun: run, Sandbox: core.Sandbox{ID: test.sandboxID, JobID: job.ID}}
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
