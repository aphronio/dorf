package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundleLocalRevisionCapturesUnpublishedCommitAndExcludesWorkspaceChanges(t *testing.T) {
	repo := localRepository(t)
	committed := commitFile(t, repo, "tracked.txt", "committed\n")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("excluded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := bundleLocalRevision(context.Background(), repo, "HEAD", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Revision != committed || !bundle.WorkingTreeChangesExcluded || len(bundle.Contents) == 0 {
		t.Fatalf("bundle=%#v", bundle)
	}
	bundlePath := filepath.Join(t.TempDir(), "source.bundle")
	if err := os.WriteFile(bundlePath, bundle.Contents, 0o600); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	runLocalGit(t, "", "clone", "--quiet", bundlePath, checkout)
	if got := strings.TrimSpace(runLocalGit(t, checkout, "rev-parse", "HEAD")); got != committed {
		t.Fatalf("HEAD=%s want=%s", got, committed)
	}
	contents, err := os.ReadFile(filepath.Join(checkout, "tracked.txt"))
	if err != nil || string(contents) != "committed\n" {
		t.Fatalf("tracked=%q err=%v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(checkout, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked file was packaged: %v", err)
	}
}

func TestBundleLocalRevisionRejectsLFSAndOversize(t *testing.T) {
	repo := localRepository(t)
	commitFile(t, repo, "large.bin", "version https://git-lfs.github.com/spec/v1\noid sha256:"+strings.Repeat("a", 64)+"\nsize 1\n")
	if _, err := bundleLocalRevision(context.Background(), repo, "HEAD", 1<<20); err == nil || !strings.Contains(err.Error(), "Git LFS pointers") {
		t.Fatalf("LFS error=%v", err)
	}
	repo = localRepository(t)
	commitFile(t, repo, "plain.txt", strings.Repeat("not-compressible-enough\n", 100))
	if _, err := bundleLocalRevision(context.Background(), repo, "HEAD", 1); err == nil || !strings.Contains(err.Error(), "no Job was admitted") {
		t.Fatalf("size error=%v", err)
	}
}

func TestBundleLocalRevisionRejectsSubmodules(t *testing.T) {
	child := localRepository(t)
	commitFile(t, child, "child.txt", "child\n")
	parent := localRepository(t)
	runLocalGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", child, "nested")
	runLocalGit(t, parent, "commit", "--quiet", "-am", "submodule")
	if _, err := bundleLocalRevision(context.Background(), parent, "HEAD", 1<<20); err == nil || !strings.Contains(err.Error(), "contains submodules") {
		t.Fatalf("submodule error=%v", err)
	}
}

func localRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runLocalGit(t, "", "init", "--quiet", repo)
	runLocalGit(t, repo, "config", "user.name", "Dorf Test")
	runLocalGit(t, repo, "config", "user.email", "dorf-test@localhost")
	return repo
}

func commitFile(t *testing.T, repo, name, contents string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	runLocalGit(t, repo, "add", name)
	runLocalGit(t, repo, "commit", "--quiet", "-m", name)
	return strings.TrimSpace(runLocalGit(t, repo, "rev-parse", "HEAD"))
}

func runLocalGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
