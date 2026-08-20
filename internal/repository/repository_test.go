package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

var repositoryTestOwner = provider.Ownership{SandboxID: "sandbox"}

func TestParseNarrowRepositoryContract(t *testing.T) {
	contract, err := ParseContract(`[commands]
prepare = "uv sync --frozen"
check = "go test ./..." # direct command
smoke = 'go build ./cmd/dorf'

[agent.codex]
model = "gpt-5.6-sol"

[review]
performance = true
`)
	if err != nil {
		t.Fatal(err)
	}
	if contract.Prepare != "uv sync --frozen" || len(contract.Checks) != 2 || contract.Checks[0].Name != "check" || contract.Checks[1].Name != "smoke" || !contract.DeclaredPerformance {
		t.Fatalf("contract=%#v", contract)
	}
	invalid := []string{
		`commands = "go test ./..."`,
		"[commands]\ncheck = [\"go\", \"test\", \"./...\"]",
		"[commands]\nprepare = \"uv sync\"\nreview = \"agent review\"\ncheck = \"go test ./...\"",
		"[commands]\nprepare = \"uv sync\"",
		"[commands]\nprepare = \"uv sync\"\ncheck = \"go test ./...\"\n[review]\nperformance = \"yes\"",
	}
	for _, contents := range invalid {
		if _, err := ParseContract(contents); err == nil {
			t.Fatalf("invalid contract accepted: %s", contents)
		}
	}
}

func TestKnownScopedCapabilitiesAreRedactedFromEvidenceOutput(t *testing.T) {
	if len(knownScopedSecrets) != 2 || knownScopedSecrets[0].path != "/root/.config/dorf/provider-route.key" || knownScopedSecrets[1].path != "/tmp/dorf/codex-app-server.control-token" {
		t.Fatalf("bounded scoped capability allowlist=%#v", knownScopedSecrets)
	}
	secrets := []scopedSecret{
		{label: "dorf-provider-route-key", replacement: "[REDACTED_DORF_PROVIDER_ROUTE_KEY]", value: "test-route-capability"},
		{label: "dorf-codex-control-token", replacement: "[REDACTED_DORF_CODEX_CONTROL_TOKEN]", value: "test-control-capability"},
	}
	stdout, stderr, labels := redact("before test-route-capability test-control-capability after", "test-control-capability test-route-capability", secrets)
	if strings.Contains(stdout+stderr, "test-route-capability") || strings.Contains(stdout+stderr, "test-control-capability") || !strings.Contains(stdout+stderr, "REDACTED_DORF_PROVIDER_ROUTE_KEY") || !strings.Contains(stdout+stderr, "REDACTED_DORF_CODEX_CONTROL_TOKEN") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	if fmt.Sprint(labels) != "[dorf-provider-route-key dorf-codex-control-token]" {
		t.Fatalf("redaction labels=%v", labels)
	}
}

func TestCommandReceiptExecutesExactCommandOnce(t *testing.T) {
	repo, head := testRepository(t)
	counter := filepath.Join(t.TempDir(), "counter")
	manager := testManager(repo)
	identity := "check-" + head[:12]
	command := fmt.Sprintf("printf 'once\\n' >> %q; printf 'observed stdout\\n'; printf 'observed stderr\\n' >&2; exit 7", counter)
	first, err := manager.RunCommand(context.Background(), repositoryTestOwner, identity, head, command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.RunCommand(context.Background(), repositoryTestOwner, identity, head, command)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExitCode != 7 || string(first.Stdout) != "observed stdout\n" || string(first.Stderr) != "observed stderr\n" || second.ExitCode != first.ExitCode {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	contents, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "once\n" {
		t.Fatalf("command was repeated: %q", contents)
	}
}

func TestObserveRevisionFromRealGitCheckout(t *testing.T) {
	tests := []struct {
		name  string
		dirty bool
	}{
		{name: "multiple agent-created commits are accepted"},
		{name: "dirty checkout is rejected", dirty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, base := testRepository(t)
			for i := 1; i <= 2; i++ {
				if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte(fmt.Sprintf("agent change %d\n", i)), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, repo, "add", "tracked.txt")
				runGit(t, repo, "commit", "-m", fmt.Sprintf("agent commit %d", i))
			}
			if tt.dirty {
				if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("not committed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			observation, err := testManager(repo).ObserveRevision(context.Background(), repositoryTestOwner, "dorf/proof", base)
			if tt.dirty {
				if err == nil || !strings.Contains(err.Error(), "checkout is dirty") {
					t.Fatalf("dirty observation error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			head := gitOutput(t, repo, "rev-parse", "HEAD")
			tree := gitOutput(t, repo, "show", "-s", "--format=%T", "HEAD")
			if observation.ComparisonBase != base || observation.Revision != head || observation.Tree != tree || observation.Branch != "dorf/proof" || observation.StartedAt.IsZero() || observation.FinishedAt.Before(observation.StartedAt) {
				t.Fatalf("observation=%#v", observation)
			}
		})
	}
}

func TestReconcileCloneOwnsExactCheckoutAboveSandboxProvider(t *testing.T) {
	remote, revision := testRepository(t)
	workspace := filepath.Join(t.TempDir(), "checkout")
	manager := testManager(workspace)
	if err := manager.ReconcileClone(context.Background(), repositoryTestOwner, remote, revision, "dorf/proof"); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, workspace, "rev-parse", "HEAD"); got != revision {
		t.Fatalf("checkout HEAD=%s want=%s", got, revision)
	}
	if got := gitOutput(t, workspace, "config", "--local", "user.name"); got != "Dorf Agent" {
		t.Fatalf("checkout user.name=%q", got)
	}

	if err := os.WriteFile(filepath.Join(remote, "tracked.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, remote, "add", "tracked.txt")
	runGit(t, remote, "commit", "-m", "second")
	second := gitOutput(t, remote, "rev-parse", "HEAD")
	if err := manager.ReconcileClone(context.Background(), repositoryTestOwner, remote, second, "dorf/proof"); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, workspace, "rev-parse", "HEAD"); got != second {
		t.Fatalf("refreshed checkout HEAD=%s want=%s", got, second)
	}

	foreign, _ := testRepository(t)
	if err := manager.ReconcileClone(context.Background(), repositoryTestOwner, foreign, second, "dorf/proof"); err == nil || !strings.Contains(err.Error(), "origin does not match") {
		t.Fatalf("foreign origin error=%v", err)
	}
}

type localSandbox struct{ workspace string }

func (s localSandbox) Workspace() string                                            { return s.workspace }
func (localSandbox) ReconcileOwnedCreate(context.Context, provider.Ownership) error { return nil }
func (localSandbox) AttestOwnership(context.Context, provider.Ownership) error      { return nil }
func (localSandbox) AttachReviewMetadata(context.Context, provider.Ownership, provider.ReviewMetadata) error {
	return nil
}
func (localSandbox) OwnedPresent(context.Context, provider.Ownership) (bool, error) { return true, nil }
func (localSandbox) DeleteOwned(context.Context, provider.Ownership) error          { return nil }
func (localSandbox) AttestReview(context.Context, provider.Ownership, provider.ReviewMetadata) error {
	return nil
}
func (localSandbox) PutFile(_ context.Context, _ provider.Ownership, path string, contents []byte) error {
	return os.WriteFile(path, contents, 0o600)
}
func (localSandbox) Endpoint(context.Context, provider.Ownership, int) (provider.Endpoint, error) {
	return provider.Endpoint{}, nil
}
func (localSandbox) ProviderRouteURL(context.Context, string) (string, error) { return "", nil }

func (localSandbox) Exec(ctx context.Context, _ provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	if len(args) == 0 {
		return provider.Result{}, errors.New("missing command")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := provider.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

func testManager(workspace string) Manager {
	return Manager{Sandbox: localSandbox{workspace: workspace}, Workspace: workspace}
}

func testRepository(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "dorf/proof")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base "+repo+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "commit", "-m", "base")
	return repo, gitOutput(t, repo, "rev-parse", "HEAD")
}

func runGit(t *testing.T, repo string, args ...string) { _ = gitOutput(t, repo, args...) }

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
