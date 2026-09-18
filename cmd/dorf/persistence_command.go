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
		return fmt.Errorf("checkpoint requires show SESSION or recover SESSION --id ID --repository ID --snapshot FULL_ID")
	}
	session, err := store.Session(ctx, args[1])
	if err != nil {
		return err
	}
	switch args[0] {
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
	case "recover":
		return recoverCheckpointCommand(ctx, store, tasks, cfg, session, args[2:], stdout, stderr)
	default:
		return fmt.Errorf("unknown checkpoint operation")
	}
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
