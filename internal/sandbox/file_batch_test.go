package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFilesViaExecPreservesBytesMissingAndEmpty(t *testing.T) {
	workspace := t.TempDir()
	contents := []byte{0, 255, '\n', 'x'}
	for name, data := range map[string][]byte{"binary": contents, "empty": {}} {
		if err := os.WriteFile(filepath.Join(workspace, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ReadFilesViaExec(context.Background(), Ownership{}, workspace, []string{"binary", "missing", "empty"}, len(contents), localExec(t, nil))
	if err != nil || !bytes.Equal(files["binary"], contents) || len(files) != 2 {
		t.Fatalf("batch did not preserve exact files: entries=%d err=%v", len(files), err)
	}
	if empty, exists := files["empty"]; !exists || len(empty) != 0 {
		t.Fatal("empty file confused with missing file")
	}
	if _, err := ReadFilesViaExec(context.Background(), Ownership{}, workspace, []string{"binary"}, len(contents)-1, localExec(t, nil)); err == nil {
		t.Fatal("accepted oversized file")
	}
}

func TestReadFilesViaExecRejectsUnsafePathsAndRetainsOpenedDescriptor(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	for _, name := range []string{"", "../escape", "nested/../file"} {
		if _, err := ReadFilesViaExec(context.Background(), Ownership{}, workspace, []string{name}, 128, nil); !errors.Is(err, ErrInvalidFilePath) {
			t.Fatalf("unsafe path %q reached transport: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"linked": outside, "file-link": filepath.Join(outside, "file"), "dangling": filepath.Join(outside, "missing")} {
		if err := os.Symlink(target, filepath.Join(workspace, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(workspace, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"linked/file", "file-link", "dangling", "directory"} {
		if _, err := ReadFilesViaExec(context.Background(), Ownership{}, workspace, []string{name}, 128, localExec(t, nil)); !errors.Is(err, ErrFileUnavailable) {
			t.Fatalf("unsafe file %q accepted: %v", name, err)
		}
	}
	racePath := filepath.Join(workspace, "race")
	if err := os.WriteFile(racePath, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	realHead, err := exec.LookPath("head")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	swap := fmt.Sprintf("#!/bin/sh\nrm -f -- %q\nln -s -- %q %q\nexec %q \"$@\"\n", racePath, filepath.Join(outside, "file"), racePath, realHead)
	if err := os.WriteFile(filepath.Join(bin, "head"), []byte(swap), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	files, err := ReadFilesViaExec(context.Background(), Ownership{}, workspace, []string{"race"}, 128, localExec(t, nil))
	if err != nil || string(files["race"]) != "retained" {
		t.Fatalf("pathname replacement changed descriptor read: %v", err)
	}
}

func TestReadFilesViaExecRejectsTruncatedOrUnboundedTransportOutput(t *testing.T) {
	for _, output := range []string{"", "D", "D\nD\n", "X\n", "D!\n", "D" + strings.Repeat("A", 100) + "\n"} {
		runner := func(context.Context, Ownership, []byte, ...string) (Result, error) {
			return Result{Stdout: output}, nil
		}
		if _, err := ReadFilesViaExec(context.Background(), Ownership{}, "/workspace/job", []string{"file"}, 4, runner); err == nil {
			t.Fatal("accepted malformed or unbounded response")
		}
	}
}
