package pi

import (
	"context"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type recordingRunner struct {
	input  []byte
	args   []string
	result incus.Result
	calls  int
}

func testSandbox(runner incustest.Runner, owner provider.Ownership) incus.Adapter {
	return incus.Adapter{Sandbox: incustest.OwnedSandbox(runner, incus.Config{}, owner)}
}

func testOwner(sandboxID string) provider.Ownership {
	return provider.Ownership{SessionID: "session-" + sandboxID, SandboxID: sandboxID, OwnershipNonce: strings.Repeat("a", 64)}
}
func (r *recordingRunner) Run(_ context.Context, _ string, input []byte, args ...string) (incus.Result, error) {
	r.calls++
	r.input = append([]byte(nil), input...)
	r.args = append([]string(nil), args...)
	return r.result, nil
}

func TestInstallRouteUsesScopedGatewayKeyWithResponsesAPI(t *testing.T) {
	runner := &recordingRunner{}
	agent := Agent{Sandbox: testSandbox(runner, testOwner("sandbox"))}
	if err := agent.InstallRoute(context.Background(), testOwner("sandbox"), "http://10.0.0.1:8317/v1", "scoped-key", "gpt-test"); err != nil {
		t.Fatal(err)
	}
	input := string(runner.input)
	for _, required := range []string{`"api":"openai-responses"`, `"apiKey":"$DORF_PROVIDER_ROUTE_KEY"`, `"id":"gpt-test"`, "scoped-key"} {
		if !strings.Contains(input, required) {
			t.Fatalf("route input is missing %q: %s", required, input)
		}
	}
	if strings.Contains(input, `"apiKey":"scoped-key"`) {
		t.Fatal("scoped key was copied into Pi model configuration")
	}
}
