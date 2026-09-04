package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunGeneratesAndChecksIndexes(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "docs", "project", "decisions")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("create source directory: %v", err)
	}
	decision := "# D001: First\n\n" +
		"- **Applicability:** current\n" +
		"- **Areas:** core\n" +
		"- **Read when:** Changing the first boundary.\n" +
		"- **Decision history:** Accepted.\n" +
		"- **Decision:** Keep the first boundary.\n"
	if err := os.WriteFile(filepath.Join(sourceDir, "D001-first.md"), []byte(decision), 0o644); err != nil {
		t.Fatalf("write decision: %v", err)
	}

	if err := run(root, false); err != nil {
		t.Fatalf("run(generate) error = %v", err)
	}
	if err := run(root, true); err != nil {
		t.Fatalf("run(check) error = %v", err)
	}
	currentPath := filepath.Join(root, "docs", "project", "decisions.md")
	if err := os.WriteFile(currentPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale index: %v", err)
	}
	if err := run(root, true); err == nil {
		t.Fatal("run(check) succeeded with a stale index")
	}
}
