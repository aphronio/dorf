package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/aphronio/dorf/internal/upgrade"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// Package upgrades are operator-only. The operator supplies a previously staged
// immutable closure; ordinary API clients cannot install arbitrary packages.
func upgradeCommand(ctx context.Context, store postgres.Store, client *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("upgrade requires show JOB or request JOB --id ID --package NIX_PATH --version VERSION")
	}
	if args[0] == "show" && len(args) == 2 {
		records, err := store.JobUpgrades(ctx, args[1])
		if err != nil {
			return err
		}
		return writeJSON(stdout, records)
	}
	if args[0] != "request" {
		return fmt.Errorf("unknown upgrade operation")
	}
	request := upgrade.Request{JobID: args[1], SandboxID: core.MainSandboxName(args[1])}
	flags := flag.NewFlagSet("upgrade request", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&request.ID, "id", "", "stable operator request ID")
	flags.StringVar(&request.PackagePath, "package", "", "staged immutable Nix package path")
	flags.StringVar(&request.Version, "version", "", "exact Codex version")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected upgrade arguments")
	}
	if err := request.Validate(); err != nil {
		return err
	}
	job, err := store.Job(ctx, request.JobID)
	if err != nil {
		return err
	}
	resolver := profileRuntimeResolver{cfg: cfg, store: store, client: client}
	base, err := resolver.resolveBase(ctx, job.ProfileRef())
	if err != nil {
		return err
	}
	if _, err := resolver.upgradeExecution(ctx, base); err != nil {
		return err
	}
	receipt, err := store.RequestSandboxUpgrade(ctx, client.QueueName(), request)
	if err != nil {
		return err
	}
	reportUpgradeAcceptance(ctx, receipt, stderr)
	return writeJSON(stdout, receipt)
}

func reportUpgradeAcceptance(ctx context.Context, receipt upgrade.Receipt, stderr io.Writer) {
	publisher, err := telemetry.FromEnv(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "Upgrade accepted; diagnostics could not initialize.")
		return
	}
	if publisher == nil {
		return
	}
	upgrade.ReportAccepted(receipt, publisher.Emit)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if publisher.Shutdown(shutdown) != nil {
		fmt.Fprintln(stderr, "Upgrade accepted; diagnostics could not finish exporting.")
	}
}
