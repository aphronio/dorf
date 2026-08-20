package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/investigation"
)

func TestPrepareInvestigationSourceRetainsLocalCommittedRevisionBeforeAdmission(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		command := exec.Command("git", args...)
		command.Dir = repo
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet")
	git("config", "user.name", "Dorf Test")
	git("config", "user.email", "dorf-test@localhost")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "--quiet", "-m", "source")
	revision := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("excluded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	records := blob.Store{Root: t.TempDir()}
	source, excluded, err := prepareInvestigationSource(context.Background(), records, "", repo, "HEAD")
	if err != nil || source.Kind != investigation.SourceGitBundle || source.Revision != revision || !excluded {
		t.Fatalf("source=%#v excluded=%v err=%v", source, excluded, err)
	}
	if _, err := records.ReadVerified(source.BundleDigest, source.BundleByteSize); err != nil {
		t.Fatalf("retained bundle unavailable: %v", err)
	}
}

func TestPrepareInvestigationSourceRequiresOneSource(t *testing.T) {
	for _, test := range [][2]string{{"", ""}, {"https://example.test/repo.git", "."}} {
		if _, _, err := prepareInvestigationSource(context.Background(), blob.Store{Root: t.TempDir()}, test[0], test[1], strings.Repeat("a", 40)); err == nil {
			t.Fatalf("accepted remote=%q local=%q", test[0], test[1])
		}
	}
}
