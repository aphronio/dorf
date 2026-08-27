package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestReviewInputComesFromExactWorkflowMessage(t *testing.T) {
	run := ReviewRunView{
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

type localReviewBoundaryRunner struct {
	implementationName string
	reviewerName       string
	implementationPath string
	reviewerPath       string
	implementation     map[string]string
	reviewer           map[string]string
	calls              [][]string
}

func (r *localReviewBoundaryRunner) Run(ctx context.Context, command string, input []byte, args ...string) (incus.Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	if command != "incus" {
		return incus.Result{}, nil
	}
	if len(args) >= 2 && args[0] == "list" {
		payload, _ := json.Marshal([]map[string]any{
			{"name": r.implementationName, "config": r.implementation},
			{"name": r.reviewerName, "config": r.reviewer},
		})
		return incus.Result{Stdout: string(payload)}, nil
	}
	if len(args) == 5 && args[0] == "config" && args[1] == "set" {
		metadata := r.reviewer
		if args[2] == r.implementationName {
			metadata = r.implementation
		}
		metadata[args[3]] = args[4]
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

	job := Job{Job: core.Job{ID: "job-real-boundary"}, Revision: revision}
	run := ReviewRunView{
		ID: "agent-run-real-boundary", JobID: job.ID, InputRevision: revision, SandboxID: "dorf-review-real",
		Sandbox: core.Sandbox{ID: "dorf-review-real", JobID: job.ID, OwnershipNonce: strings.Repeat("d", 64)},
	}
	reviewerMetadata := map[string]string{
		"user.dorf.owner": "sandbox", "user.dorf.job": job.ID, "user.dorf.sandbox": run.Sandbox.ID, "user.dorf.agent_run": run.ID,
		"user.dorf.revision": revision, "user.dorf.ownership_nonce": run.Sandbox.OwnershipNonce,
	}
	baseSandbox := incus.Sandbox{Config: incus.Config{Workspace: "/workspace/job"}}
	implementationOwner := provider.Ownership{
		JobID: job.ID, SandboxID: baseSandbox.Name(job.ID), OwnershipNonce: strings.Repeat("e", 64),
	}
	implementationMetadata := map[string]string{
		"user.dorf.owner": "sandbox", "user.dorf.job": job.ID, "user.dorf.sandbox": implementationOwner.SandboxID,
		"user.dorf.ownership_nonce": implementationOwner.OwnershipNonce,
	}
	runner := &localReviewBoundaryRunner{
		implementationName: implementationOwner.SandboxID, reviewerName: run.Sandbox.ID,
		implementationPath: implementationPath, reviewerPath: reviewerPath,
		implementation: implementationMetadata, reviewer: reviewerMetadata,
	}
	sandbox := incustest.Sandbox(runner, incus.Config{Workspace: "/workspace/job"})
	controller := ReviewController{
		Transport: incus.Adapter{Sandbox: sandbox},
		Ownership: func(_ context.Context, sandboxID string) (provider.Ownership, error) {
			if sandboxID == run.Sandbox.ID {
				return reviewOwnership(run.Sandbox), nil
			}
			return implementationOwner, nil
		},
	}
	withoutSandbox := run
	withoutSandbox.SandboxID = ""
	if err := controller.PrepareReviewCheckout(context.Background(), job, withoutSandbox); err == nil {
		t.Fatal("review checkout accepted the implementation Sandbox")
	}
	if err := controller.PrepareReviewCheckout(context.Background(), job, run); err != nil {
		t.Fatal(err)
	}
	checkout, err := controller.VerifyReviewCheckout(context.Background(), job, run)
	wantCheckout := ReviewCheckoutObservation{Revision: revision, Tree: tree}
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
