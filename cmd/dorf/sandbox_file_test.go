package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

type sandboxFileTestStore struct {
	job       core.Job
	sandboxes map[string]core.Sandbox
}

func (s sandboxFileTestStore) Job(context.Context, string) (core.Job, error) { return s.job, nil }
func (s sandboxFileTestStore) Sandbox(_ context.Context, id string) (core.Sandbox, error) {
	owned, ok := s.sandboxes[id]
	if !ok {
		return core.Sandbox{}, errors.New("Sandbox not found")
	}
	return owned, nil
}
func (sandboxFileTestStore) EnsureSandbox(context.Context, string, string) (core.Sandbox, error) {
	return core.Sandbox{}, nil
}
func (sandboxFileTestStore) JobTasks(context.Context, string) ([]core.JobTask, error) {
	return nil, nil
}
func (sandboxFileTestStore) CleanupRequests(context.Context) ([]string, error) { return nil, nil }
func (sandboxFileTestStore) WithJobFence(_ context.Context, _ string, run func() error) error {
	return run()
}
func (sandboxFileTestStore) AttachJobTask(context.Context, string, string, string, string) error {
	return nil
}
func (sandboxFileTestStore) RequestCleanup(context.Context, string) error { return nil }
func (sandboxFileTestStore) AttachCleanupTask(context.Context, string, string, string, string) error {
	return nil
}
func (sandboxFileTestStore) GetOrCreateSandboxAction(context.Context, string, core.ActionKind) (core.Action, error) {
	return core.Action{}, nil
}
func (sandboxFileTestStore) RecordSandboxActionSuccess(context.Context, string) error { return nil }
func (sandboxFileTestStore) RecordSandboxProfileUnavailable(context.Context, string, string, string, error) error {
	return nil
}
func (sandboxFileTestStore) SetCleanupAttention(context.Context, string, string) error { return nil }

type sandboxFileTestRuntime struct{ contents []byte }

func (r sandboxFileTestRuntime) ResolveSandbox(_ context.Context, profile string) (core.SandboxRuntime, error) {
	return core.SandboxRuntime{SandboxProfile: profile, Files: r}, nil
}
func (r sandboxFileTestRuntime) ReadSandboxFile(_ context.Context, _ core.Job, _ core.Sandbox, _ string) ([]byte, error) {
	return append([]byte(nil), r.contents...), nil
}

func TestSandboxFileGetWritesExactBytesToExplicitOutput(t *testing.T) {
	job := core.Job{ID: "job-cli-file", SandboxProfile: "profile", AdmissionOpen: true, CleanupState: core.CleanupPending}
	defaultID := core.MainSandboxName(job.ID)
	namedID := core.NamedSandboxID(job.ID, "review")
	store := sandboxFileTestStore{job: job, sandboxes: map[string]core.Sandbox{
		defaultID: {ID: defaultID, JobID: job.ID}, namedID: {ID: namedID, JobID: job.ID},
	}}
	want := []byte{0, 1, '\n', 255}
	application := core.Application{Store: store, SandboxRuntimes: sandboxFileTestRuntime{contents: want}}
	directory := t.TempDir()
	outside := filepath.Join(directory, "outside.bin")
	if err := os.WriteFile(outside, []byte("preserve outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "result.bin")
	if err := os.Symlink(outside, output); err != nil {
		t.Fatal(err)
	}
	if err := sandboxFileGet(context.Background(), application, []string{namedID, "nested/result.bin", "--output", output}, &bytes.Buffer{}); err != nil {
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
	if err := sandboxFileGet(context.Background(), application, []string{"", "result.bin", "--output=-"}, &bytes.Buffer{}); err == nil {
		t.Fatal("empty Sandbox identity was accepted")
	}
	var stdout bytes.Buffer
	if err := sandboxFileGet(context.Background(), application, []string{"--output=-", defaultID, "--", "-result.bin"}, &stdout); err != nil || !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("stdout=%v want=%v err=%v", stdout.Bytes(), want, err)
	}
}
