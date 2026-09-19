package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

const DefaultResticPath = "/nix/var/nix/profiles/dorf-tools/bin/restic"
const defaultOperation = 120 * time.Second
const maxProtectedPaths = 32

var snapshotPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Driver uses the provider's bounded command API. Successful backups become
// checkpoints only after the caller verifies native consistency and publishes.
type Driver struct {
	Sandbox          provider.Sandbox
	Repository       R2Repository
	ResticPath       string
	OperationTimeout time.Duration
	credentials      func(context.Context, provider.Ownership, RepositoryAccess) (repositoryCredentials, error)
}

type Result struct {
	SnapshotID   string
	Duration     time.Duration
	FilesNew     uint64
	FilesChanged uint64
	// Logical and packed new-blob sizes exclude failed transfers and HTTP overhead.
	DataAdded          uint64
	DataAddedPacked    uint64
	DataAddedKnown     bool
	Cancelled          bool
	RemoteStopped      bool
	RemoteStopDuration time.Duration
}

func (d Driver) InitializeRepository(ctx context.Context, owner provider.Ownership) error {
	result, _, err := d.run(ctx, owner, RepositoryReadWrite, "cat", "config", "--no-lock")
	if err != nil {
		return err
	}
	if result.ExitCode == 10 { // Upstream's repository-not-found exit code.
		result, _, err = d.run(ctx, owner, RepositoryReadWrite, "init")
	}
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("restic initialization failed (exit %d)", result.ExitCode)
	}
	return nil
}

func (d Driver) Backup(ctx context.Context, owner provider.Ownership, paths, excludes []string) (Result, error) {
	if err := validateProtectedPaths(paths); err != nil {
		return Result{}, err
	}
	args := []string{"backup", "--json", "--no-scan"}
	for _, pattern := range excludes {
		args = append(args, "--exclude", pattern)
	}
	args = append(append(args, "--"), paths...)
	observed, result, err := d.run(ctx, owner, RepositoryReadWrite, args...)
	if err != nil {
		return result, err
	}
	if observed.ExitCode != 0 {
		return result, fmt.Errorf("restic backup failed (exit %d)", observed.ExitCode)
	}
	var summary struct {
		MessageType     string `json:"message_type"`
		SnapshotID      string `json:"snapshot_id"`
		FilesNew        uint64 `json:"files_new"`
		FilesChanged    uint64 `json:"files_changed"`
		DataAdded       uint64 `json:"data_added"`
		DataAddedPacked uint64 `json:"data_added_packed"`
	}
	for _, line := range strings.Split(observed.Stdout, "\n") {
		if json.Unmarshal([]byte(line), &summary) == nil && summary.MessageType == "summary" {
			if !snapshotPattern.MatchString(summary.SnapshotID) {
				break
			}
			result.SnapshotID, result.FilesNew, result.FilesChanged = summary.SnapshotID, summary.FilesNew, summary.FilesChanged
			result.DataAdded, result.DataAddedPacked, result.DataAddedKnown = summary.DataAdded, summary.DataAddedPacked, true
			return result, nil
		}
	}
	return result, fmt.Errorf("restic backup omitted its full snapshot ID")
}

func (d Driver) Restore(ctx context.Context, owner provider.Ownership, snapshot, target string) (Result, error) {
	if !snapshotPattern.MatchString(snapshot) || !path.IsAbs(target) || path.Clean(target) != target {
		return Result{}, fmt.Errorf("restore requires an exact snapshot ID and absolute target")
	}
	observed, result, err := d.run(ctx, owner, RepositoryReadOnly, "restore", snapshot, "--no-lock", "--target", target)
	if err != nil {
		return result, err
	}
	if observed.ExitCode != 0 {
		return result, fmt.Errorf("restic restore failed (exit %d)", observed.ExitCode)
	}
	return result, nil
}

func (d Driver) run(ctx context.Context, owner provider.Ownership, access RepositoryAccess, args ...string) (provider.RunResult, Result, error) {
	timeout := d.OperationTimeout
	if timeout == 0 {
		timeout = defaultOperation
	}
	if timeout < time.Second || timeout > 30*time.Minute {
		return provider.RunResult{}, Result{}, fmt.Errorf("invalid backup timeout")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	executable := d.ResticPath
	if executable == "" {
		executable = DefaultResticPath
	}
	if !path.IsAbs(executable) || path.Clean(executable) != executable {
		return provider.RunResult{}, Result{}, fmt.Errorf("restic requires an absolute executable path")
	}
	issue := d.credentials
	if issue == nil {
		issue = d.Repository.credentials
	}
	credentials, err := issue(ctx, owner, access)
	if err != nil {
		return provider.RunResult{}, Result{}, err
	}
	if time.Until(credentials.ExpiresAt) <= timeout {
		return provider.RunResult{}, Result{}, fmt.Errorf("repository credential expires before command deadline")
	}
	digest := sha256.Sum256([]byte(credentials.Repository))
	environment := map[string]string{
		"HOME": "/root", "AWS_DEFAULT_REGION": "auto", "GOMAXPROCS": "1",
		"AWS_ACCESS_KEY_ID": credentials.AccessKeyID, "AWS_SECRET_ACCESS_KEY": credentials.SecretAccessKey,
		"AWS_SESSION_TOKEN": credentials.SessionToken, "RESTIC_REPOSITORY": credentials.Repository, "RESTIC_PASSWORD": credentials.Password,
	}
	// No-fork flock and nice both exec the command. The provider's PID is restic's
	// PID, and a retry cannot overlap a command whose termination was uncertain.
	command := []string{"/usr/bin/flock", "--nonblock", "--no-fork", "/var/tmp/dorf-restic.lock", "/usr/bin/nice", "-n", "10", executable,
		"--cache-dir", fmt.Sprintf("/var/tmp/dorf-restic-cache/%x", digest)}
	started := time.Now()
	observed, err := d.Sandbox.Run(ctx, owner, provider.RunRequest{Args: append(command, args...), Env: environment, Timeout: timeout})
	result := Result{Duration: time.Since(started), Cancelled: ctx.Err() != nil, RemoteStopped: observed.Stopped, RemoteStopDuration: observed.StopDuration}
	if err != nil {
		return observed, result, fmt.Errorf("restic command failed: remote_stopped=%t: %w", observed.Stopped, commandFailure(ctx))
	}
	if ctx.Err() != nil {
		return observed, result, ctx.Err()
	}
	return observed, result, nil
}

func validateProtectedPaths(paths []string) error {
	if len(paths) == 0 || len(paths) > maxProtectedPaths {
		return fmt.Errorf("backup requires between one and %d paths", maxProtectedPaths)
	}
	for _, candidate := range paths {
		if !path.IsAbs(candidate) || path.Clean(candidate) != candidate || candidate == "/" || strings.HasPrefix(candidate, "/var/tmp/dorf-") {
			return fmt.Errorf("backup path must be absolute and outside temporary control files")
		}
	}
	return nil
}

// Provider errors can contain command output; keep guest paths and credentials
// out of checkpoint telemetry while preserving cancellation classification.
func commandFailure(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("provider command observation failed")
}
