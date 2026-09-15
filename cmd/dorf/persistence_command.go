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
		return fmt.Errorf("checkpoint requires show JOB or recover JOB --id ID --repository ID --snapshot FULL_ID")
	}
	job, err := store.Job(ctx, args[1])
	if err != nil {
		return err
	}
	switch args[0] {
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("checkpoint show requires exactly one Job")
		}
		checkpoints, err := store.ListCheckpoints(ctx, core.MainSandboxName(job.ID))
		if err != nil {
			return err
		}
		recoveries, err := store.JobRecoveries(ctx, job.ID)
		if err != nil {
			return err
		}
		return writeJSON(stdout, struct {
			Checkpoints []persistence.Checkpoint      `json:"checkpoints"`
			Recoveries  []persistence.RecoveryReceipt `json:"recoveries"`
		}{checkpoints, recoveries})
	case "recover":
		return recoverCheckpointCommand(ctx, store, tasks, cfg, job, args[2:], stdout, stderr)
	default:
		return fmt.Errorf("unknown checkpoint operation")
	}
}

func wakeFailedCheckpointJob(ctx context.Context, store postgres.Store, tasks *absurd.Client, job core.Job, requestID string) error {
	if job.CurrentTaskID == "" {
		return fmt.Errorf("checkpoint recovery has no attached execution task")
	}
	result, err := tasks.FetchTaskResult(ctx, tasks.QueueName(), job.CurrentTaskID)
	if err != nil {
		return err
	}
	if result != nil && result.State == absurd.TaskFailed {
		_, err := coreApplication(store, tasks).RetryFailedJob(ctx, job.ID, "checkpoint-recovery:"+requestID)
		return err
	}
	return nil
}

func recoverCheckpointCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, job core.Job, args []string, stdout, stderr io.Writer) error {
	request := persistence.RecoveryRequest{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID)}
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
	if checkpointCfg == nil || !checkpointCfg.enabled(job.ProfileRef()) || checkpointCfg.ID != request.Repository {
		return fmt.Errorf("checkpoint recovery custody is not configured")
	}
	resolver := profileRuntimeResolver{cfg: cfg, store: store, client: tasks}
	if _, err := resolver.checkpointRecovery(ctx, job.ProfileRef()); err != nil {
		return err
	}
	receipt, err := store.RequestCheckpointRecovery(ctx, tasks.QueueName(), request)
	if err != nil {
		return err
	}
	if err := wakeFailedCheckpointJob(ctx, store, tasks, job, request.ID); err != nil {
		return err
	}
	return writeJSON(stdout, receipt)
}
