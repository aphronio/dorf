package decisionindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceRoutesDecisionsByApplicabilityAndArea(t *testing.T) {
	dir := t.TempDir()
	writeDecision(t, dir, "D001-first.md", "D001", "First", "current", "product, core")
	writeDecision(t, dir, "D002-second.md", "D002", "Second", "partial", "core")
	writeDecision(t, dir, "D003-third.md", "D003", "Third", "historical", "deployment")

	current, historical, err := Source(dir)
	if err != nil {
		t.Fatalf("Source() error = %v", err)
	}
	currentText := string(current)
	historicalText := string(historical)
	if count := strings.Count(currentText, "decisions/D001-first.md"); count != 2 {
		t.Fatalf("D001 current link count = %d, want 2", count)
	}
	if !strings.Contains(currentText, "decisions/D002-second.md") {
		t.Fatal("current index does not contain partial D002")
	}
	if strings.Contains(currentText, "D003-third.md") {
		t.Fatal("current index contains historical D003")
	}
	if !strings.Contains(historicalText, "D003-third.md") {
		t.Fatal("historical index does not contain D003")
	}
	if strings.Contains(historicalText, "D001-first.md") {
		t.Fatal("historical index contains current D001")
	}
}

func TestSourceRejectsInvalidDecisionSets(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
		want  string
	}{
		{
			name: "unknown area",
			setup: func(t *testing.T, dir string) {
				writeDecision(t, dir, "D001-first.md", "D001", "First", "current", "unknown")
			},
			want: "invalid Area",
		},
		{
			name: "sequence gap",
			setup: func(t *testing.T, dir string) {
				writeDecision(t, dir, "D001-first.md", "D001", "First", "current", "core")
				writeDecision(t, dir, "D003-third.md", "D003", "Third", "current", "core")
			},
			want: "D002 is required",
		},
		{
			name: "heading mismatch",
			setup: func(t *testing.T, dir string) {
				writeDecision(t, dir, "D001-first.md", "D002", "First", "current", "core")
			},
			want: "does not match filename",
		},
		{
			name: "unexpected filename",
			setup: func(t *testing.T, dir string) {
				writeDecision(t, dir, "D001.md", "D001", "First", "current", "core")
			},
			want: "decision filenames must match",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			test.setup(t, dir)
			_, _, err := Source(dir)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Source() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func writeDecision(t *testing.T, dir, filename, id, title, applicability, decisionAreas string) {
	t.Helper()
	content := "# " + id + ": " + title + "\n\n" +
		"- **Applicability:** " + applicability + "\n" +
		"- **Areas:** " + decisionAreas + "\n" +
		"- **Read when:** Changing the tested boundary.\n" +
		"- **Decision history:** Accepted.\n" +
		"- **Decision:** Keep the test record.\n"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write decision: %v", err)
	}
}
