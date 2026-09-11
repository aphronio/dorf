package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReadFileViaExecReturnsExactBytesAndRefusesSymlinks(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantText := []byte("exact text\n")
	wantBinary := []byte{0, 1, '\n', 255}
	if err := os.WriteFile(filepath.Join(workspace, "first.txt"), wantText, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "second.bin"), wantBinary, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	for relativePath, want := range map[string][]byte{"first.txt": wantText, "nested/second.bin": wantBinary} {
		got, err := ReadFileViaExec(context.Background(), Ownership{}, workspace, relativePath, localExec(t, nil))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("ReadFileViaExec(%q)=%v want=%v err=%v", relativePath, got, want, err)
		}
	}
	racePath := filepath.Join(workspace, "race.bin")
	raceBytes := []byte{7, 0, 255, '\n'}
	if err := os.WriteFile(racePath, raceBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	realBase64, err := exec.LookPath("base64")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	swapBase64 := fmt.Sprintf("#!/bin/sh\nrm -f -- %q\nln -s -- %q %q\nexec %q \"$@\"\n", racePath, outside, racePath, realBase64)
	if err := os.WriteFile(filepath.Join(bin, "base64"), []byte(swapBase64), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := ReadFileViaExec(context.Background(), Ownership{}, workspace, "race.bin", localExec(t, nil))
	if err != nil || !bytes.Equal(got, raceBytes) {
		t.Fatalf("concurrent pathname replacement read=%v want=%v err=%v", got, raceBytes, err)
	}
	if info, err := os.Lstat(racePath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("race replacement did not occur: info=%v err=%v", info, err)
	}
	for _, relativePath := range []string{"", ".", "..", "/", "~", "../outside", "nested/../first.txt"} {
		if _, err := ReadFileViaExec(context.Background(), Ownership{}, workspace, relativePath, localExec(t, nil)); !errors.Is(err, ErrInvalidFilePath) {
			t.Fatalf("ReadFileViaExec invalid path %q error=%v", relativePath, err)
		}
	}
	for _, relativePath := range []string{"escape", "nested", "missing"} {
		if _, err := ReadFileViaExec(context.Background(), Ownership{}, workspace, relativePath, localExec(t, nil)); !errors.Is(err, ErrFileUnavailable) {
			t.Fatalf("ReadFileViaExec unavailable path %q error=%v", relativePath, err)
		}
	}
}

func TestPutFileViaExecReconcilesExactBinaryBytes(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "nested", "input.bundle")
	contents := []byte{'g', 'i', 't', 0, '\n', 0xff}
	runner := localExec(t, nil)
	if err := PutFileViaExec(context.Background(), Ownership{}, destination, contents, runner); err != nil {
		t.Fatal(err)
	}
	if err := PutFileViaExec(context.Background(), Ownership{}, destination, contents, runner); err != nil {
		t.Fatal(err)
	}
	observed, err := exec.Command("bash", "-c", "cat -- \"$1\"", "test-read", destination).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(observed, contents) {
		t.Fatalf("contents=%v want=%v", observed, contents)
	}
}

func TestPutFileViaExecConvergesAfterLostSuccess(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "input.bundle")
	contents := []byte("complete bundle")
	lost := true
	runner := localExec(t, func(result Result, err error) (Result, error) {
		if lost && err == nil && result.ExitCode == 0 {
			lost = false
			return Result{}, errors.New("lost response")
		}
		return result, err
	})
	if err := PutFileViaExec(context.Background(), Ownership{}, destination, contents, runner); err == nil {
		t.Fatal("first response was not lost")
	}
	if err := PutFileViaExec(context.Background(), Ownership{}, destination, contents, runner); err != nil {
		t.Fatal(err)
	}
}

func localExec(t *testing.T, after func(Result, error) (Result, error)) ExecFunc {
	t.Helper()
	return func(ctx context.Context, _ Ownership, input []byte, argv ...string) (Result, error) {
		command := exec.CommandContext(ctx, argv[0], argv[1:]...)
		command.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
		if exit, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exit.ExitCode()
			err = nil
		}
		if after != nil {
			return after(result, err)
		}
		return result, err
	}
}

func TestSandboxFileWritePreservesExistingDefaultsAndRejectsSymlinks(t *testing.T) {
	workspace := t.TempDir()
	runner := localExec(t, nil)
	ctx := context.Background()
	put := func(name, content string, absent bool) error {
		return WriteFileViaExec(ctx, Ownership{}, workspace, name, []byte(content), absent, runner)
	}
	for _, step := range []struct {
		content string
		absent  bool
		want    string
	}{
		{"first", true, "first"}, {"new default", true, "first"}, {"edited", false, "edited"}, {"", false, ""}, {"default", true, ""},
	} {
		if err := put("AGENTS.md", step.content, step.absent); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(workspace, "AGENTS.md"))
		if err != nil || string(content) != step.want {
			t.Fatalf("file=%q err=%v want=%q", content, err, step.want)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "SOUL.md")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SOUL.md", "../outside", "/tmp/../outside"} {
		if err := put(name, "replacement", false); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "private" {
		t.Fatalf("outside=%q err=%v", content, err)
	}
}

func TestSandboxFilesSupportAbsoluteHomeAndNestedPaths(t *testing.T) {
	workspace, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	ctx, runner := context.Background(), localExec(t, nil)
	contents := []byte{'a', 0, '\n', 255}
	for name, target := range map[string]string{
		"nested/file.bin":                           filepath.Join(workspace, "nested", "file.bin"),
		"~/.config/agent0/access.json":              filepath.Join(home, ".config", "agent0", "access.json"),
		filepath.Join(home, "absolute", "file.bin"): filepath.Join(home, "absolute", "file.bin"),
	} {
		if err := WriteFileViaExec(ctx, Ownership{}, workspace, name, contents, false, runner); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
		info, err := os.Stat(target)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file %q permissions: %v, %v", target, info, err)
		}
		parent, err := os.Stat(filepath.Dir(target))
		if err != nil || parent.Mode().Perm() != 0o700 {
			t.Fatalf("parent %q permissions: %v, %v", target, parent, err)
		}
		got, err := ReadFileViaExec(ctx, Ownership{}, workspace, name, runner)
		if err != nil || !bytes.Equal(got, contents) {
			t.Fatalf("read %q: %v, %v", name, got, err)
		}
	}
	if err := os.Symlink(home, filepath.Join(workspace, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileViaExec(ctx, Ownership{}, workspace, "linked/new/file", contents, false, runner); err == nil {
		t.Fatal("accepted a symlink parent")
	}
	if _, err := os.Stat(filepath.Join(home, "new")); !os.IsNotExist(err) {
		t.Fatalf("rejected write created directories through a symlink: %v", err)
	}
}
