package persistence

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type localPersistenceSandbox struct {
	provider.Sandbox
	root    string
	started chan struct{}
}

// Local integration fixture for the same command boundary used by providers.
func (s localPersistenceSandbox) Run(ctx context.Context, _ provider.Ownership, request provider.RunRequest) (provider.RunResult, error) {
	args := append([]string(nil), request.Args...)
	for i, arg := range args {
		if strings.HasPrefix(arg, "/var/tmp/dorf-restic") {
			args[i] = filepath.Join(s.root, strings.TrimPrefix(arg, "/var/tmp/"))
		}
	}
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return provider.RunResult{}, err
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	for key, value := range request.Env {
		command.Env = append(command.Env, key+"="+value)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		return provider.RunResult{}, err
	}
	if s.started != nil {
		s.started <- struct{}{}
	}
	err := command.Wait()
	result := provider.RunResult{Result: provider.Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: command.ProcessState.ExitCode()}, Stopped: true}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		err = nil
	}
	return result, err
}

type commandFixture struct {
	provider.Sandbox
	commands []provider.RunRequest
	result   provider.RunResult
	err      error
}

func (s *commandFixture) Run(_ context.Context, _ provider.Ownership, c provider.RunRequest) (provider.RunResult, error) {
	s.commands = append(s.commands, c)
	return s.result, s.err
}
func testDriver(s provider.Sandbox) Driver {
	return Driver{Sandbox: s, credentials: func(context.Context, provider.Ownership, RepositoryAccess) (repositoryCredentials, error) {
		return repositoryCredentials{Repository: "synthetic", Password: "synthetic", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
}
func TestResticSummaryAndFailedCapture(t *testing.T) {
	s := &commandFixture{result: provider.RunResult{Stopped: true, Result: provider.Result{Stdout: fmt.Sprintf(`{"message_type":"summary","snapshot_id":%q,"data_added":4096,"data_added_packed":1024}`, strings.Repeat("a", 64))}}}
	d := testDriver(s)
	got, err := d.Backup(t.Context(), provider.Ownership{}, []string{"/workspace"}, nil)
	if err != nil || got.SnapshotID == "" || got.DataAdded != 4096 || got.DataAddedPacked != 1024 {
		t.Fatalf("summary=%+v error=%v", got, err)
	}
	for _, arg := range s.commands[0].Args {
		if arg == "--no-lock" {
			t.Fatal("ordinary backup bypassed upstream lock")
		}
	}
	s.result.ExitCode = 3
	got, err = d.Backup(t.Context(), provider.Ownership{}, []string{"/workspace"}, nil)
	if err == nil || got.SnapshotID != "" {
		t.Fatal("partial backup was published")
	}
	s.err = errors.New("synthetic private command diagnostics")
	_, err = d.Backup(t.Context(), provider.Ownership{}, []string{"/workspace"}, nil)
	if err == nil || strings.Contains(err.Error(), "private command") {
		t.Fatal("provider details leaked")
	}
}
func TestResticInitOnlyOnMissingRepository(t *testing.T) {
	s := &commandFixture{result: provider.RunResult{Result: provider.Result{ExitCode: 12}}}
	if err := testDriver(s).InitializeRepository(t.Context(), provider.Ownership{}); err == nil || len(s.commands) != 1 {
		t.Fatal("init retried an authorization failure")
	}
	s.commands = nil
	s.result.ExitCode = 10
	_ = testDriver(s).InitializeRepository(t.Context(), provider.Ownership{})
	if len(s.commands) != 2 || s.commands[1].Args[len(s.commands[1].Args)-1] != "init" {
		t.Fatal("missing repository not initialized")
	}
}
func testAttempt(t *testing.T, prefix string) string {
	t.Helper()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(nonce[:])
}
