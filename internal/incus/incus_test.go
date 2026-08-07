package incus

import (
	"context"
	"strings"
	"testing"
	"time"
)

type scriptedRunner struct {
	calls    [][]string
	inputs   [][]byte
	existing bool
	head     string
}

func (r *scriptedRunner) Run(_ context.Context, command string, input []byte, args ...string) (Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	r.inputs = append(r.inputs, append([]byte(nil), input...))
	joined := strings.Join(args, " ")
	if strings.HasPrefix(joined, "info ") && !r.existing {
		return Result{ExitCode: 1, Stderr: "not found"}, nil
	}
	if strings.Contains(joined, "rev-parse --git-dir") && !r.existing {
		return Result{ExitCode: 1, Stderr: "not a repository"}, nil
	}
	if strings.HasPrefix(joined, "init ") {
		r.existing = true
	}
	if strings.Contains(joined, "git clone --no-checkout") {
		r.existing = true
	}
	if strings.Contains(joined, "rev-parse HEAD") {
		return Result{Stdout: r.head + "\n"}, nil
	}
	if strings.HasPrefix(joined, "network get ") {
		return Result{Stdout: r.head + "\n"}, nil
	}
	return Result{}, nil
}

func TestSandboxCreationUsesStableNameAndCredentialFreeBoundary(t *testing.T) {
	runner := &scriptedRunner{}
	sandbox := Sandbox{Config: Config{Image: "dorf-codex", Network: "incusbr0", DiskSize: "40GiB", Workspace: "/workspace/job"}, Runner: runner, Sleep: func(_ time.Duration) {}}
	first, err := sandbox.ReconcileCreate(context.Background(), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sandbox.ReconcileCreate(context.Background(), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("Sandbox identity changed: %s != %s", first, second)
	}
	creates := 0
	credentialChecks := 0
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.HasPrefix(joined, "incus init ") {
			creates++
		}
		if strings.Contains(joined, "auth.json") && strings.Contains(joined, "provider-route.key") {
			credentialChecks++
		}
	}
	if creates != 1 {
		t.Fatalf("Incus creates=%d, want 1", creates)
	}
	if credentialChecks == 0 {
		t.Fatal("credential-free image boundary was not checked")
	}
}

func TestRepositoryCloneVerifiesExactAdmittedHead(t *testing.T) {
	revision := strings.Repeat("a", 40)
	runner := &scriptedRunner{head: revision}
	sandbox := Sandbox{Config: Config{Workspace: "/workspace/job"}, Runner: runner}
	if err := sandbox.ReconcileClone(context.Background(), "dorf-job", "https://example.test/repo.git", revision, "dorf/proof"); err != nil {
		t.Fatal(err)
	}
	if !hasCall(runner.calls, "git -C /workspace/job rev-parse HEAD") {
		t.Fatal("Sandbox HEAD was not observed after checkout")
	}

	runner = &scriptedRunner{head: strings.Repeat("b", 40)}
	sandbox.Runner = runner
	if err := sandbox.ReconcileClone(context.Background(), "dorf-job", "https://example.test/repo.git", revision, "dorf/proof"); err == nil || !strings.Contains(err.Error(), "does not match admitted Revision") {
		t.Fatalf("mismatched Sandbox HEAD error = %v", err)
	}
}

func TestInstallRouteEnablesResponsesWebSockets(t *testing.T) {
	runner := &scriptedRunner{}
	sandbox := Sandbox{Runner: runner}
	if err := sandbox.InstallRoute(context.Background(), "dorf-job", "http://10.42.0.1:8317/v1", "scoped-test-key", true); err != nil {
		t.Fatal(err)
	}
	if len(runner.inputs) != 1 || !strings.Contains(string(runner.inputs[0]), "supports_websockets = true") {
		t.Fatalf("installed Codex provider config does not enable Responses WebSockets: %q", runner.inputs)
	}
}

func TestInstallRouteRetainsHTTPWhenUpstreamWebSocketsAreUnavailable(t *testing.T) {
	runner := &scriptedRunner{}
	sandbox := Sandbox{Runner: runner}
	if err := sandbox.InstallRoute(context.Background(), "dorf-job", "http://10.42.0.1:8317/v1", "scoped-test-key", false); err != nil {
		t.Fatal(err)
	}
	if len(runner.inputs) != 1 || !strings.Contains(string(runner.inputs[0]), "supports_websockets = false") {
		t.Fatalf("installed Codex provider config does not retain HTTP fallback: %q", runner.inputs)
	}
}

func TestConfiguredBridgeIPv4ComesFromExactIncusNetwork(t *testing.T) {
	runner := &scriptedRunner{}
	sandbox := Sandbox{Config: Config{Network: "dorfbr0"}, Runner: runner}
	runner.head = "10.42.0.1/24"
	address, err := sandbox.BridgeIPv4(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if address != "10.42.0.1" {
		t.Fatalf("bridge address = %q", address)
	}
	if !hasCall(runner.calls, "incus network get dorfbr0 ipv4.address") {
		t.Fatal("configured Incus network was not queried")
	}
}

func hasCall(calls [][]string, suffix string) bool {
	for _, call := range calls {
		if strings.HasSuffix(strings.Join(call, " "), suffix) {
			return true
		}
	}
	return false
}
