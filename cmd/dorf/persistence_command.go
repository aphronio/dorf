package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const checkpointPinTimeout = 30 * time.Second
const checkpointPinOutputLimit = 64 << 10

// Checkpoints are operator-visible recovery facts. The operator chooses an
// exact snapshot; storage order or an object-store alias never chooses it.
func checkpointCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("checkpoint requires show, boundary, capture-pin, recover, branch, branch-status, or release-branch")
	}
	if args[0] == "branch-status" || args[0] == "release-branch" {
		return branchStatusCommand(ctx, store, tasks, args, stdout)
	}
	session, err := store.Session(ctx, args[1])
	if err != nil {
		return err
	}
	switch args[0] {
	case "boundary":
		return checkpointBoundaryCommand(ctx, store, session, args, stdout)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("checkpoint show requires exactly one Session")
		}
		checkpoints, err := store.ListCheckpoints(ctx, core.MainSandboxName(session.ID))
		if err != nil {
			return err
		}
		recoveries, err := store.SessionRecoveries(ctx, session.ID)
		if err != nil {
			return err
		}
		return writeJSON(stdout, struct {
			Checkpoints []persistence.Checkpoint      `json:"checkpoints"`
			Recoveries  []persistence.RecoveryReceipt `json:"recoveries"`
		}{checkpoints, recoveries})
	case "capture-pin":
		return capturePinCommand(ctx, store, tasks, cfg, session, args[2:], stdout, stderr)
	case "recover":
		return recoverCheckpointCommand(ctx, store, tasks, cfg, session, args[2:], stdout, stderr)
	case "branch":
		return branchCheckpointCommand(ctx, store, tasks, cfg, session, args[2:], stdout, stderr)
	default:
		return fmt.Errorf("unknown checkpoint operation")
	}
}

// capturePinCommand invokes a local, trusted application pin executable while
// the native source guard remains armed. Its output is provisional until the
// checkpoint succeeds and is therefore hidden on every failed attempt.
func capturePinCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, session core.Session, args []string, stdout, stderr io.Writer) error {
	return capturePinCommandWithEnv(ctx, store, tasks, cfg, session, args, nil, stdout, stderr)
}

func capturePinCommandWithEnv(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, session core.Session, args, pinEnv []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("checkpoint capture-pin", flag.ContinueOnError)
	flags.SetOutput(stderr)
	command := flags.String("pin-command", "", "absolute executable that pins an application view")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*command) || filepath.Clean(*command) != *command {
		return fmt.Errorf("checkpoint capture-pin requires one absolute --pin-command executable")
	}
	info, err := os.Stat(*command)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("checkpoint pin executable is unavailable")
	}
	boundary, err := store.Boundary(ctx, core.MainSandboxName(session.ID), false)
	if err != nil {
		return err
	}
	resolver := profileRuntimeResolver{cfg: cfg, store: store, client: tasks}
	service, err := resolver.checkpointService(ctx, boundary)
	if err != nil {
		return err
	}
	// This is a foreground operator invocation, not an Absurd backup task.
	service.Claim = func(context.Context) error { return nil }
	identity := make([]byte, 16)
	if _, err := rand.Read(identity); err != nil {
		return fmt.Errorf("create checkpoint capture attempt: %w", err)
	}
	attempt := hex.EncodeToString(identity)
	var pinned json.RawMessage
	checkpoint, err := service.CaptureWithPin(ctx, boundary.SandboxID, func(pinCtx context.Context, native persistence.CaptureBoundary, reference persistence.Reference) error {
		var err error
		pinned, err = runCheckpointPinWithEnv(pinCtx, *command, flags.Args(), pinEnv, attempt, native, reference)
		return err
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, struct {
		AttemptID  string                 `json:"attempt_id"`
		Checkpoint persistence.Checkpoint `json:"checkpoint"`
		Pin        json.RawMessage        `json:"pin"`
	}{attempt, checkpoint, pinned})
}

func runCheckpointPin(ctx context.Context, executable string, args []string, attempt string, boundary persistence.CaptureBoundary, reference persistence.Reference) (json.RawMessage, error) {
	return runCheckpointPinWithEnv(ctx, executable, args, nil, attempt, boundary, reference)
}

func runCheckpointPinWithEnv(ctx context.Context, executable string, args, environment []string, attempt string, boundary persistence.CaptureBoundary, reference persistence.Reference) (json.RawMessage, error) {
	input, err := json.Marshal(struct {
		AttemptID string                      `json:"attempt_id"`
		Boundary  persistence.CaptureBoundary `json:"boundary"`
		Reference persistence.Reference       `json:"reference"`
	}{attempt, boundary, reference})
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, checkpointPinTimeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, executable, args...)
	if environment != nil {
		cmd.Env = environment
	}
	cmd.Stdin = bytes.NewReader(append(input, '\n'))
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	buffer := &boundedPinBuffer{}
	cmd.Stdout = buffer
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("checkpoint pin command failed")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(buffer.Bytes(), &object); err != nil || object == nil {
		return nil, fmt.Errorf("checkpoint pin command must return one JSON object")
	}
	return append(json.RawMessage(nil), buffer.Bytes()...), nil
}

type boundedPinBuffer struct{ bytes.Buffer }

func (b *boundedPinBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > checkpointPinOutputLimit {
		return 0, fmt.Errorf("checkpoint pin output exceeded limit")
	}
	return b.Buffer.Write(p)
}

func checkpointBoundaryCommand(ctx context.Context, store postgres.Store, session core.Session, args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("checkpoint boundary requires exactly one Session")
	}
	result, err := (&workerCheckpoints{store: store}).CheckpointBoundary(ctx, session.ID)
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func branchStatusCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("checkpoint %s requires one branch ID", args[0])
	}
	receipt, err := (&workerCheckpoints{store: store, tasks: tasks}).ObserveBranch(ctx, args[1], args[0] == "release-branch")
	if err != nil {
		return err
	}
	return writeJSON(stdout, receipt)
}

func branchCheckpointCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, source core.Session, args []string, stdout, stderr io.Writer) error {
	request := persistence.BranchRequest{SourceSessionID: source.ID}
	flags := flag.NewFlagSet("checkpoint branch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&request.ID, "id", "", "stable branch request ID")
	flags.StringVar(&request.Repository, "repository", "", "configured logical repository ID")
	flags.StringVar(&request.SnapshotID, "snapshot", "", "exact published restic snapshot ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected checkpoint branch arguments")
	}
	receipt, err := (&workerCheckpoints{store: store, tasks: tasks, cfg: cfg}).BranchCheckpoint(ctx, request)
	if err != nil {
		return err
	}
	return writeJSON(stdout, receipt)
}

func wakeFailedCheckpointSession(ctx context.Context, store postgres.Store, tasks *absurd.Client, session core.Session, requestID string) error {
	if session.CurrentTaskID == "" {
		return fmt.Errorf("checkpoint recovery has no attached execution task")
	}
	result, err := tasks.FetchTaskResult(ctx, tasks.QueueName(), session.CurrentTaskID)
	if err != nil {
		return err
	}
	if result != nil && result.State == absurd.TaskFailed {
		_, err := coreApplication(store, tasks).RetryFailedSession(ctx, session.ID, "checkpoint-recovery:"+requestID)
		return err
	}
	return nil
}

func recoverCheckpointCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, session core.Session, args []string, stdout, stderr io.Writer) error {
	request := persistence.RecoveryRequest{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID)}
	flags := flag.NewFlagSet("checkpoint recover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&request.ID, "id", "", "stable recovery request ID")
	flags.StringVar(&request.Repository, "repository", "", "configured logical repository ID")
	flags.StringVar(&request.SnapshotID, "snapshot", "", "exact published restic snapshot ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected checkpoint recovery arguments")
	}
	if err := request.Validate(); err != nil {
		return err
	}
	checkpointCfg, err := readCheckpointConfig(cfg.PersistenceFile)
	if err != nil {
		return err
	}
	if checkpointCfg == nil || !checkpointCfg.enabled(session.ProfileRef()) || checkpointCfg.ID != request.Repository {
		return fmt.Errorf("checkpoint recovery custody is not configured")
	}
	resolver := profileRuntimeResolver{cfg: cfg, store: store, client: tasks}
	if _, err := resolver.checkpointRecovery(ctx, session.ProfileRef()); err != nil {
		return err
	}
	receipt, err := store.RequestCheckpointRecovery(ctx, tasks.QueueName(), request)
	if err != nil {
		return err
	}
	if err := wakeFailedCheckpointSession(ctx, store, tasks, session, request.ID); err != nil {
		return err
	}
	return writeJSON(stdout, receipt)
}
