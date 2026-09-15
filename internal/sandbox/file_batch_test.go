package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
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

func TestDecodeFileBatchDistinguishesOversizeFromMalformedOutput(t *testing.T) {
	const maxBytes = 4
	encode := func(contents []byte) string { return "D" + base64.StdEncoding.EncodeToString(contents) + "\n" }

	files, err := decodeFileBatch(encode([]byte("1234")), []string{"file"}, maxBytes)
	if err != nil || string(files["file"]) != "1234" {
		t.Fatalf("exact-limit decode=%q err=%v", files["file"], err)
	}
	if files, err := decodeFileBatch(encode([]byte("12345")), []string{"file"}, maxBytes); files != nil || !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("valid oversize decode=%v err=%v", files, err)
	}

	tooLong := "D" + strings.Repeat("A", base64.StdEncoding.EncodedLen(maxBytes+1)+1) + "\n"
	for _, output := range []string{
		"D!!!!\n",
		encode([]byte("1234"))[:len(encode([]byte("1234")))-1],
		"M\nM\n",
		tooLong,
	} {
		if files, err := decodeFileBatch(output, []string{"file"}, maxBytes); files != nil || err == nil || errors.Is(err, ErrFileTooLarge) {
			t.Fatalf("malformed output files=%v err=%v", files, err)
		}
	}
}

func TestReadFilesViaExecBoundsGrowingOpenedDescriptor(t *testing.T) {
	workspace := t.TempDir()
	filePath := filepath.Join(workspace, "growing")
	if err := os.WriteFile(filePath, []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
	realHead, err := exec.LookPath("head")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	started := filepath.Join(bin, "started")
	release := filepath.Join(bin, "release")
	observed := filepath.Join(bin, "observed")
	wrapper := fmt.Sprintf("#!/bin/sh\n: > %q\nwhile test ! -e %q; do sleep 0.01; done\n%q \"$@\" | tee %q\n", started, release, realHead, observed)
	if err := os.WriteFile(filepath.Join(bin, "head"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	const maxBytes = 64
	type readResult struct {
		files map[string][]byte
		err   error
	}
	runner := localExec(t, nil)
	done := make(chan readResult, 1)
	go func() {
		files, err := ReadFilesViaExec(context.Background(), Ownership{}, workspace, []string{"growing"}, maxBytes, runner)
		done <- readResult{files: files, err: err}
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filePath, bytes.Repeat([]byte("x"), maxBytes+20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.files != nil || !errors.Is(result.err, ErrFileTooLarge) {
		t.Fatalf("growing file result=%v err=%v", result.files, result.err)
	}
	readBytes, err := os.ReadFile(observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(readBytes) != maxBytes+1 {
		t.Fatalf("source bytes read=%d want=%d", len(readBytes), maxBytes+1)
	}
}
