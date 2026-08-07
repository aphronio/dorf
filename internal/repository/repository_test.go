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

	"github.com/aphronio/dorf/internal/incus"
)

func TestParseNarrowRepositoryContract(t *testing.T) {
	contract, err := ParseContract(`[commands]
prepare = "uv sync --frozen"
check = "uv run pytest" # direct command
smoke = 'uv run dorf --help'

[agent.codex]
model = "gpt-5.6-sol"
`)
	if err != nil {
		t.Fatal(err)
	}
	if contract.Prepare != "uv sync --frozen" || len(contract.Checks) != 2 || contract.Checks[0].Name != "check" || contract.Checks[1].Name != "smoke" {
		t.Fatalf("contract=%#v", contract)
	}
	invalid := []string{
		`commands = "uv run pytest"`,
		"[commands]\ncheck = [\"uv\", \"run\", \"pytest\"]",
		"[commands]\nprepare = \"uv sync\"\nreview = \"agent review\"\ncheck = \"go test ./...\"",
		"[commands]\nprepare = \"uv sync\"",
	}
	for _, contents := range invalid {
		if _, err := ParseContract(contents); err == nil {
			t.Fatalf("invalid contract accepted: %s", contents)
		}
	}
}

func TestKnownScopedRouteSecretIsRedactedFromEvidenceOutput(t *testing.T) {
	stdout, stderr := redact("before route-secret after", "route-secret", "route-secret")
	if strings.Contains(stdout+stderr, "route-secret") || !strings.Contains(stdout+stderr, "REDACTED_DORF_PROVIDER_ROUTE_KEY") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestCommandReceiptExecutesExactCommandOnce(t *testing.T) {
	repo, head := testRepository(t)
	counter := filepath.Join(t.TempDir(), "counter")
	manager := testManager(repo)
	identity := "check-" + head[:12]
	command := fmt.Sprintf("printf 'once\\n' >> %q; printf 'observed stdout\\n'; printf 'observed stderr\\n' >&2; exit 7", counter)
	first, err := manager.RunCommand(context.Background(), "sandbox", identity, head, command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.RunCommand(context.Background(), "sandbox", identity, head, command)
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

func TestCommitReconcilesLostResponseFromExactGitFacts(t *testing.T) {
	repo, parent := testRepository(t)
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("bounded change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := testManager(repo)
	actionID := "action-" + parent[:12]
	first, _, err := manager.Commit(context.Background(), "sandbox", actionID, "job-proof", "dorf/proof", parent, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.Commit(context.Background(), "sandbox", actionID, "job-proof", "dorf/proof", parent, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Parent != parent || first.Revision == parent {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	if got := gitOutput(t, repo, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
		t.Fatalf("committed checkout is dirty: %q", got)
	}
	if err := os.WriteFile(filepath.Join(repo, "ambiguous.txt"), []byte("unrecorded"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Commit(context.Background(), "sandbox", actionID, "job-proof", "dorf/proof", parent, 1); err == nil || !strings.Contains(err.Error(), "attention") {
		t.Fatalf("dirty reconciliation error=%v", err)
	}
}

type localIncusRunner struct{}

func (localIncusRunner) Run(ctx context.Context, command string, input []byte, args ...string) (incus.Result, error) {
	if command != "incus" || len(args) < 4 || args[0] != "exec" || args[2] != "--" {
		return incus.Result{}, errors.New("unexpected fake Incus command")
	}
	cmd := exec.CommandContext(ctx, args[3], args[4:]...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := incus.Result{Stdout: stdout.String(), Stderr: stderr.String()}
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
	return Manager{Sandbox: incus.Sandbox{Runner: localIncusRunner{}}, Workspace: workspace}
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
