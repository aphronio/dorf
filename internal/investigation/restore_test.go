package investigation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type localRestoreSandbox struct {
	root      string
	workspace string
}

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
	source := Source{
		JobID: job.ID, Kind: SourceGitBundle, Revision: bundle.Revision,
		BundleDigest: digest, BundleByteSize: int64(len(bundle.Contents)),
	}
	sandbox := localRestoreSandbox{root: t.TempDir(), workspace: "/workspace/job"}
	restore := RetainedRestore{Transport: sandbox, Workspace: sandbox.workspace}
	for attempt := 0; attempt < 2; attempt++ {
		if err := restore.Reconcile(context.Background(), job, owned, source, bundle.Contents); err != nil {
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
	source := Source{
		JobID: job.ID, Kind: SourceGitBundle, Revision: revision,
		BundleDigest: strings.Repeat("c", 64), BundleByteSize: 1,
	}
	restore := RetainedRestore{Transport: sandbox, Workspace: sandbox.workspace}
	err := restore.Reconcile(context.Background(), job, owned, source, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("restore error=%v", err)
	}
	contents, readErr := os.ReadFile(filepath.Join(workspace, "foreign.txt"))
	if readErr != nil || string(contents) != "do not delete\n" {
		t.Fatalf("foreign contents=%q err=%v", contents, readErr)
	}
}
