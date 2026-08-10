package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/spine"
)

func TestReviewControllerIdentityIsDerivedFromDurableOwnership(t *testing.T) {
	run := spine.ReviewRunView{
		AgentRun: spine.AgentRun{ID: "agent-run-review", SandboxID: "dorf-review-owned"},
		Sandbox:  spine.Sandbox{ID: "dorf-review-owned", OwnershipNonce: strings.Repeat("a", 64)},
	}
	want := spine.ReviewControllerID(run.ID, run.Sandbox.ID, run.Sandbox.OwnershipNonce)
	if got := reviewControllerID(run); got != want {
		t.Fatalf("review controller=%q want %q", got, want)
	}
}

func TestHarnessIdentityIsCodex(t *testing.T) {
	if got := (Externals{}).Harness(); got != "codex" {
		t.Fatalf("harness=%q", got)
	}
}

func TestReviewInputComesFromExactWorkflowMessage(t *testing.T) {
	run := spine.ReviewRunView{
		AgentRun: spine.AgentRun{ID: "agent-run-review", JobID: "job-review", MessageID: "message-review"},
		Request:  spine.Message{ID: "message-review", JobID: "job-review", FromKind: spine.MessageFromWorkflow, Input: "review this exact Revision"},
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

func TestSandboxRoutesRequireExactConfiguredBridgeAddress(t *testing.T) {
	if err := requireBridgeRoute("http://10.42.0.1:8317/v1", "10.42.0.1"); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"http://10.43.0.1:8317/v1", "http://127.0.0.1:8317/v1", "http://0.0.0.0:8317/v1", "http://192.0.2.10:8317/v1", "https://10.42.0.1:8317/v1"} {
		if err := requireBridgeRoute(value, "10.42.0.1"); err == nil {
			t.Fatalf("accepted unsafe Sandbox route %s", value)
		}
	}
}

func TestCodingTurnInputKeepsReviewFeedbackOpaque(t *testing.T) {
	job := spine.Job{Branch: "dorf/feedback", Revision: strings.Repeat("a", 40)}
	message := spine.Message{FromKind: spine.MessageFromAgent, FromID: "review-run-1", Input: "Reviewer prose that the implementation agent must interpret."}

	reviewer := codingTurnInput(job, spine.Delivery{Message: message, AgentRun: spine.AgentRun{Role: "critical-boundary-review"}})
	if reviewer != message.Input {
		t.Fatalf("reviewer input was rewritten: %q", reviewer)
	}

	implementation := codingTurnInput(job, spine.Delivery{Message: message, AgentRun: spine.AgentRun{Role: "implement"}})
	if !strings.HasPrefix(implementation, message.Input+"\n\n") || !strings.Contains(implementation, job.Branch) || !strings.Contains(implementation, job.Revision) {
		t.Fatalf("implementation input is missing the coding contract: %q", implementation)
	}
}

type localReviewBoundaryRunner struct {
	implementationName string
	reviewerName       string
	implementationPath string
	reviewerPath       string
	metadata           map[string]string
}

func (r *localReviewBoundaryRunner) Run(ctx context.Context, command string, input []byte, args ...string) (incus.Result, error) {
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

	job := spine.Job{ID: "job-real-boundary", Revision: revision}
	run := spine.ReviewRunView{
		AgentRun: spine.AgentRun{ID: "agent-run-real-boundary", JobID: job.ID, Revision: revision, SandboxID: "dorf-review-real"},
		Sandbox:  spine.Sandbox{ID: "dorf-review-real", JobID: job.ID, OwnershipNonce: strings.Repeat("d", 64)},
	}
	metadata := map[string]string{
		"user.dorf.owner": "sandbox", "user.dorf.job": job.ID, "user.dorf.sandbox": run.Sandbox.ID, "user.dorf.agent_run": run.ID,
		"user.dorf.revision": revision, "user.dorf.ownership_nonce": run.Sandbox.OwnershipNonce,
	}
	sandbox := incus.Sandbox{Config: incus.Config{Workspace: "/workspace/job"}}
	runner := &localReviewBoundaryRunner{implementationName: sandbox.Name(job.ID), reviewerName: run.Sandbox.ID, implementationPath: implementationPath, reviewerPath: reviewerPath, metadata: metadata}
	sandbox.Runner = runner
	externals := Externals{Sandbox: sandbox}
	if err := externals.PrepareReviewCheckout(context.Background(), job, run); err != nil {
		t.Fatal(err)
	}
	checkout, err := externals.VerifyReviewCheckout(context.Background(), job, run)
	wantCheckout := spine.ReviewCheckoutObservation{Revision: revision, Tree: tree}
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
}

type reviewBoundaryRunner struct {
	metadata map[string]string
	calls    [][]string
	revision string
	tree     string
}

func (r *reviewBoundaryRunner) Run(_ context.Context, command string, _ []byte, args ...string) (incus.Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	joined := strings.Join(args, " ")
	if strings.HasPrefix(joined, "list ") {
		payload, _ := json.Marshal([]map[string]any{{"name": "dorf-review-owned", "config": r.metadata}})
		return incus.Result{Stdout: string(payload)}, nil
	}
	if strings.Contains(joined, "dorf-review-source") {
		return incus.Result{Stdout: "trusted-git-bundle"}, nil
	}
	if strings.Contains(joined, "dorf-review-checkout") || strings.Contains(joined, "dorf-review-verify") {
		return incus.Result{Stdout: r.revision + " " + r.tree + " clean\n"}, nil
	}
	return incus.Result{}, nil
}

func TestPrepareReviewCheckoutUsesSeparateOwnedSandboxAndExactGitState(t *testing.T) {
	revision, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	job := spine.Job{ID: "job-1", Revision: revision}
	run := spine.ReviewRunView{
		AgentRun: spine.AgentRun{ID: "agent-run-1", JobID: job.ID, Revision: revision, SandboxID: "dorf-review-owned"},
		Sandbox:  spine.Sandbox{ID: "dorf-review-owned", JobID: job.ID, OwnershipNonce: strings.Repeat("c", 64)},
	}
	runner := &reviewBoundaryRunner{revision: revision, tree: tree, metadata: map[string]string{
		"user.dorf.owner": "sandbox", "user.dorf.job": job.ID, "user.dorf.sandbox": run.Sandbox.ID, "user.dorf.agent_run": run.ID,
		"user.dorf.revision": revision, "user.dorf.ownership_nonce": run.Sandbox.OwnershipNonce,
	}}
	externals := Externals{Sandbox: incus.Sandbox{Config: incus.Config{Workspace: "/workspace/job"}, Runner: runner}}
	withoutSandbox := run
	withoutSandbox.SandboxID = ""
	if err := externals.PrepareReviewCheckout(context.Background(), job, withoutSandbox); err == nil {
		t.Fatal("review checkout accepted the implementation Sandbox")
	}
	if err := externals.PrepareReviewCheckout(context.Background(), job, run); err != nil {
		t.Fatal(err)
	}
	checkout, err := externals.VerifyReviewCheckout(context.Background(), job, run)
	if err != nil || checkout != (spine.ReviewCheckoutObservation{Revision: revision, Tree: tree}) {
		t.Fatalf("review checkout=%#v err=%v", checkout, err)
	}
	implementationSandbox := externals.Sandbox.Name(job.ID)
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
