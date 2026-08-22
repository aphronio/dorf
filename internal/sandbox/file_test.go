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

func TestReadFileViaExecReturnsExactBytesAndRefusesWorkspaceEscape(t *testing.T) {
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
	for _, relativePath := range []string{"", ".", "../outside", "/etc/passwd", "nested/../first.txt", "escape", "nested"} {
		if _, err := ReadFileViaExec(context.Background(), Ownership{}, workspace, relativePath, localExec(t, nil)); err == nil {
			t.Fatalf("ReadFileViaExec accepted %q", relativePath)
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
