package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// Checkpoints are operator-visible recovery facts. The operator chooses an
// exact snapshot; storage order or an object-store alias never chooses it.
func checkpointCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("checkpoint requires show, boundary, capture, recover, branch, branch-status, or release-branch")
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
	case "capture":
		return captureCheckpointCommand(ctx, store, tasks, cfg, session, stdout)
	case "recover":
		return recoverCheckpointCommand(ctx, store, tasks, cfg, session, args[2:], stdout, stderr)
	case "branch":
		return branchCheckpointCommand(ctx, store, tasks, cfg, session, args[2:], stdout, stderr)
	default:
		return fmt.Errorf("unknown checkpoint operation")
	}
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

func captureCheckpointCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, session core.Session, stdout io.Writer) error {
	resolver := profileRuntimeResolver{cfg: cfg, store: store, client: tasks}
	boundary, err := store.Boundary(ctx, core.MainSandboxName(session.ID), false)
	if err != nil {
		return err
	}
	service, err := resolver.checkpointService(ctx, boundary)
	if err != nil {
		return err
	}
	service.Claim = func(context.Context) error { return nil }
	saved, err := service.CaptureCopy(ctx, boundary.SandboxID, "", func(persistence.CaptureBoundary) {})
	if err != nil {
		return err
	}
	return writeJSON(stdout, saved)
}
