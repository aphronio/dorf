package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadSandboxFileWritesExactBytesToExplicitOutput(t *testing.T) {
	const sandboxID = "job-cli-file--review"
	want := []byte{0, 1, '\n', 255}
	read := func(context.Context, string) ([]byte, error) { return want, nil }
	directory := t.TempDir()
	outside := filepath.Join(directory, "outside.bin")
	if err := os.WriteFile(outside, []byte("preserve outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "result.bin")
	if err := os.Symlink(outside, output); err != nil {
		t.Fatal(err)
	}
	if err := downloadSandboxFile(context.Background(), sandboxID, "nested/result.bin", output, &bytes.Buffer{}, read); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("output=%v want=%v err=%v", got, want, err)
	}
	info, err := os.Lstat(output)
	outsideBytes, outsideErr := os.ReadFile(outside)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || outsideErr != nil || string(outsideBytes) != "preserve outside" {
		t.Fatalf("atomic output info=%v err=%v outside=%q outsideErr=%v", info, err, outsideBytes, outsideErr)
	}
	if _, _, _, err := parseSandboxFileGet([]string{"", "result.bin", "--output=-"}); err == nil {
		t.Fatal("empty Sandbox identity was accepted")
	}
	if _, _, _, err := parseSandboxFileGet([]string{sandboxID, "/workspace/job/result.bin", "--output=-"}); err == nil ||
		!strings.Contains(err.Error(), "workspace-relative") || !strings.Contains(err.Error(), `use "result.bin"`) {
		t.Fatalf("absolute Sandbox path guidance=%v", err)
	}
	var stdout bytes.Buffer
	sandbox, relative, destination, err := parseSandboxFileGet([]string{"--output=-", sandboxID, "--", "-result.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := downloadSandboxFile(context.Background(), sandbox, relative, destination, &stdout, read); err != nil || !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("stdout=%v want=%v err=%v", stdout.Bytes(), want, err)
	}
}
