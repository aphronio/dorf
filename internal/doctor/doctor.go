package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/earendil-works/absurd/sdks/go/absurd"
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
	var platformErr error
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		platformErr = fmt.Errorf("found %s/%s; supported host is linux/amd64", runtime.GOOS, runtime.GOARCH)
	}
	add("host-platform", platformErr, "use an x86_64 Linux host; macOS cannot host the local Incus VM daemon")
	kvm, kvmErr := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if kvmErr == nil {
		kvmErr = kvm.Close()
	}
	add("hardware-virtualization", kvmErr, "enable virtualization and grant this user read/write access to /dev/kvm")
	add("host-capacity", HostCapacity(), "provide at least 4 GiB total memory and 20 GiB free on /")
	add("postgresql", db.PingContext(ctx), "verify DORF_DATABASE_URL and local PostgreSQL is running")
	var version string
	err := db.QueryRowContext(ctx, `select absurd.get_schema_version()`).Scan(&version)
	if err == nil && version != "0.5.0" {
		err = fmt.Errorf("found version %s, require 0.5.0", version)
	}
	add("absurd", err, "run dorf migrate --absurd-schema /path/to/absurd-0.5.0.sql")
	client, clientErr := absurd.New(absurd.Options{DB: db, QueueName: config.QueueName})
	var queues []string
	if clientErr == nil {
		queues, err = client.ListQueues(ctx)
	} else {
		err = clientErr
	}
	queue := false
	for _, name := range queues {
		queue = queue || name == config.QueueName
	}
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
	add("incus-image", err, "install the official credential-free Dorf image; the worker verifies its credential boundary before route installation")
	err = gateway.Gateway{StatePath: cfg.GatewayStatePath}.Check(ctx, connection)
	add("provider-route-authority", err, "connect the named provider and bind the broker to the private Incus bridge")
	return checks
}

func HostCapacity() error {
	contents, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return fmt.Errorf("read memory capacity: %w", err)
	}
	var memoryKiB uint64
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			memoryKiB, _ = strconv.ParseUint(fields[1], 10, 64)
			break
		}
	}
	if memoryKiB < 4*1024*1024 {
		return fmt.Errorf("total memory is %.1f GiB", float64(memoryKiB)/(1024*1024))
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return fmt.Errorf("read root filesystem capacity: %w", err)
	}
	free := stat.Bavail * uint64(stat.Bsize)
	if free < 20*1024*1024*1024 {
		return fmt.Errorf("root filesystem has %.1f GiB free", float64(free)/(1024*1024*1024))
	}
	return nil
}

func Ready(checks []Check) bool {
	for _, check := range checks {
		if check.Status != "ready" {
			return false
		}
	}
	return true
}
