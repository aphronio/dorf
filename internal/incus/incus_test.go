package incus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type scriptedRunner struct {
	calls    [][]string
	inputs   [][]byte
	existing bool
	head     string
	remote   string
	name     string
	metadata map[string]string
}

type inventoryRunner struct {
	instances []reviewInstance
	exists    bool
	calls     [][]string
}

type imageInfoRunner struct{ result Result }

type missingImageRunner struct{ create Result }

func (r missingImageRunner) Run(_ context.Context, _ string, _ []byte, args ...string) (Result, error) {
	if len(args) > 0 && args[0] == "info" {
		return Result{ExitCode: 1, Stderr: "not found"}, nil
	}
	if len(args) > 0 && args[0] == "init" {
		return r.create, nil
	}
	return Result{}, nil
}

func (r imageInfoRunner) Run(_ context.Context, command string, _ []byte, args ...string) (Result, error) {
	if command != "incus" || strings.Join(args, " ") != "image info custom" {
		return Result{ExitCode: 1, Stderr: "unexpected command"}, nil
	}
	return r.result, nil
}

func TestResolveImageFingerprintTurnsAliasIntoExactIdentity(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	got, err := ResolveImageFingerprint(context.Background(), "custom", imageInfoRunner{result: Result{Stdout: "Fingerprint: " + fingerprint + "\n"}})
	if err != nil || got != fingerprint {
		t.Fatalf("fingerprint=%q err=%v", got, err)
	}
	if _, err := ResolveImageFingerprint(context.Background(), "custom", imageInfoRunner{result: Result{Stdout: "Aliases:\n- custom\n"}}); err == nil {
		t.Fatal("missing exact fingerprint was accepted")
	}
}

func TestCreateClassifiesOnlyMissingImageAsUnavailableProfileArtifact(t *testing.T) {
	owner := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-owned", OwnershipNonce: strings.Repeat("b", 64)}
	for _, test := range []struct {
		name        string
		create      Result
		unavailable bool
	}{
		{name: "missing image", create: Result{ExitCode: 1, Stderr: `Error: Image "missing" not found`}, unavailable: true},
		{name: "other create failure", create: Result{ExitCode: 1, Stderr: "network unavailable"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sandbox := Sandbox{Config: Config{Image: "missing", Network: "incusbr0", DiskSize: "40GiB"}, Runner: missingImageRunner{create: test.create}}
			err := sandbox.ReconcileOwnedCreate(context.Background(), owner)
			if provider.IsArtifactUnavailable(err) != test.unavailable {
				t.Fatalf("unavailable=%v error=%v", provider.IsArtifactUnavailable(err), err)
			}
		})
	}
}

func (r *inventoryRunner) Run(_ context.Context, command string, _ []byte, args ...string) (Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	joined := strings.Join(args, " ")
	if strings.HasPrefix(joined, "list ") {
		payload, _ := json.Marshal(r.instances)
		return Result{Stdout: string(payload)}, nil
	}
	if strings.HasPrefix(joined, "info ") && !r.exists {
		return Result{ExitCode: 1, Stderr: "not found"}, nil
	}
	return Result{}, nil
}

func TestSandboxRequiresExactDurableOwnership(t *testing.T) {
	metadata := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-owned", OwnershipNonce: strings.Repeat("b", 64)}
	config := map[string]string{
		"user.dorf.owner": "sandbox", "user.dorf.job": metadata.JobID, "user.dorf.sandbox": metadata.SandboxID,
		"user.dorf.ownership_nonce": metadata.OwnershipNonce,
	}
	runner := &inventoryRunner{exists: true, instances: []reviewInstance{{Name: metadata.SandboxID, Config: config}}}
	sandbox := Sandbox{Runner: runner}
	if err := sandbox.AttestOwnership(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	wrong := metadata
	wrong.OwnershipNonce = strings.Repeat("c", 64)
	if err := sandbox.AttestOwnership(context.Background(), wrong); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched owner metadata error=%v", err)
	}
	runner.instances = append(runner.instances, reviewInstance{Name: "dorf-competing", Config: config})
	if err := sandbox.AttestOwnership(context.Background(), metadata); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ambiguous Sandbox error=%v", err)
	}
}

func TestSandboxDeletionIsRetrySafeButNeverDeletesForeignMetadata(t *testing.T) {
	metadata := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-owned", OwnershipNonce: strings.Repeat("b", 64)}
	runner := &inventoryRunner{}
	sandbox := Sandbox{Runner: runner}
	for range 2 {
		if err := sandbox.DeleteOwned(context.Background(), metadata); err != nil {
			t.Fatal(err)
		}
	}
	runner.exists = true
	runner.instances = []reviewInstance{{Name: metadata.SandboxID, Config: map[string]string{"user.dorf.sandbox": metadata.SandboxID, "user.dorf.owner": "foreign"}}}
	if err := sandbox.DeleteOwned(context.Background(), metadata); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("foreign Sandbox deletion error=%v", err)
	}
}

func (r *scriptedRunner) Run(_ context.Context, command string, input []byte, args ...string) (Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	r.inputs = append(r.inputs, append([]byte(nil), input...))
	joined := strings.Join(args, " ")
	if strings.HasPrefix(joined, "list ") {
		instances := []reviewInstance{}
		if r.existing && r.metadata != nil {
			instances = append(instances, reviewInstance{Name: r.name, Config: r.metadata})
		}
		payload, _ := json.Marshal(instances)
		return Result{Stdout: string(payload)}, nil
	}
	if strings.HasPrefix(joined, "info ") && !r.existing {
		return Result{ExitCode: 1, Stderr: "not found"}, nil
	}
	if strings.Contains(joined, "rev-parse --git-dir") && !r.existing {
		return Result{ExitCode: 1, Stderr: "not a repository"}, nil
	}
	if strings.HasPrefix(joined, "init ") {
		r.existing = true
		r.name = args[2]
		r.metadata = make(map[string]string)
		for i := 3; i+1 < len(args); i++ {
			if args[i] != "-c" {
				continue
			}
			key, value, ok := strings.Cut(args[i+1], "=")
			if ok {
				r.metadata[key] = value
			}
			i++
		}
	}
	if strings.Contains(joined, "git clone --no-checkout") {
		r.existing = true
	}
	if strings.Contains(joined, "rev-parse HEAD") {
		return Result{Stdout: r.head + "\n"}, nil
	}
	if strings.Contains(joined, "remote get-url origin") {
		return Result{Stdout: r.remote + "\n"}, nil
	}
	if strings.HasPrefix(joined, "network get ") {
		return Result{Stdout: r.head + "\n"}, nil
	}
	return Result{}, nil
}

func TestOwnedSandboxCreationUsesRecordedIdentityAndCredentialFreeBoundary(t *testing.T) {
	runner := &scriptedRunner{}
	sandbox := Sandbox{Config: Config{Image: "dorf-codex", Network: "incusbr0", DiskSize: "40GiB", Workspace: "/workspace/job"}, Runner: runner, Sleep: func(_ time.Duration) {}}
	metadata := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-sandbox-exact", OwnershipNonce: strings.Repeat("a", 64)}
	if err := sandbox.ReconcileOwnedCreate(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ReconcileOwnedCreate(context.Background(), metadata); err != nil {
		t.Fatal(err)
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
