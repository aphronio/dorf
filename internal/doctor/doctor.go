package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func Run(ctx context.Context, db *sql.DB, cfg config.Config, profile core.SandboxProfile, connection string) []Check {
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
	localVMs := profile.Provider == core.SandboxProviderIncus && cfg.Incus != nil && strings.HasPrefix(cfg.Incus.Endpoint, "unix://")
	capacityRepair := "provide at least 2 GiB total memory and 10 GiB free on /"
	if localVMs {
		capacityRepair = "provide at least 4 GiB total memory and 20 GiB free on /"
	}
	add("host-capacity", HostCapacity(localVMs), capacityRepair)
	databaseRepair := "run dorf setup to reconcile the selected PostgreSQL deployment"
	if cfg.DatabaseExternal {
		databaseRepair = "verify DORF_DATABASE_URL and the external PostgreSQL service"
	}
	add("postgresql", db.PingContext(ctx), databaseRepair)
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
	switch profile.Provider {
	case core.SandboxProviderIncus:
		addIncusChecks(ctx, cfg.Incus, profile, add)
	case core.SandboxProviderE2B:
		adapter := e2b.Adapter{Config: e2b.AdapterConfig{Template: profile.Artifact, Workspace: cfg.Workspace, SandboxTimeout: profile.E2BSandboxTimeout, ProcessTimeout: cfg.TurnTimeout, ProviderGatewayURL: profile.E2BGatewayURL, AllowInternet: profile.E2BAllowInternet}}
		add("e2b-profile", adapter.Validate(), "configure the exact E2B template, whole-second timeout, workspace, and deployment-owned HTTPS /v1 Gateway URL")
		var keyErr error
		if strings.TrimSpace(cfg.E2BAPIKey) == "" {
			keyErr = fmt.Errorf("E2B_API_KEY is empty")
		}
		add("e2b-api-key", keyErr, "provide the E2B project key only through the host environment")
	default:
		add("sandbox-profile", fmt.Errorf("unsupported Sandbox provider %q in profile %q", profile.Provider, profile.Name), "select a supported named profile")
	}
	err = (gateway.Gateway{StatePath: cfg.GatewayStatePath, InternalDialOrigin: cfg.GatewayInternalOrigin}).Check(ctx, connection)
	repair := "connect the named provider and bind the broker to the private Incus bridge"
	if profile.Provider == core.SandboxProviderE2B {
		repair = "connect the named provider; the deployment-owned HTTPS route must reach its private broker"
	}
	add("provider-route-authority", err, repair)
	return checks
}

type incusReadClient interface {
	NetworkIPv4(context.Context, string) (string, error)
	Close()
}

type incusCheckDependencies struct {
	openClient              func(context.Context, incus.ConnectionConfig) (incusReadClient, error)
	resolveImageFingerprint func(context.Context, incus.ConnectionConfig, string) (string, error)
}

var productionIncusCheckDependencies = incusCheckDependencies{
	openClient: func(ctx context.Context, config incus.ConnectionConfig) (incusReadClient, error) {
		return (incus.SDKClientFactory{}).Open(ctx, config)
	},
	resolveImageFingerprint: incus.ResolveImageFingerprint,
}

func addIncusChecks(ctx context.Context, authority *deployment.Incus, profile core.SandboxProfile, add func(string, error, string)) {
	addIncusChecksWithDependencies(ctx, authority, profile, add, productionIncusCheckDependencies)
}

func addIncusChecksWithDependencies(ctx context.Context, authority *deployment.Incus, profile core.SandboxProfile, add func(string, error, string), dependencies incusCheckDependencies) {
	connection, local, err := incusConnectionForProfile(authority, profile)
	if local {
		addLocalIncusHostChecks(add)
	}
	if err != nil {
		add("incus-access", err, "configure the exact Incus endpoint authority selected by this profile")
		add("incus-network", fmt.Errorf("Incus endpoint access is unavailable: %w", err), "create the configured private Incus bridge")
		add("incus-image", fmt.Errorf("Incus endpoint access is unavailable: %w", err), "restore the exact Incus image fingerprint selected by this profile, then rerun profile verification")
		return
	}

	client, err := dependencies.openClient(ctx, connection)
	add("incus-access", err, "grant this deployment identity direct access to the configured Incus endpoint")
	if err != nil {
		add("incus-network", fmt.Errorf("Incus endpoint access is unavailable: %w", err), "create the configured private Incus bridge")
		add("incus-image", fmt.Errorf("Incus endpoint access is unavailable: %w", err), "restore the exact Incus image fingerprint selected by this profile, then rerun profile verification")
		return
	} else {
		defer client.Close()
		_, networkErr := client.NetworkIPv4(ctx, profile.IncusNetwork)
		if networkErr != nil {
			networkErr = fmt.Errorf("inspect Incus network %q: %w", profile.IncusNetwork, networkErr)
		}
		add("incus-network", networkErr, "create the configured private Incus bridge")
	}

	_, imageErr := dependencies.resolveImageFingerprint(ctx, connection, profile.Artifact)
	add("incus-image", imageErr, "restore the exact Incus image fingerprint selected by this profile, then rerun profile verification")
}

func incusConnectionForProfile(authority *deployment.Incus, profile core.SandboxProfile) (incus.ConnectionConfig, bool, error) {
	if authority == nil {
		return incus.ConnectionConfig{}, false, fmt.Errorf("Incus endpoint authority is not configured")
	}
	authorityHash, err := authority.AuthorityHash()
	if err != nil {
		return incus.ConnectionConfig{}, false, err
	}
	local := strings.HasPrefix(authority.Endpoint, "unix://")
	if profile.IncusEndpointAuthorityHash == "" {
		return incus.ConnectionConfig{}, local, fmt.Errorf("Incus profile %q does not select an endpoint authority", profile.Name)
	}
	if authorityHash != profile.IncusEndpointAuthorityHash {
		return incus.ConnectionConfig{}, local, fmt.Errorf("configured Incus endpoint authority does not match profile %q", profile.Name)
	}
	connection := incus.ConnectionConfig{
		Endpoint:             authority.Endpoint,
		Project:              profile.IncusProject,
		StoragePool:          profile.IncusStoragePool,
		TLSServerCertificate: authority.ServerCertificate,
		TLSClientCertificate: authority.ClientCertificate,
		TLSClientKey:         authority.ClientPrivateKey,
	}
	if err := connection.Validate(); err != nil {
		return incus.ConnectionConfig{}, local, err
	}
	return connection, local, nil
}

func addLocalIncusHostChecks(add func(string, error, string)) {
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
}

func HostCapacity(localVMs bool) error {
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
	minimumMemoryKiB := uint64(2 * 1024 * 1024)
	minimumDisk := uint64(10 * 1024 * 1024 * 1024)
	if localVMs {
		minimumMemoryKiB = 4 * 1024 * 1024
		minimumDisk = 20 * 1024 * 1024 * 1024
	}
	if memoryKiB < minimumMemoryKiB {
		return fmt.Errorf("total memory is %.1f GiB", float64(memoryKiB)/(1024*1024))
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return fmt.Errorf("read root filesystem capacity: %w", err)
	}
	free := stat.Bavail * uint64(stat.Bsize)
	if free < minimumDisk {
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
