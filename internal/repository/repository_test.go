package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/incus"
)

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

func TestCommitTreeCrashBeforeReceiptConvergesOnDeterministicOID(t *testing.T) {
	repo, parent := testRepository(t)
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("bounded change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	tree := gitOutput(t, repo, "write-tree")
	parentTime := gitOutput(t, repo, "show", "-s", "--format=%ct", parent)
	message := "Dorf Job job-proof revision 1\n"
	commitTime, err := strconv.ParseInt(parentTime, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "commit-tree", tree, "-p", parent)
	cmd.Stdin = strings.NewReader(message)
	deterministicDate := fmt.Sprintf("@%d +0000", commitTime+1)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dorf", "GIT_AUTHOR_EMAIL=dorf@localhost", "GIT_AUTHOR_DATE="+deterministicDate, "GIT_COMMITTER_NAME=Dorf", "GIT_COMMITTER_EMAIL=dorf@localhost", "GIT_COMMITTER_DATE="+deterministicDate)
	lostOIDBytes, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lostOID := strings.TrimSpace(string(lostOIDBytes))
	if gitOutput(t, repo, "rev-parse", "HEAD") != parent {
		t.Fatal("inner-boundary simulation unexpectedly advanced the branch")
	}
	manager := testManager(repo)
	recovered, _, err := manager.Commit(context.Background(), "sandbox", "action-"+parent[:12], "job-proof", "dorf/proof", parent, 1)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != lostOID {
		t.Fatalf("retry commit=%s lost-response commit=%s", recovered.Revision, lostOID)
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
