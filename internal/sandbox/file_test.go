package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

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
