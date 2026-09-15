package main

import (
	"context"
	"errors"
	"os"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	"time"
)

func runWithCheckpoints(ctx context.Context, r profileRuntimeResolver, foreground func(context.Context) error) error {
	cfg, err := readCheckpointConfig(r.cfg.PersistenceFile)
	if err != nil {
		return err
	}
	if cfg == nil {
		return foreground(ctx)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tasks, err := absurd.New(absurd.Options{DB: r.store.DB, QueueName: r.client.QueueName() + "_checkpoints", DefaultMaxAttempts: 5, Logger: absurdruntime.WorkerLogger(os.Stderr)})
	if err != nil {
		return err
	}
	defer tasks.Close()
	if err := tasks.CreateQueue(ctx, tasks.QueueName()); err != nil {
		return err
	}
	worker := persistence.Worker{Store: r.store, Tasks: tasks, IdleDelay: time.Duration(cfg.IdleDelaySeconds) * time.Second,
		Enabled: func(b persistence.CaptureBoundary) bool {
			return cfg.enabled(core.SandboxProfileRef{Name: b.ProfileName, Revision: b.ProfileRevision})
		},
		Resolve: r.checkpointService}
	worker.Register(runCtx)
	backupDone := make(chan error, 1)
	go func() { backupDone <- worker.Run(runCtx) }()
	foregroundErr := foreground(runCtx)
	cancel()
	backupErr := <-backupDone
	if foregroundErr != nil && !errors.Is(foregroundErr, context.Canceled) {
		return foregroundErr
	}
	if backupErr != nil && !errors.Is(backupErr, context.Canceled) {
		return backupErr
	}
	return foregroundErr
}
