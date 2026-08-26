package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/managedservice"
)

func TestManagedServiceConfigurationAuditsNamesWithoutRetainingValues(t *testing.T) {
	home := filepath.Join(t.TempDir(), "operator")
	cfg := config.Config{
		DeploymentPath:   filepath.Join(home, ".config", "dorf", "deployment.json"),
		DatabaseExternal: true,
	}
	configuration := managedServiceConfigurationFromEnvironment(cfg, []string{
		"PATH=/usr/bin",
		"DORF_DATABASE_URL=postgresql://operator:top-secret@database/dorf",
		"DORF_BLOB_ROOT=/secret/blob-root",
		"DORF_DATABASE_URL=duplicate-that-must-not-survive",
		"XDG_CONFIG_HOME=/secret/config",
		"XDG_SESSION_TYPE=wayland",
		"DORF_TEST_DATABASE_URL=development-only",
		"E2B_API_KEY=secret-that-is-persisted-by-setup",
	})
	want := []string{"DORF_BLOB_ROOT", "DORF_DATABASE_URL", "E2B_API_KEY", "XDG_CONFIG_HOME"}
	if !reflect.DeepEqual(configuration.EnvironmentOverrides, want) ||
		configuration.DeploymentPath != cfg.DeploymentPath || !configuration.ExternalDatabase {
		t.Fatalf("configuration=%#v want override names=%v", configuration, want)
	}
	diagnostic := ""
	if err := configuration.Validate(home); err != nil {
		diagnostic = err.Error()
	}
	for _, secret := range []string{"top-secret", "blob-root", "duplicate-that-must-not-survive", "secret-that-is-persisted-by-setup"} {
		if strings.Contains(diagnostic, secret) {
			t.Fatalf("configuration diagnostic leaked %q: %s", secret, diagnostic)
		}
	}
	for _, name := range want {
		if !strings.Contains(diagnostic, name) {
			t.Fatalf("configuration diagnostic omitted override name %s: %s", name, diagnostic)
		}
	}
	setupConfiguration := managedServiceConfigurationAfterSetupFromEnvironment(config.Config{DeploymentPath: cfg.DeploymentPath}, []string{"E2B_API_KEY=persisted"}, true)
	if slices.Contains(setupConfiguration.EnvironmentOverrides, "E2B_API_KEY") {
		t.Fatalf("setup retained E2B credential remained a process-only override: %#v", setupConfiguration)
	}
	unretained := managedServiceConfigurationAfterSetupFromEnvironment(config.Config{DeploymentPath: cfg.DeploymentPath}, []string{"E2B_API_KEY=process-only"}, false)
	if !slices.Contains(unretained.EnvironmentOverrides, "E2B_API_KEY") {
		t.Fatalf("setup dropped an unretained E2B override: %#v", unretained)
	}
}

func TestCurrentManagedServiceSpecPinsRunningBinaryAndOperator(t *testing.T) {
	spec, err := currentManagedServiceSpec()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if spec.Binary != executable || spec.Operator.UID != os.Getuid() || spec.Operator.GID != os.Getgid() || spec.Operator.Home != filepath.Clean(account.HomeDir) {
		t.Fatalf("spec=%#v executable=%q uid=%d gid=%d home=%q", spec, executable, os.Getuid(), os.Getgid(), account.HomeDir)
	}
}

func TestExistingManagedServicePairIsAllOrNothing(t *testing.T) {
	unitDir := t.TempDir()
	installed, err := existingManagedServicePair(unitDir)
	if err != nil || installed {
		t.Fatalf("empty pair installed=%t err=%v", installed, err)
	}
	worker := filepath.Join(unitDir, managedservice.WorkerUnit)
	if err := os.WriteFile(worker, []byte("owned later by Manager.Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := existingManagedServicePair(unitDir); err == nil || !strings.Contains(err.Error(), managedservice.ControlAPIUnit) {
		t.Fatalf("partial-pair error=%v", err)
	}
	api := filepath.Join(unitDir, managedservice.ControlAPIUnit)
	if err := os.WriteFile(api, []byte("owned later by Manager.Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installed, err = existingManagedServicePair(unitDir)
	if err != nil || !installed {
		t.Fatalf("complete pair installed=%t err=%v", installed, err)
	}
	invalidDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(invalidDir, managedservice.WorkerUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := existingManagedServicePair(invalidDir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("non-regular error=%v", err)
	}
}

func TestPrepareManagedServiceStateRefusesSymlinkedCustody(t *testing.T) {
	home := t.TempDir()
	share := filepath.Join(home, ".local", "share")
	if err := os.MkdirAll(share, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "unrelated")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(share, "dorf")); err != nil {
		t.Fatal(err)
	}
	if err := prepareManagedServiceState(home); err == nil || !strings.Contains(err.Error(), "must be a directory") {
		t.Fatalf("symlinked state error=%v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("symlink target mode=%#o, want unchanged 0755", got)
	}
}

func TestServiceRestartAndLogsKeepFixedTargetsAndBounds(t *testing.T) {
	manager := &managedServiceTestOperations{}
	var stdout, stderr strings.Builder
	if err := managedServiceRestartCommand(context.Background(), manager, false, []string{"all"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := managedServiceLogsCommand(context.Background(), manager, false, []string{"api", "--lines", "17"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manager.restarts, []managedservice.Target{managedservice.TargetAll}) ||
		!reflect.DeepEqual(manager.logs, []managedServiceTestLog{{target: managedservice.TargetAPI, lines: 17}}) {
		t.Fatalf("restarts=%v logs=%v", manager.restarts, manager.logs)
	}
	before := len(manager.logs)
	if err := managedServiceLogsCommand(context.Background(), manager, false, []string{"worker", "--lines", "10001"}, &stdout, &stderr); err == nil {
		t.Fatal("logs accepted an unbounded line count")
	}
	if err := managedServiceLogsCommand(context.Background(), manager, false, []string{"all"}, &stdout, &stderr); err == nil {
		t.Fatal("logs accepted the aggregate target")
	}
	if len(manager.logs) != before {
		t.Fatalf("invalid log request reached manager: %v", manager.logs[before:])
	}
}

func TestServiceStatusPreservesMachineDiagnosticsAndHumanDetail(t *testing.T) {
	manager := fixedManagedServiceStatus{status: readyManagedServiceTestStatus()}
	spec := managedServiceTestSpec(t.TempDir())
	var machine, stderr strings.Builder
	if err := managedServiceStatusCommand(context.Background(), manager, spec, []string{"--output", "json"}, &machine, &stderr); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(machine.String()))
	decoder.DisallowUnknownFields()
	var status managedServiceStatusJSON
	if err := decoder.Decode(&status); err != nil {
		t.Fatalf("decode status: %v\n%s", err, machine.String())
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("status has trailing JSON: %v", err)
	}
	if !status.Ready || !status.Converged || !status.ControlAPI.Converged || !status.Worker.Converged ||
		status.ControlAPI.Name != managedservice.ControlAPIUnit || status.Worker.Name != managedservice.WorkerUnit ||
		status.API.URL != managedservice.ControlURL || status.API.Version != "1.2.3" {
		t.Fatalf("machine status=%#v", status)
	}

	var human strings.Builder
	if err := managedServiceStatusCommand(context.Background(), manager, spec, nil, &human, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Dorf managed services: ready · converged", "Control API: ready · converged", "Worker: ready · converged",
		"load=loaded", "unit-file=enabled", "active=active/running",
		"Discovery: ready · dorf 1.2.3", "Authentication: ready · typed unauthenticated response",
	} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("human status omitted %q:\n%s", want, human.String())
		}
	}
}

func TestReconcileApprovalUsesPlanAndDoesNotMutateEarly(t *testing.T) {
	summaries := []string{
		"Install " + managedservice.WorkerUnit,
		"Install " + managedservice.ControlAPIUnit,
		"Reload systemd for managed Dorf units",
	}
	var stdout strings.Builder
	err := approveManagedServiceChanges(context.Background(), summaries, false, &stdout)
	if err == nil || !strings.Contains(err.Error(), "rerun dorf service reconcile --yes") {
		t.Fatalf("approval error=%v", err)
	}
	for _, summary := range summaries {
		if !strings.Contains(stdout.String(), summary) {
			t.Fatalf("approval omitted exact summary %q:\n%s", summary, stdout.String())
		}
	}
}

func TestReconcileRestartForcesWorkerThenAPIBeforeReady(t *testing.T) {
	manager := &managedServiceTestOperations{status: readyManagedServiceTestStatus()}
	spec := managedServiceTestSpec(t.TempDir())
	if err := os.Mkdir(spec.Operator.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := managedServiceTestConfiguration(spec)
	var stdout, stderr strings.Builder
	err := reconcileManagedServicesWith(context.Background(), manager, false, spec, configuration, managedServiceReconcileOptions{Yes: true}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manager.restarts, []managedservice.Target{managedservice.TargetAll}) || manager.planCalls != 1 || manager.applyCalls != 1 || manager.statusCalls != 1 {
		t.Fatalf("plan=%d apply=%d restarts=%v status=%d", manager.planCalls, manager.applyCalls, manager.restarts, manager.statusCalls)
	}
	for _, path := range []string{filepath.Join(spec.Operator.Home, ".local", "share", "dorf"), filepath.Join(spec.Operator.Home, ".local", "state", "dorf")} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("prepared state %s info=%v err=%v", path, info, err)
		}
	}
	for _, text := range []string{
		"Restart " + managedservice.WorkerUnit + " then " + managedservice.ControlAPIUnit,
		"Dorf managed services: ready",
	} {
		if !strings.Contains(stdout.String(), text) {
			t.Fatalf("reconcile output omitted %q:\n%s", text, stdout.String())
		}
	}
}

func TestManagedServiceReadinessWaitIsBoundedAndDiagnostic(t *testing.T) {
	status := managedservice.Status{
		ControlAPI: managedservice.ServiceStatus{Detail: "API unit inactive"},
		Worker:     managedservice.ServiceStatus{Detail: "worker activating"},
		API: managedservice.APIStatus{
			Discovery:      managedservice.Check{Detail: "connection refused"},
			Authentication: managedservice.Check{Detail: "connection refused"},
		},
	}
	started := time.Now()
	_, err := waitForManagedServiceReady(context.Background(), fixedManagedServiceStatus{status: status}, managedServiceTestSpec(t.TempDir()), 8*time.Millisecond, time.Millisecond)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("bounded wait error=%v elapsed=%s", err, time.Since(started))
	}
	for _, detail := range []string{"API unit inactive", "worker activating", "connection refused"} {
		if !strings.Contains(err.Error(), detail) {
			t.Fatalf("readiness error omitted %q: %v", detail, err)
		}
	}
}

func TestManagedServiceRootDispatchIsHostSpecificBeforeComposition(t *testing.T) {
	handled, err := managedServiceRootCommand(context.Background(), []string{"doctor"}, io.Discard, io.Discard)
	if handled || err != nil {
		t.Fatalf("unrelated command handled=%t err=%v", handled, err)
	}
	configHome := t.TempDir()
	if err := os.Mkdir(filepath.Join(configHome, "dorf"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "dorf", "deployment.json"), []byte("invalid deployment"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	var stderr strings.Builder
	err = run(context.Background(), []string{"service", "--help"}, io.Discard, &stderr)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(stderr.String(), "dorf service") {
		t.Fatalf("service help err=%v stderr=%q", err, stderr.String())
	}
}

func TestServiceReconcileAuditsEnvironmentBeforeLoadingItsValues(t *testing.T) {
	override := filepath.Join(t.TempDir(), "secret-environment-value")
	t.Setenv("XDG_CONFIG_HOME", override)
	var stdout, stderr strings.Builder
	err := serviceCommand(context.Background(), []string{"reconcile", "--yes"}, &stdout, &stderr)
	if !errors.Is(err, managedservice.ErrUnsupportedConfigSource) || !strings.Contains(err.Error(), "XDG_CONFIG_HOME") {
		t.Fatalf("environment audit error=%v", err)
	}
	if strings.Contains(err.Error(), override) {
		t.Fatalf("environment audit leaked value %q: %v", override, err)
	}
}

func managedServiceTestSpec(root string) managedservice.Spec {
	return managedservice.Spec{
		Binary: "/usr/local/bin/dorf",
		Operator: managedservice.Operator{
			UID: 1001, GID: 1002, Home: filepath.Join(root, "operator"),
		},
	}
}

func managedServiceTestConfiguration(spec managedservice.Spec) managedservice.Configuration {
	return managedservice.Configuration{
		DeploymentPath: filepath.Join(spec.Operator.Home, ".config", "dorf", "deployment.json"),
	}
}

func readyManagedServiceTestStatus() managedservice.Status {
	unit := func(name string) managedservice.ServiceStatus {
		return managedservice.ServiceStatus{
			Name: name, Owned: true, Current: true, Converged: true, LoadState: "loaded", UnitFileState: "enabled",
			ActiveState: "active", SubState: "running", Result: "success", ExecMainCode: "exited",
			ExecMainStatus: "0", Ready: true, Detail: "ready and notified",
		}
	}
	return managedservice.Status{
		ControlAPI: unit(managedservice.ControlAPIUnit),
		Worker:     unit(managedservice.WorkerUnit),
		API: managedservice.APIStatus{
			URL:            managedservice.ControlURL,
			Version:        "1.2.3",
			Discovery:      managedservice.Check{Ready: true, Detail: "dorf 1.2.3"},
			Authentication: managedservice.Check{Ready: true, Detail: "typed unauthenticated response"},
		},
		Converged: true,
		Ready:     true,
	}
}

type managedServiceUnitStatusJSON struct {
	Name           string `json:"name"`
	Owned          bool   `json:"owned"`
	Current        bool   `json:"current"`
	Converged      bool   `json:"converged"`
	LoadState      string `json:"load_state"`
	UnitFileState  string `json:"unit_file_state"`
	ActiveState    string `json:"active_state"`
	SubState       string `json:"sub_state"`
	Result         string `json:"result"`
	ExecMainCode   string `json:"exec_main_code"`
	ExecMainStatus string `json:"exec_main_status"`
	Ready          bool   `json:"ready"`
	Detail         string `json:"detail"`
}

type managedServiceCheckJSON struct {
	Ready  bool   `json:"ready"`
	Detail string `json:"detail"`
}

type managedServiceAPIStatusJSON struct {
	URL            string                  `json:"url"`
	Version        string                  `json:"version"`
	Discovery      managedServiceCheckJSON `json:"discovery"`
	Authentication managedServiceCheckJSON `json:"authentication"`
}

type managedServiceStatusJSON struct {
	ControlAPI managedServiceUnitStatusJSON `json:"control_api"`
	Worker     managedServiceUnitStatusJSON `json:"worker"`
	API        managedServiceAPIStatusJSON  `json:"api"`
	Converged  bool                         `json:"converged"`
	Ready      bool                         `json:"ready"`
}

type fixedManagedServiceStatus struct{ status managedservice.Status }

func (fixed fixedManagedServiceStatus) Status(context.Context, managedservice.Spec) (managedservice.Status, error) {
	return fixed.status, nil
}

type managedServiceTestLog struct {
	target managedservice.Target
	lines  int
}

type managedServiceTestOperations struct {
	status      managedservice.Status
	restarts    []managedservice.Target
	logs        []managedServiceTestLog
	planCalls   int
	applyCalls  int
	statusCalls int
}

func (operations *managedServiceTestOperations) Plan(context.Context, managedservice.Spec, managedservice.Configuration) (managedservice.Plan, error) {
	operations.planCalls++
	return managedservice.Plan{}, nil
}

func (operations *managedServiceTestOperations) Apply(context.Context, managedservice.Plan, io.Writer, io.Writer) error {
	operations.applyCalls++
	return nil
}

func (operations *managedServiceTestOperations) Status(context.Context, managedservice.Spec) (managedservice.Status, error) {
	operations.statusCalls++
	return operations.status, nil
}

func (operations *managedServiceTestOperations) Restart(_ context.Context, target managedservice.Target, _, _ io.Writer) error {
	operations.restarts = append(operations.restarts, target)
	return nil
}

func (operations *managedServiceTestOperations) Logs(_ context.Context, target managedservice.Target, lines int, _, _ io.Writer) error {
	operations.logs = append(operations.logs, managedServiceTestLog{target: target, lines: lines})
	return nil
}
