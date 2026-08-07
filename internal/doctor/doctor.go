package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strings"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
)

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func Run(ctx context.Context, db *sql.DB, cfg config.Config, connection string) []Check {
	checks := []Check{}
	add := func(name string, err error, repair string) {
		if err == nil {
			checks = append(checks, Check{Name: name, Status: "ready", Detail: "ready"})
			return
		}
		detail := err.Error()
		if repair != "" {
			detail += "; " + repair
		}
		checks = append(checks, Check{Name: name, Status: "failed", Detail: detail})
	}
	add("postgresql", db.PingContext(ctx), "verify DORF_DATABASE_URL and local PostgreSQL is running")
	var version string
	err := db.QueryRowContext(ctx, `select absurd.get_schema_version()`).Scan(&version)
	if err == nil && version != "0.5.0" {
		err = fmt.Errorf("found version %s, require 0.5.0", version)
	}
	add("absurd", err, "run dorf migrate --absurd-schema /path/to/absurd-0.5.0.sql")
	var queue bool
	err = db.QueryRowContext(ctx, `select exists(select 1 from absurd.queues where queue_name='dorf_jobs')`).Scan(&queue)
	if err == nil && !queue {
		err = fmt.Errorf("queue dorf_jobs is missing")
	}
	add("absurd-queue", err, "run dorf migrate")
	_, err = exec.LookPath("incus")
	add("incus-command", err, "install and initialize Incus; no Docker socket is used")
	runner := incus.CommandRunner{}
	if err == nil {
		result, runErr := runner.Run(ctx, "incus", nil, "info")
		if runErr != nil {
			err = runErr
		} else if result.ExitCode != 0 {
			err = fmt.Errorf("%s", strings.TrimSpace(result.Stderr))
		}
	}
	add("incus-access", err, "grant the current user direct Incus access")
	result, runErr := runner.Run(ctx, "incus", nil, "network", "show", cfg.IncusNetwork)
	err = runErr
	if err == nil && result.ExitCode != 0 {
		err = fmt.Errorf("network %s is unavailable", cfg.IncusNetwork)
	}
	add("incus-network", err, "create the configured private Incus bridge")
	result, runErr = runner.Run(ctx, "incus", nil, "image", "info", cfg.IncusImage)
	err = runErr
	if err == nil && result.ExitCode != 0 {
		err = fmt.Errorf("image %s is unavailable", cfg.IncusImage)
	}
	add("incus-image", err, "publish the official credential-free dorf-codex image; the worker verifies its credential boundary before route installation")
	err = gateway.Gateway{StatePath: cfg.GatewayStatePath}.Check(ctx, connection)
	add("provider-route-authority", err, "connect the named provider and bind the broker to the private Incus bridge")
	return checks
}

func Ready(checks []Check) bool {
	for _, check := range checks {
		if check.Status != "ready" {
			return false
		}
	}
	return true
}
