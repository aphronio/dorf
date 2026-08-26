package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/managedservice"
	"github.com/aphronio/dorf/internal/version"
)

const (
	defaultManagedServiceLogLines = 200
	managedServiceReadyTimeout    = 15 * time.Second
	managedServiceReadyPoll       = 250 * time.Millisecond
)

var errManagedServiceReconcileCancelled = errors.New("managed service reconciliation cancelled")

type managedServiceReconcileOptions struct {
	Yes      bool
	Existing bool
}

// managedServiceRootCommand keeps host service operations ahead of client and
// deployment composition. In particular, status and logs must remain useful
// when PostgreSQL or the worker itself is unavailable.
func managedServiceRootCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "service" {
		return false, nil
	}
	return true, serviceCommand(ctx, args[1:], stdout, stderr)
}

func serviceCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		writeManagedServiceUsage(stderr)
		return fmt.Errorf("service requires: reconcile, status, restart, or logs")
	}
	if args[0] == "-h" || args[0] == "--help" {
		writeManagedServiceUsage(stderr)
		return flag.ErrHelp
	}

	manager := currentManagedServiceManager()
	switch args[0] {
	case "status":
		spec, err := currentManagedServiceSpec()
		if err != nil {
			return err
		}
		return managedServiceStatusCommand(ctx, manager, spec, args[1:], stdout, stderr)
	case "restart":
		return managedServiceRestartCommand(ctx, manager, manager.UseSudo, args[1:], stdout, stderr)
	case "logs":
		return managedServiceLogsCommand(ctx, manager, manager.UseSudo, args[1:], stdout, stderr)
	case "reconcile":
		options, err := parseManagedServiceReconcileOptions(args[1:], stderr)
		if err != nil {
			return err
		}
		if options.Existing {
			installed, err := existingManagedServicePair(managedservice.DefaultUnitDir)
			if err != nil {
				return err
			}
			if !installed {
				fmt.Fprintln(stdout, "Dorf managed services are not installed; nothing to reconcile")
				return nil
			}
		}
		spec, err := currentManagedServiceSpec()
		if err != nil {
			return err
		}
		if err := auditManagedServiceEnvironment(spec, os.Environ()); err != nil {
			return err
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if strings.TrimSpace(cfg.DatabaseURL) == "" {
			return fmt.Errorf("PostgreSQL is not configured; run dorf setup")
		}
		return reconcileManagedServices(ctx, manager, spec, managedServiceConfiguration(cfg), options, stdout, stderr)
	default:
		return fmt.Errorf("service requires: reconcile, status, restart, or logs")
	}
}

func currentManagedServiceManager() managedservice.Manager {
	return managedservice.Manager{
		UseSudo:         os.Geteuid() != 0,
		ExpectedVersion: version.Version,
	}
}

// currentManagedServiceSpec resolves the account and binary which are already
// running Dorf. Numeric account identities and the resolved executable avoid
// PATH, username, and symlink drift in the compiled units.
func currentManagedServiceSpec() (managedservice.Spec, error) {
	account, err := user.Current()
	if err != nil {
		return managedservice.Spec{}, fmt.Errorf("resolve current Dorf operator: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid < 0 {
		return managedservice.Spec{}, fmt.Errorf("current Dorf operator UID is invalid")
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil || gid < 0 {
		return managedservice.Spec{}, fmt.Errorf("current Dorf operator GID is invalid")
	}
	binary, err := currentDorfBinary()
	if err != nil {
		return managedservice.Spec{}, err
	}
	spec := managedservice.Spec{
		Binary: filepath.Clean(binary),
		Operator: managedservice.Operator{
			UID: uid, GID: gid, Home: filepath.Clean(account.HomeDir),
		},
	}
	if err := spec.Validate(); err != nil {
		return managedservice.Spec{}, err
	}
	return spec, nil
}

func currentDorfBinary() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running Dorf executable: %w", err)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", fmt.Errorf("resolve running Dorf executable: %w", err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return "", fmt.Errorf("resolve running Dorf executable: %w", err)
	}
	return filepath.Clean(binary), nil
}

// managedServiceConfiguration translates the already-loaded deployment
// configuration without retaining process-only values. The environment audit
// records names only; Manager.Plan refuses those names before writing units.
func managedServiceConfiguration(cfg config.Config) managedservice.Configuration {
	return managedServiceConfigurationFromEnvironment(cfg, os.Environ())
}

// setup has just retained a verified E2B_API_KEY in deployment.json, so that
// one process value no longer changes the authority available to the units.
func managedServiceConfigurationAfterSetup(cfg config.Config) managedservice.Configuration {
	stored, found, err := deployment.Load(cfg.DeploymentPath)
	e2bRetained := err == nil && found && stored.E2B != nil && strings.TrimSpace(stored.E2B.APIKey) == strings.TrimSpace(cfg.E2BAPIKey)
	return managedServiceConfigurationAfterSetupFromEnvironment(cfg, os.Environ(), e2bRetained)
}

func managedServiceConfigurationAfterSetupFromEnvironment(cfg config.Config, environment []string, e2bRetained bool) managedservice.Configuration {
	configuration := managedServiceConfigurationFromEnvironment(cfg, environment)
	if e2bRetained {
		configuration.EnvironmentOverrides = slices.DeleteFunc(configuration.EnvironmentOverrides, func(name string) bool {
			return name == "E2B_API_KEY"
		})
	}
	return configuration
}

func managedServiceConfigurationFromEnvironment(cfg config.Config, environment []string) managedservice.Configuration {
	return managedservice.Configuration{
		DeploymentPath:       cfg.DeploymentPath,
		ExternalDatabase:     cfg.DatabaseExternal,
		EnvironmentOverrides: managedServiceEnvironmentOverrides(environment),
	}
}

func managedServiceEnvironmentOverrides(environment []string) []string {
	names := make([]string, 0)
	for _, entry := range environment {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 {
			continue
		}
		name := entry[:separator]
		if managedServiceEnvironmentAuthority(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}

func managedServiceEnvironmentAuthority(name string) bool {
	switch name {
	case "DORF_DATABASE_URL", "DORF_PROVIDER_GATEWAY_STATE", "DORF_BLOB_ROOT", "DORF_GITHUB_CREDENTIALS",
		"DORF_GITHUB_API_URL", "DORF_TURN_TIMEOUT", "DORF_CODEX_APP_SERVER_PORT", "E2B_API_KEY",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME":
		return true
	default:
		return strings.HasPrefix(name, "DORF_PROOF_FAULT_")
	}
}

func auditManagedServiceEnvironment(spec managedservice.Spec, environment []string) error {
	return (managedservice.Configuration{
		DeploymentPath:       filepath.Join(spec.Operator.Home, ".config", "dorf", "deployment.json"),
		EnvironmentOverrides: managedServiceEnvironmentOverrides(environment),
	}).Validate(spec.Operator.Home)
}

func managedServiceStatusCommand(ctx context.Context, manager managedServiceStatuser, spec managedservice.Spec, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("service status", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("service status does not accept positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	status, err := manager.Status(ctx, spec)
	if err != nil {
		return err
	}
	if *output == "json" {
		if err := writeJSON(stdout, status); err != nil {
			return err
		}
	} else {
		renderManagedServiceStatus(stdout, status)
	}
	if !status.Ready {
		return fmt.Errorf("managed Dorf services are not ready")
	}
	return nil
}

type managedServiceRestarter interface {
	Restart(context.Context, managedservice.Target, io.Writer, io.Writer) error
}

func managedServiceRestartCommand(ctx context.Context, manager managedServiceRestarter, useSudo bool, args []string, stdout, stderr io.Writer) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "usage: dorf service restart <api|worker|all>")
		return flag.ErrHelp
	}
	if len(args) != 1 {
		return fmt.Errorf("service restart requires api, worker, or all")
	}
	target, err := managedServiceTarget(args[0], true)
	if err != nil {
		return err
	}
	if err := authorizeManagedServiceAdmin(ctx, useSudo, stdout, stderr); err != nil {
		return err
	}
	if err := manager.Restart(ctx, target, stdout, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restarted managed Dorf service target: %s\n", target)
	return nil
}

type managedServiceLogger interface {
	Logs(context.Context, managedservice.Target, int, io.Writer, io.Writer) error
}

func managedServiceLogsCommand(ctx context.Context, manager managedServiceLogger, useSudo bool, args []string, stdout, stderr io.Writer) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "usage: dorf service logs <api|worker> [--lines N]")
		return flag.ErrHelp
	}
	if len(args) == 0 {
		return fmt.Errorf("service logs requires api or worker")
	}
	target, err := managedServiceTarget(args[0], false)
	if err != nil {
		return err
	}
	set := flag.NewFlagSet("service logs "+args[0], flag.ContinueOnError)
	set.SetOutput(stderr)
	lines := set.Int("lines", defaultManagedServiceLogLines, "maximum journal lines (1-10000)")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("service logs does not accept positional arguments after the target")
	}
	if *lines < 1 || *lines > 10_000 {
		return fmt.Errorf("service log lines must be between 1 and 10000")
	}
	if err := authorizeManagedServiceAdmin(ctx, useSudo, stdout, stderr); err != nil {
		return err
	}
	return manager.Logs(ctx, target, *lines, stdout, stderr)
}

func managedServiceTarget(raw string, allowAll bool) (managedservice.Target, error) {
	switch raw {
	case string(managedservice.TargetAPI):
		return managedservice.TargetAPI, nil
	case string(managedservice.TargetWorker):
		return managedservice.TargetWorker, nil
	case string(managedservice.TargetAll):
		if allowAll {
			return managedservice.TargetAll, nil
		}
	}
	expected := "api or worker"
	if allowAll {
		expected += ", or all"
	}
	return "", fmt.Errorf("service target must be %s", expected)
}

func parseManagedServiceReconcileOptions(args []string, stderr io.Writer) (managedServiceReconcileOptions, error) {
	set := flag.NewFlagSet("service reconcile", flag.ContinueOnError)
	set.SetOutput(stderr)
	yes := set.Bool("yes", false, "approve every managed service change shown")
	existing := set.Bool("existing", false, "reconcile only when both managed units already exist")
	if err := set.Parse(args); err != nil {
		return managedServiceReconcileOptions{}, err
	}
	if set.NArg() != 0 {
		return managedServiceReconcileOptions{}, fmt.Errorf("service reconcile does not accept positional arguments")
	}
	return managedServiceReconcileOptions{Yes: *yes, Existing: *existing}, nil
}

// existingManagedServicePair is the updater's client-only safety gate. It
// deliberately checks only the two fixed paths; Manager.Plan remains the
// authority for ownership envelopes, loaded fragments, and current contents.
func existingManagedServicePair(unitDir string) (bool, error) {
	present := make(map[string]bool, 2)
	for _, name := range []string{managedservice.WorkerUnit, managedservice.ControlAPIUnit} {
		path := filepath.Join(unitDir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("inspect managed service unit %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("managed service unit %s is not a regular file", path)
		}
		present[name] = true
	}
	if len(present) == 0 {
		return false, nil
	}
	if len(present) != 2 {
		missing := managedservice.WorkerUnit
		if present[managedservice.WorkerUnit] {
			missing = managedservice.ControlAPIUnit
		}
		return false, fmt.Errorf("managed service installation is partial; %s is absent", missing)
	}
	return true, nil
}

// reconcileManagedServices is shared by setup and the host-only command. The
// Plan is both the approval receipt and the exact object later applied.
func reconcileManagedServices(ctx context.Context, manager managedservice.Manager, spec managedservice.Spec, configuration managedservice.Configuration, options managedServiceReconcileOptions, stdout, stderr io.Writer) error {
	return reconcileManagedServicesWith(ctx, manager, manager.UseSudo, spec, configuration, options, stdout, stderr)
}

type managedServiceReconciler interface {
	managedServiceStatuser
	managedServiceRestarter
	Plan(context.Context, managedservice.Spec, managedservice.Configuration) (managedservice.Plan, error)
	Apply(context.Context, managedservice.Plan, io.Writer, io.Writer) error
}

func reconcileManagedServicesWith(ctx context.Context, manager managedServiceReconciler, useSudo bool, spec managedservice.Spec, configuration managedservice.Configuration, options managedServiceReconcileOptions, stdout, stderr io.Writer) error {
	plan, err := manager.Plan(ctx, spec, configuration)
	if err != nil {
		return err
	}
	summaries := plan.Summaries()
	// Reconciliation always restarts both processes. This is what makes an
	// interrupted Apply repairable: a later empty Plan still activates the
	// installed units and an in-place binary update.
	summaries = append(summaries, "Restart "+managedservice.WorkerUnit+" then "+managedservice.ControlAPIUnit)
	if err := approveManagedServiceChanges(ctx, summaries, options.Yes, stdout); err != nil {
		return err
	}
	if err := prepareManagedServiceState(spec.Operator.Home); err != nil {
		return err
	}
	if err := authorizeManagedServiceAdmin(ctx, useSudo, stdout, stderr); err != nil {
		return err
	}
	if err := manager.Apply(ctx, plan, stdout, stderr); err != nil {
		return err
	}
	if err := manager.Restart(ctx, managedservice.TargetAll, stdout, stderr); err != nil {
		return err
	}
	status, err := waitForManagedServiceReady(ctx, manager, spec, managedServiceReadyTimeout, managedServiceReadyPoll)
	if err != nil {
		return err
	}
	renderManagedServiceStatus(stdout, status)
	return nil
}

func reconcileSetupManagedServices(ctx context.Context, cfg config.Config, yes bool, presenter setupPresenter, stdout, stderr io.Writer) (bool, error) {
	spec, err := currentManagedServiceSpec()
	if err != nil {
		return false, err
	}
	err = reconcileManagedServices(ctx, currentManagedServiceManager(), spec, managedServiceConfigurationAfterSetup(cfg), managedServiceReconcileOptions{Yes: yes}, stdout, stderr)
	if errors.Is(err, managedservice.ErrUnsupportedConfigSource) {
		installed, pairErr := existingManagedServicePair(managedservice.DefaultUnitDir)
		if pairErr != nil {
			return false, pairErr
		}
		if installed {
			return false, fmt.Errorf("managed Dorf services are installed but this setup uses process-only configuration; remove the overrides or disable the managed units before supervising Dorf explicitly")
		}
		presenter.Note("Managed services", "Skipped because this deployment uses process-only configuration · supervise dorf serve and dorf worker explicitly")
		return false, nil
	}
	return err == nil, err
}

func prepareManagedServiceState(home string) error {
	for _, components := range [][]string{{".local", "share", "dorf"}, {".local", "state", "dorf"}} {
		path := home
		for _, component := range components {
			path = filepath.Join(path, component)
			if err := ensureManagedServiceDirectory(path); err != nil {
				return err
			}
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("protect managed Dorf state: %w", err)
		}
	}
	return nil
}

func ensureManagedServiceDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("prepare managed Dorf state: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect managed Dorf state: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("managed Dorf state path %s must be a directory, not a link or file", path)
	}
	return nil
}

func approveManagedServiceChanges(ctx context.Context, summaries []string, yes bool, output io.Writer) error {
	description := managedServiceChangeDescription(summaries)
	fmt.Fprintln(output, "Managed Dorf service changes:")
	fmt.Fprintln(output, description)
	if yes {
		return nil
	}
	presenter := newSetupPresenter(output)
	if !presenter.interactive {
		return fmt.Errorf("managed service changes require approval; rerun dorf service reconcile --yes")
	}
	approved := false
	if err := presenter.RunForm(ctx, presenter.ConfirmGroup("Apply these managed service changes?", description, &approved)); err != nil {
		if errors.Is(err, errSetupCancelled) {
			return errManagedServiceReconcileCancelled
		}
		return fmt.Errorf("confirm managed service changes: %w", err)
	}
	if !approved {
		return errManagedServiceReconcileCancelled
	}
	return nil
}

func managedServiceChangeDescription(summaries []string) string {
	lines := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		lines = append(lines, "  • "+summary)
	}
	return strings.Join(lines, "\n")
}

// authorizeManagedServiceAdmin acquires interactive authorization before the
// manager's non-interactive sudo calls. Root callers never execute sudo.
func authorizeManagedServiceAdmin(ctx context.Context, useSudo bool, stdout, stderr io.Writer) error {
	if !useSudo {
		return nil
	}
	command := exec.CommandContext(ctx, "/usr/bin/sudo", "-v")
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, stdout, stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("administrator authentication: %w", err)
	}
	return nil
}

type managedServiceStatuser interface {
	Status(context.Context, managedservice.Spec) (managedservice.Status, error)
}

func waitForManagedServiceReady(ctx context.Context, manager managedServiceStatuser, spec managedservice.Spec, timeout, interval time.Duration) (managedservice.Status, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last managedservice.Status
	for {
		status, err := manager.Status(waitCtx, spec)
		if err != nil {
			return managedservice.Status{}, err
		}
		last = status
		if status.Ready {
			return status, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			return last, fmt.Errorf("managed Dorf services did not become ready within %s: %s", timeout, managedServiceFailureSummary(last))
		case <-timer.C:
		}
	}
}

func renderManagedServiceStatus(output io.Writer, status managedservice.Status) {
	fmt.Fprintf(output, "Dorf managed services: %s · %s\n", managedServiceReadiness(status.Ready), managedServiceConvergence(status.Converged))
	renderManagedUnitStatus(output, "Control API", status.ControlAPI)
	renderManagedUnitStatus(output, "Worker", status.Worker)
	fmt.Fprintf(output, "  Discovery: %s · %s\n", managedServiceReadiness(status.API.Discovery.Ready), status.API.Discovery.Detail)
	fmt.Fprintf(output, "  Authentication: %s · %s\n", managedServiceReadiness(status.API.Authentication.Ready), status.API.Authentication.Detail)
}

func renderManagedUnitStatus(output io.Writer, label string, status managedservice.ServiceStatus) {
	fmt.Fprintf(output, "  %s: %s · %s · owned=%t · current=%t · load=%s · unit-file=%s · active=%s/%s · result=%s · exec=%s/%s · %s\n",
		label, managedServiceReadiness(status.Ready), managedServiceConvergence(status.Converged), status.Owned, status.Current,
		empty(status.LoadState), empty(status.UnitFileState),
		empty(status.ActiveState), empty(status.SubState), empty(status.Result), empty(status.ExecMainCode),
		empty(status.ExecMainStatus), status.Detail)
}

func managedServiceReadiness(ready bool) string {
	if ready {
		return "ready"
	}
	return "not ready"
}

func managedServiceConvergence(converged bool) string {
	if converged {
		return "converged"
	}
	return "not converged"
}

func managedServiceFailureSummary(status managedservice.Status) string {
	return fmt.Sprintf("control API: %s; worker: %s; discovery: %s; authentication: %s",
		status.ControlAPI.Detail, status.Worker.Detail, status.API.Discovery.Detail, status.API.Authentication.Detail)
}

func writeManagedServiceUsage(output io.Writer) {
	fmt.Fprintln(output, "usage: dorf service <reconcile|status|restart|logs> [options]")
}
