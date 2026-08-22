package terminal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/investigation"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type localRestoreSandbox struct {
	provider.Sandbox
	root      string
	workspace string
}

func (s localRestoreSandbox) Workspace() string { return s.workspace }

func (s localRestoreSandbox) physical(path string) string {
	switch {
	case path == s.workspace || strings.HasPrefix(path, s.workspace+"/"):
		return filepath.Join(s.root, strings.TrimPrefix(path, "/"))
	case path == "/tmp/dorf" || strings.HasPrefix(path, "/tmp/dorf/"):
		return filepath.Join(s.root, strings.TrimPrefix(path, "/"))
	default:
		return path
	}
}

func (s localRestoreSandbox) PutFile(_ context.Context, _ provider.Ownership, destination string, contents []byte) error {
	destination = s.physical(destination)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	return os.WriteFile(destination, contents, 0o600)
}

func (s localRestoreSandbox) Exec(ctx context.Context, _ provider.Ownership, input []byte, argv ...string) (provider.Result, error) {
	argv = append([]string(nil), argv...)
	for i := range argv {
		argv[i] = s.physical(argv[i])
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// A real Sandbox command does not inherit Dorf's repository as its current
	// directory. Keep this fake outside any Git repository so bundle operations
	// must establish their own explicit repository context.
	command.Dir = s.root
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := provider.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if exit, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

func TestRepositoryRestoreMaterializesExactRetainedBundleAndReconcilesReplay(t *testing.T) {
	local := t.TempDir()
	run := func(directory string, args ...string) string {
		command := exec.Command("git", args...)
		command.Dir = directory
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("", "init", "--quiet", local)
	run(local, "config", "user.name", "Dorf Test")
	run(local, "config", "user.email", "dorf-test@localhost")
	if err := os.WriteFile(filepath.Join(local, "unpublished.txt"), []byte("exact committed source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(local, "add", "unpublished.txt")
	run(local, "commit", "--quiet", "-m", "unpublished")
	bundle, err := gitworkspace.BundleLocalRevision(context.Background(), local, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(bundle.Contents))
	job := core.Job{ID: "job-local-source"}
	owned := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	source := investigation.Source{
		JobID: job.ID, Kind: investigation.SourceGitBundle, Revision: bundle.Revision,
		BundleDigest: digest, BundleByteSize: int64(len(bundle.Contents)),
	}
	sandbox := localRestoreSandbox{root: t.TempDir(), workspace: "/workspace/job"}
	externals := Externals{Sandbox: sandbox}
	for attempt := 0; attempt < 2; attempt++ {
		if err := externals.RepositoryRestore(context.Background(), job, owned, source, bundle.Contents); err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
	}
	checkout := sandbox.physical(sandbox.workspace)
	if got := run(checkout, "rev-parse", "HEAD"); got != bundle.Revision {
		t.Fatalf("HEAD=%s want=%s", got, bundle.Revision)
	}
	if got := run(checkout, "branch", "--show-current"); got != "" {
		t.Fatalf("retained investigation checkout is attached to branch %s", got)
	}
	contents, err := os.ReadFile(filepath.Join(checkout, "unpublished.txt"))
	if err != nil || string(contents) != "exact committed source\n" {
		t.Fatalf("contents=%q err=%v", contents, err)
	}
}

func TestRepositoryRestoreRefusesUnownedWorkspaceContents(t *testing.T) {
	sandbox := localRestoreSandbox{root: t.TempDir(), workspace: "/workspace/job"}
	workspace := sandbox.physical(sandbox.workspace)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "foreign.txt"), []byte("do not delete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	job := core.Job{ID: "job-foreign"}
	owned := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID, OwnershipNonce: strings.Repeat("b", 64)}
	source := investigation.Source{
		JobID: job.ID, Kind: investigation.SourceGitBundle, Revision: revision,
		BundleDigest: strings.Repeat("c", 64), BundleByteSize: 1,
	}
	err := (Externals{Sandbox: sandbox}).RepositoryRestore(context.Background(), job, owned, source, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("restore error=%v", err)
	}
	contents, readErr := os.ReadFile(filepath.Join(workspace, "foreign.txt"))
	if readErr != nil || string(contents) != "do not delete\n" {
		t.Fatalf("foreign contents=%q err=%v", contents, readErr)
	}
}

func TestReviewInputComesFromExactWorkflowMessage(t *testing.T) {
	run := coding.ReviewRunView{
		ID: "agent-run-review", JobID: "job-review", MessageID: "message-review",
		Request: core.Message{ID: "message-review", JobID: "job-review", FromKind: core.MessageFromWorkflow, Input: "review this exact Revision"},
	}
	input, err := reviewInput(run)
	if err != nil || input != run.Request.Input {
		t.Fatalf("review input=%q err=%v", input, err)
	}
	run.Request.ID = "message-forged"
	if _, err := reviewInput(run); err == nil {
		t.Fatal("review accepted a request Message that did not own the AgentRun")
	}
}

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

func TestSandboxRoutesRequireExactConfiguredBridgeAddress(t *testing.T) {
	adapter := incus.Adapter{Sandbox: incus.Sandbox{Config: incus.Config{Network: "dorf0"}, Runner: bridgeAddressRunner{}}}
	if _, err := adapter.ProviderRouteURL(context.Background(), "http://10.42.0.1:8317/v1"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"http://10.43.0.1:8317/v1", "http://127.0.0.1:8317/v1", "http://0.0.0.0:8317/v1", "http://192.0.2.10:8317/v1", "https://10.42.0.1:8317/v1"} {
		if _, err := adapter.ProviderRouteURL(context.Background(), value); err == nil {
			t.Fatalf("accepted unsafe Sandbox route %s", value)
		}
	}
}

type bridgeAddressRunner struct{}

func (bridgeAddressRunner) Run(context.Context, string, []byte, ...string) (incus.Result, error) {
	return incus.Result{Stdout: "10.42.0.1/24\n"}, nil
}

type localReviewBoundaryRunner struct {
	implementationName string
	reviewerName       string
	implementationPath string
	reviewerPath       string
	metadata           map[string]string
	calls              [][]string
}

func (r *localReviewBoundaryRunner) Run(ctx context.Context, command string, input []byte, args ...string) (incus.Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	if command != "incus" {
		return incus.Result{}, nil
	}
	if len(args) >= 2 && args[0] == "list" {
		payload, _ := json.Marshal([]map[string]any{{"name": r.reviewerName, "config": r.metadata}})
		return incus.Result{Stdout: string(payload)}, nil
	}
	if len(args) == 5 && args[0] == "config" && args[1] == "set" {
		r.metadata[args[3]] = args[4]
		return incus.Result{}, nil
	}
	if len(args) < 4 || args[0] != "exec" || args[2] != "--" {
		return incus.Result{ExitCode: 1, Stderr: "unexpected Incus invocation"}, nil
	}
	physical := r.reviewerPath
	if args[1] == r.implementationName {
		physical = r.implementationPath
	} else if args[1] != r.reviewerName {
		return incus.Result{ExitCode: 1, Stderr: "unexpected Sandbox identity"}, nil
	}
	guest := append([]string(nil), args[3:]...)
	for i := range guest {
		if guest[i] == "/workspace/job" {
			guest[i] = physical
		}
	}
	cmd := exec.CommandContext(ctx, guest[0], guest[1:]...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := incus.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	if exit, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

func TestPrepareReviewCheckoutRealGitIgnoresImplementationForgedWorktree(t *testing.T) {
	root := t.TempDir()
	implementationPath := filepath.Join(root, "implementation", "workspace", "job")
	reviewerPath := filepath.Join(root, "reviewer", "workspace", "job")
	if err := os.MkdirAll(implementationPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", implementationPath}, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(implementationPath, "reviewed.txt"), []byte("exact admitted tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "reviewed.txt")
	runGit("-c", "user.name=Dorf Test", "-c", "user.email=dorf@example.invalid", "commit", "-m", "admitted")
	revision := runGit("rev-parse", "HEAD")
	tree := runGit("rev-parse", "HEAD^{tree}")
	forged := filepath.Join(root, "implementation", "tmp", "dorf", "review-checkouts", "agent-run-forged")
	if err := os.MkdirAll(forged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(forged, "forged.txt"), []byte("must remain invisible\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	job := coding.Job{Job: core.Job{ID: "job-real-boundary"}, Revision: revision}
	run := coding.ReviewRunView{
		ID: "agent-run-real-boundary", JobID: job.ID, InputRevision: revision, SandboxID: "dorf-review-real",
		Sandbox: core.Sandbox{ID: "dorf-review-real", JobID: job.ID, OwnershipNonce: strings.Repeat("d", 64)},
	}
	metadata := map[string]string{
		"user.dorf.owner": "sandbox", "user.dorf.job": job.ID, "user.dorf.sandbox": run.Sandbox.ID, "user.dorf.agent_run": run.ID,
		"user.dorf.revision": revision, "user.dorf.ownership_nonce": run.Sandbox.OwnershipNonce,
	}
	sandbox := incus.Sandbox{Config: incus.Config{Workspace: "/workspace/job"}}
	runner := &localReviewBoundaryRunner{implementationName: sandbox.Name(job.ID), reviewerName: run.Sandbox.ID, implementationPath: implementationPath, reviewerPath: reviewerPath, metadata: metadata}
	sandbox.Runner = runner
	externals := Externals{
		Sandbox: incus.Adapter{Sandbox: sandbox},
		Ownership: func(_ context.Context, sandboxID string) (provider.Ownership, error) {
			if sandboxID == run.Sandbox.ID {
				return ownershipMetadata(run.Sandbox), nil
			}
			return provider.Ownership{JobID: job.ID, SandboxID: sandboxID}, nil
		},
	}
	withoutSandbox := run
	withoutSandbox.SandboxID = ""
	if err := externals.PrepareReviewCheckout(context.Background(), job, withoutSandbox); err == nil {
		t.Fatal("review checkout accepted the implementation Sandbox")
	}
	if err := externals.PrepareReviewCheckout(context.Background(), job, run); err != nil {
		t.Fatal(err)
	}
	checkout, err := externals.VerifyReviewCheckout(context.Background(), job, run)
	wantCheckout := coding.ReviewCheckoutObservation{Revision: revision, Tree: tree}
	if err != nil || checkout != wantCheckout {
		t.Fatalf("review checkout=%#v err=%v", checkout, err)
	}
	contents, err := os.ReadFile(filepath.Join(reviewerPath, "reviewed.txt"))
	if err != nil || string(contents) != "exact admitted tree\n" {
		t.Fatalf("reviewer contents=%q err=%v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(reviewerPath, "forged.txt")); !os.IsNotExist(err) {
		t.Fatalf("implementation forged worktree crossed reviewer boundary: %v", err)
	}
	implementationSandbox := core.MainSandboxName(job.ID)
	var sourceImplementation, targetReviewer, verifyReviewer bool
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		sourceImplementation = sourceImplementation || strings.Contains(joined, "exec "+implementationSandbox+" --") && strings.Contains(joined, "dorf-review-source")
		targetReviewer = targetReviewer || strings.Contains(joined, "exec "+run.Sandbox.ID+" --") && strings.Contains(joined, "dorf-review-checkout")
		verifyReviewer = verifyReviewer || strings.Contains(joined, "exec "+run.Sandbox.ID+" --") && strings.Contains(joined, "dorf-review-verify")
	}
	if !sourceImplementation || !targetReviewer || !verifyReviewer {
		t.Fatalf("Git object source or isolated target boundary missing: calls=%v", runner.calls)
	}
}
