package incus

import (
	"context"
	"strings"
	"testing"
	"time"
)

type scriptedRunner struct {
	calls    [][]string
	existing bool
}

func (r *scriptedRunner) Run(_ context.Context, command string, input []byte, args ...string) (Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	joined := strings.Join(args, " ")
	if strings.HasPrefix(joined, "info ") && !r.existing {
		return Result{ExitCode: 1, Stderr: "not found"}, nil
	}
	if strings.HasPrefix(joined, "init ") {
		r.existing = true
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
