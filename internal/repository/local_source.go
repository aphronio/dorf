package repository

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const MaxLocalBundleBytes int64 = 128 << 20

type LocalBundle struct {
	Revision                   string
	Contents                   []byte
	WorkingTreeChangesExcluded bool
}

// BundleLocalRevision captures exactly one committed local Git history. Host
// working-tree and index changes are deliberately excluded.
func BundleLocalRevision(ctx context.Context, repositoryPath, revision string) (LocalBundle, error) {
	return bundleLocalRevision(ctx, repositoryPath, revision, MaxLocalBundleBytes)
}

func bundleLocalRevision(ctx context.Context, repositoryPath, revision string, maxBytes int64) (LocalBundle, error) {
	if maxBytes <= 0 {
		return LocalBundle{}, fmt.Errorf("local repository bundle limit must be positive")
	}
	repositoryPath = strings.TrimSpace(repositoryPath)
	if repositoryPath == "" {
		return LocalBundle{}, fmt.Errorf("local repository path is required")
	}
	root, err := runGitOutput(ctx, "", "-C", repositoryPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return LocalBundle{}, fmt.Errorf("resolve local Git repository: %w", err)
	}
	root, err = filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return LocalBundle{}, fmt.Errorf("resolve local Git repository path: %w", err)
	}
	if strings.TrimSpace(revision) == "" {
		revision = "HEAD"
	}
	oid, err := runGitOutput(ctx, root, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return LocalBundle{}, fmt.Errorf("resolve local committed Revision %q: %w", revision, err)
	}
	oid = strings.TrimSpace(oid)

	tree, err := runGitOutput(ctx, root, "ls-tree", "-r", oid)
	if err != nil {
		return LocalBundle{}, fmt.Errorf("inspect local committed Revision %s: %w", oid, err)
	}
	for _, line := range strings.Split(tree, "\n") {
		if strings.HasPrefix(line, "160000 commit ") {
			return LocalBundle{}, fmt.Errorf("local committed Revision %s contains submodules; local bundle admission does not package submodule repositories", oid)
		}
	}
	lfs, lfsErr := runGitCommand(ctx, root, "grep", "-I", "-l", "-e", "^version https://git-lfs.github.com/spec/v1$", oid, "--")
	switch {
	case lfsErr == nil && strings.TrimSpace(lfs) != "":
		return LocalBundle{}, fmt.Errorf("local committed Revision %s contains Git LFS pointers; local bundle admission does not package LFS objects", oid)
	case lfsErr == nil:
	case isGitNoMatches(lfsErr):
	default:
		return LocalBundle{}, fmt.Errorf("inspect local Revision for Git LFS pointers: %w", lfsErr)
	}

	status, err := runGitOutput(ctx, root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return LocalBundle{}, fmt.Errorf("inspect local working tree: %w", err)
	}
	temporary, err := os.MkdirTemp("", "dorf-local-source-")
	if err != nil {
		return LocalBundle{}, err
	}
	defer os.RemoveAll(temporary)
	bare := filepath.Join(temporary, "source.git")
	if _, err := runGitOutput(ctx, "", "init", "--bare", "--quiet", bare); err != nil {
		return LocalBundle{}, fmt.Errorf("prepare local bundle repository: %w", err)
	}
	if _, err := runGitOutput(ctx, bare, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", root, oid); err != nil {
		return LocalBundle{}, fmt.Errorf("copy exact local Revision into bundle repository: %w", err)
	}
	if _, err := runGitOutput(ctx, bare, "update-ref", "refs/heads/dorf-source", "FETCH_HEAD"); err != nil {
		return LocalBundle{}, fmt.Errorf("name exact local Revision for bundle export: %w", err)
	}
	if _, err := runGitOutput(ctx, bare, "symbolic-ref", "HEAD", "refs/heads/dorf-source"); err != nil {
		return LocalBundle{}, fmt.Errorf("select exact local Revision for bundle export: %w", err)
	}
	bundlePath := filepath.Join(temporary, "source.bundle")
	if _, err := runGitOutput(ctx, bare, "bundle", "create", bundlePath, "HEAD"); err != nil {
		return LocalBundle{}, fmt.Errorf("create local repository bundle: %w", err)
	}
	if _, err := runGitOutput(ctx, bare, "bundle", "verify", bundlePath); err != nil {
		return LocalBundle{}, fmt.Errorf("verify local repository bundle: %w", err)
	}
	info, err := os.Stat(bundlePath)
	if err != nil {
		return LocalBundle{}, err
	}
	if info.Size() > maxBytes {
		return LocalBundle{}, fmt.Errorf("local repository bundle is %s bytes; limit is %s; no Job was admitted", strconv.FormatInt(info.Size(), 10), strconv.FormatInt(maxBytes, 10))
	}
	contents, err := os.ReadFile(bundlePath)
	if err != nil {
		return LocalBundle{}, err
	}
	return LocalBundle{Revision: oid, Contents: contents, WorkingTreeChangesExcluded: strings.TrimSpace(status) != ""}, nil
}

func isGitNoMatches(err error) bool {
	exit, ok := err.(*exec.ExitError)
	return ok && exit.ExitCode() == 1
}

func runGitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	output, err := runGitCommand(ctx, directory, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(output))
	}
	return output, nil
}

func runGitCommand(ctx context.Context, directory string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	if directory != "" {
		command.Dir = directory
	}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	return output.String(), err
}
