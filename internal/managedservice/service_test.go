package managedservice

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func testSpec(root string) Spec {
	home := filepath.Join(root, "operator")
	return Spec{
		Binary: "/usr/local/bin/dorf", Operator: Operator{UID: 1001, GID: 1002, Home: home},
	}
}

func testConfiguration(spec Spec) Configuration {
	return Configuration{DeploymentPath: filepath.Join(spec.Operator.Home, ".config", "dorf", "deployment.json")}
}

func testManager(runner CommandRunner, unitDir string) Manager {
	return Manager{
		Runner:        runner,
		unitDir:       unitDir,
		expectedOwner: unitFileOwner{uid: uint32(os.Getuid()), gid: uint32(os.Getgid())},
	}
}

func TestRenderUnitsPinsResponsibilitiesOwnershipAndHardening(t *testing.T) {
	spec := testSpec(t.TempDir())
	units, err := RenderUnits(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 || units[0].Name != WorkerUnit || units[1].Name != ControlAPIUnit {
		t.Fatalf("units=%v", []string{units[0].Name, units[1].Name})
	}
	for _, unit := range units {
		if err := validateOwnedUnit(unit.Name, unit.Contents); err != nil {
			t.Fatalf("%s ownership: %v", unit.Name, err)
		}
		contents := string(unit.Contents)
		for _, exact := range []string{
			"Type=notify\n", "NotifyAccess=main\n", "User=1001\n", "Group=1002\n",
			"Environment=\"HOME=" + spec.Operator.Home + "\"\n",
			"ExecStartPre=/usr/local/bin/dorf migrate\n", "Restart=on-failure\n",
			"NoNewPrivileges=true\n", "ProtectSystem=strict\n", "ProtectHome=tmpfs\n",
			"CapabilityBoundingSet=\n", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\n",
			"BindReadOnlyPaths=-/usr/local/bin/dorf ",
		} {
			if !strings.Contains(contents, exact) {
				t.Fatalf("%s lacks %q\n%s", unit.Name, exact, contents)
			}
		}
		if strings.Contains(contents, "DORF_DATABASE_URL") {
			t.Fatalf("%s persists process configuration", unit.Name)
		}
	}
	worker, api := string(units[0].Contents), string(units[1].Contents)
	if !strings.Contains(worker, "ExecStart=/usr/local/bin/dorf worker\n") ||
		!strings.Contains(worker, "BindPaths=-"+filepath.Join(spec.Operator.Home, ".local", "share", "dorf")) {
		t.Fatalf("worker responsibility is incomplete:\n%s", worker)
	}
	if !strings.Contains(api, "ExecStart=/usr/local/bin/dorf serve --listen 127.0.0.1:8745\n") || strings.Contains(api, "BindPaths=") {
		t.Fatalf("API responsibility is not narrow:\n%s", api)
	}
}

func TestSpecRefusesSystemdPathMetacharacters(t *testing.T) {
	for _, test := range []struct {
		name      string
		character string
	}{
		{name: "dollar", character: "$"},
		{name: "single quote", character: "'"},
		{name: "colon", character: ":"},
	} {
		for _, field := range []string{"HOME", "binary"} {
			t.Run(test.name+" in "+field, func(t *testing.T) {
				spec := testSpec(t.TempDir())
				if field == "HOME" {
					spec.Operator.Home += test.character + "unsafe"
				} else {
					spec.Binary += test.character + "unsafe"
				}
				if err := spec.Validate(); err == nil {
					t.Fatalf("Spec.Validate accepted %s in %s", test.name, field)
				}
			})
		}
	}
}

func TestSpecRefusesProcessOnlyConfigurationAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*Configuration){
		"external database": func(configuration *Configuration) { configuration.ExternalDatabase = true },
		"DORF override": func(configuration *Configuration) {
			configuration.EnvironmentOverrides = []string{"DORF_BLOB_ROOT"}
		},
		"XDG path": func(configuration *Configuration) {
			configuration.DeploymentPath = "/tmp/xdg/dorf/deployment.json"
			configuration.EnvironmentOverrides = []string{"XDG_CONFIG_HOME"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec(t.TempDir())
			configuration := testConfiguration(spec)
			mutate(&configuration)
			err := configuration.Validate(spec.Operator.Home)
			if !errors.Is(err, ErrUnsupportedConfigSource) {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(err.Error(), "postgresql://") {
				t.Fatalf("configuration error leaked a value: %v", err)
			}
			if name == "XDG path" && strings.Contains(err.Error(), configuration.DeploymentPath) {
				t.Fatalf("configuration error leaked the resolved path: %v", err)
			}
		})
	}
}

func TestPlanRefusesForeignUnitBeforeLifecycleCommands(t *testing.T) {
	unitDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(unitDir, WorkerUnit), []byte("[Service]\nExecStart=/usr/bin/foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{unitDir: unitDir, shows: readyProperties(unitDir)}
	spec := testSpec(t.TempDir())
	_, err := testManager(runner, unitDir).Plan(context.Background(), spec, testConfiguration(spec))
	if !errors.Is(err, ErrForeignUnit) {
		t.Fatalf("error=%v", err)
	}
	if len(runner.outputs) != 0 || len(runner.runs) != 0 {
		t.Fatalf("foreign preflight executed commands: output=%v run=%v", runner.outputs, runner.runs)
	}
}

func TestPlanRefusesLoadedUnitFromAnotherFragment(t *testing.T) {
	unitDir := t.TempDir()
	shows := missingProperties()
	shows[WorkerUnit] = unitProperties{LoadState: "loaded", FragmentPath: "/usr/lib/systemd/system/" + WorkerUnit}
	runner := &fakeRunner{unitDir: unitDir, shows: shows}
	spec := testSpec(t.TempDir())
	_, err := testManager(runner, unitDir).Plan(context.Background(), spec, testConfiguration(spec))
	if !errors.Is(err, ErrForeignUnit) {
		t.Fatalf("error=%v", err)
	}
}

func TestPlanApplyConvergesAndThenBecomesEmpty(t *testing.T) {
	unitDir := t.TempDir()
	spec := testSpec(t.TempDir())
	runner := &fakeRunner{unitDir: unitDir, shows: missingProperties()}
	manager := testManager(runner, unitDir)
	manager.UseSudo = true
	configuration := testConfiguration(spec)
	plan, err := manager.Plan(context.Background(), spec, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		t.Fatal("fresh host produced an empty plan")
	}
	if err := manager.Apply(context.Background(), plan, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{WorkerUnit, ControlAPIUnit} {
		contents, err := os.ReadFile(filepath.Join(unitDir, unit))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateOwnedUnit(unit, contents); err != nil {
			t.Fatalf("installed %s: %v", unit, err)
		}
	}
	if len(runner.runs) == 0 || runner.runs[0][0] != sudoPath || !strings.Contains(strings.Join(runner.runs[0], " "), " "+installPath+" -m 0644 /dev/stdin ") {
		t.Fatalf("installation did not use the exact elevated boundary: %v", runner.runs)
	}
	for _, call := range runner.runs {
		invocation := " " + strings.Join(call, " ") + " "
		if strings.Contains(invocation, " --now ") || strings.Contains(invocation, " start ") || strings.Contains(invocation, " restart ") {
			t.Fatalf("Plan.Apply crossed into reconciliation-owned process lifecycle: %v", runner.runs)
		}
	}
	// Re-plan with the exact same specification; file and systemd state are
	// both authoritative, so no lifecycle operation remains.
	converged, err := manager.Plan(context.Background(), spec, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if !converged.Empty() {
		t.Fatalf("converged changes=%v", converged.Changes())
	}
	next := spec
	next.Binary = "/opt/dorf/bin/dorf"
	upgrade, err := manager.Plan(context.Background(), next, configuration)
	if err != nil {
		t.Fatalf("recognize intact older Dorf units: %v", err)
	}
	updates := 0
	for _, change := range upgrade.Changes() {
		if change.Action == "update" {
			updates++
		}
	}
	if updates != 2 {
		t.Fatalf("upgrade changes=%v", upgrade.Changes())
	}
}

func TestPlanRepairsOwnedUnitMetadataBeforeConvergence(t *testing.T) {
	unitDir := t.TempDir()
	spec := testSpec(t.TempDir())
	units, err := RenderUnits(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		if err := os.WriteFile(filepath.Join(unitDir, unit.Name), unit.Contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(unitDir, WorkerUnit), 0o666); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{unitDir: unitDir, shows: readyProperties(unitDir)}
	manager := testManager(runner, unitDir)
	status := manager.serviceStatus(context.Background(), units[0])
	if status.Converged || !status.Ready || !strings.Contains(status.Detail, "unit file mode") {
		t.Fatalf("metadata-drift status=%#v", status)
	}
	plan, err := manager.Plan(context.Background(), spec, testConfiguration(spec))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Changes(), Change{Unit: WorkerUnit, Action: "update"}) ||
		slices.Contains(plan.Changes(), Change{Unit: ControlAPIUnit, Action: "update"}) {
		t.Fatalf("metadata repair changes=%v", plan.Changes())
	}
	if err := os.Chmod(filepath.Join(unitDir, WorkerUnit), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(context.Background(), plan, io.Discard, io.Discard); !errors.Is(err, ErrStalePlan) {
		t.Fatalf("metadata drift after approval error=%v", err)
	}
	plan, err = manager.Plan(context.Background(), spec, testConfiguration(spec))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(context.Background(), plan, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(unitDir, WorkerUnit))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0o644 {
		t.Fatalf("repaired mode=%s", info.Mode())
	}
	if status = manager.serviceStatus(context.Background(), units[0]); !status.Converged {
		t.Fatalf("repaired status=%#v", status)
	}
}

func TestApplyAttestsInstalledUnitMetadata(t *testing.T) {
	unitDir := t.TempDir()
	spec := testSpec(t.TempDir())
	runner := &fakeRunner{unitDir: unitDir, shows: missingProperties(), installMode: 0o666}
	manager := testManager(runner, unitDir)
	plan, err := manager.Plan(context.Background(), spec, testConfiguration(spec))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(context.Background(), plan, io.Discard, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "attest installed "+WorkerUnit) || !strings.Contains(err.Error(), "unit file mode") {
		t.Fatalf("error=%v", err)
	}
}

func TestPlanRefusesHardlinkedOwnedUnitBeforeLifecycleCommands(t *testing.T) {
	unitDir := t.TempDir()
	spec := testSpec(t.TempDir())
	units, err := RenderUnits(spec)
	if err != nil {
		t.Fatal(err)
	}
	workerPath := filepath.Join(unitDir, WorkerUnit)
	if err := os.WriteFile(workerPath, units[0].Contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(workerPath, filepath.Join(unitDir, "unexpected-hardlink")); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{unitDir: unitDir, shows: readyProperties(unitDir)}
	_, err = testManager(runner, unitDir).Plan(context.Background(), spec, testConfiguration(spec))
	if !errors.Is(err, ErrForeignUnit) {
		t.Fatalf("error=%v", err)
	}
	if len(runner.outputs) != 0 || len(runner.runs) != 0 {
		t.Fatalf("hardlink preflight executed commands: output=%v run=%v", runner.outputs, runner.runs)
	}
}

func TestApplyRefusesAPlanAfterUnitBytesChange(t *testing.T) {
	unitDir := t.TempDir()
	spec := testSpec(t.TempDir())
	runner := &fakeRunner{unitDir: unitDir, shows: missingProperties()}
	manager := testManager(runner, unitDir)
	plan, err := manager.Plan(context.Background(), spec, testConfiguration(spec))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, WorkerUnit), []byte("foreign race\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(context.Background(), plan, io.Discard, io.Discard); !errors.Is(err, ErrStalePlan) {
		t.Fatalf("error=%v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("stale plan executed commands: %v", runner.runs)
	}
}

func TestApplyRefusesAPlanAfterRelevantSystemdStateChanges(t *testing.T) {
	unitDir := t.TempDir()
	spec := testSpec(t.TempDir())
	runner := &fakeRunner{unitDir: unitDir, shows: missingProperties()}
	manager := testManager(runner, unitDir)
	plan, err := manager.Plan(context.Background(), spec, testConfiguration(spec))
	if err != nil {
		t.Fatal(err)
	}
	runner.shows[WorkerUnit] = unitProperties{LoadState: "loaded", FragmentPath: filepath.Join(unitDir, WorkerUnit)}
	if err := manager.Apply(context.Background(), plan, io.Discard, io.Discard); !errors.Is(err, ErrStalePlan) {
		t.Fatalf("error=%v", err)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("stale systemd plan executed commands: %v", runner.runs)
	}
}

func TestApplyDoesNotBindReconciliationOwnedRuntimeState(t *testing.T) {
	unitDir := t.TempDir()
	spec := testSpec(t.TempDir())
	runner := &fakeRunner{unitDir: unitDir, shows: missingProperties()}
	manager := testManager(runner, unitDir)
	plan, err := manager.Plan(context.Background(), spec, testConfiguration(spec))
	if err != nil {
		t.Fatal(err)
	}
	worker := runner.shows[WorkerUnit]
	worker.ActiveState, worker.SubState = "active", "running"
	worker.Type, worker.NotifyAccess = "notify", "main"
	runner.shows[WorkerUnit] = worker
	if err := manager.Apply(context.Background(), plan, io.Discard, io.Discard); err != nil {
		t.Fatalf("runtime-only change made Plan stale: %v", err)
	}
}

func TestPlanRecoversFilesInstalledBeforeDaemonReload(t *testing.T) {
	unitDir := t.TempDir()
	spec := testSpec(t.TempDir())
	units, err := RenderUnits(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		if err := os.WriteFile(filepath.Join(unitDir, unit.Name), unit.Contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shows := readyProperties(unitDir)
	for name, properties := range shows {
		properties.NeedDaemonReload = "yes"
		shows[name] = properties
	}
	runner := &fakeRunner{unitDir: unitDir, shows: shows}
	manager := testManager(runner, unitDir)
	plan, err := manager.Plan(context.Background(), spec, testConfiguration(spec))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(plan.Changes(), func(change Change) bool { return change.Action == "reload" }) {
		t.Fatalf("partial install recovery changes=%v", plan.Changes())
	}
	if err := manager.Apply(context.Background(), plan, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, properties := range runner.shows {
		if properties.NeedDaemonReload != "no" {
			t.Fatalf("daemon reload remained required: %#v", properties)
		}
	}
}

func TestStatusSeparatesConvergenceRuntimeAndAPIReadiness(t *testing.T) {
	unitDir := t.TempDir()
	spec := testSpec(t.TempDir())
	units, err := RenderUnits(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		if err := os.WriteFile(filepath.Join(unitDir, unit.Name), unit.Contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{unitDir: unitDir, shows: readyProperties(unitDir)}
	apiCalls := 0
	client := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		apiCalls++
		if request.URL.Scheme != "http" || request.URL.Host != ControlAddress {
			t.Fatalf("probe URL=%s", request.URL)
		}
		deadline, bounded := request.Context().Deadline()
		if !bounded || time.Until(deadline) > statusTimeout {
			t.Fatalf("probe context deadline=%v bounded=%t", deadline, bounded)
		}
		switch request.URL.Path {
		case "/v1":
			return response(http.StatusOK, "application/json", `{"product":"dorf","version":"1.2.3"}`), nil
		case "/v1/me":
			if request.Header.Get("Authorization") != "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
				t.Fatalf("authentication probe=%q", request.Header.Get("Authorization"))
			}
			return response(http.StatusUnauthorized, "application/problem+json", `{"status":401,"code":"unauthenticated"}`), nil
		default:
			t.Fatalf("probe path=%s", request.URL.Path)
			return nil, nil
		}
	})}
	manager := testManager(runner, unitDir)
	manager.HTTPClient, manager.ExpectedVersion = client, "1.2.3"
	status, err := manager.Status(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || !status.Converged || !status.ControlAPI.Converged || !status.Worker.Converged ||
		!status.ControlAPI.Ready || !status.Worker.Ready || !status.API.Discovery.Ready || !status.API.Authentication.Ready {
		t.Fatalf("status=%#v", status)
	}

	worker := runner.shows[WorkerUnit]
	worker.UnitFileState = "disabled"
	runner.shows[WorkerUnit] = worker
	manager.HTTPClient, manager.ExpectedVersion = client, ""
	status, err = manager.Status(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if status.Ready || status.Converged || status.Worker.Converged || !status.Worker.Ready || !strings.Contains(status.Worker.Detail, "unit-file state disabled") {
		t.Fatalf("running but unconverged worker status=%#v", status.Worker)
	}

	worker.UnitFileState, worker.Type, worker.NotifyAccess = "enabled", "simple", "none"
	runner.shows[WorkerUnit] = worker
	status, err = manager.Status(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if status.Worker.Ready || status.Worker.Converged || !strings.Contains(status.Worker.Detail, "not notify/main") {
		t.Fatalf("stale loaded worker contract status=%#v", status.Worker)
	}

	runner.shows[WorkerUnit] = readyProperties(unitDir)[WorkerUnit]
	control := runner.shows[ControlAPIUnit]
	control.ActiveState, control.SubState = "failed", "failed"
	control.Result, control.ExecMainCode, control.ExecMainStatus = "exit-code", "exited", "1"
	runner.shows[ControlAPIUnit] = control
	before := apiCalls
	status, err = manager.Status(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if status.ControlAPI.Ready || apiCalls != before || status.ControlAPI.ActiveState != "failed" || status.ControlAPI.SubState != "failed" ||
		status.ControlAPI.Result != "exit-code" || status.ControlAPI.ExecMainCode != "exited" || status.ControlAPI.ExecMainStatus != "1" ||
		!strings.Contains(status.ControlAPI.Detail, "systemd state failed/failed") || !strings.Contains(status.API.Discovery.Detail, "skipped") {
		t.Fatalf("failed control status=%#v API=%#v calls=%d", status.ControlAPI, status.API, apiCalls-before)
	}

	runner.shows[ControlAPIUnit] = readyProperties(unitDir)[ControlAPIUnit]
	discoveryCalls := 0
	failingDiscovery := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		discoveryCalls++
		if request.URL.Path != "/v1" {
			t.Fatalf("dependent auth probe ran after discovery failure: %s", request.URL.Path)
		}
		return response(http.StatusServiceUnavailable, "application/json", `{}`), nil
	})}
	manager.HTTPClient = failingDiscovery
	status, err = manager.Status(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if status.Ready || discoveryCalls != 1 || status.API.Discovery.Ready || status.API.Authentication.Ready || !strings.Contains(status.API.Authentication.Detail, "skipped") {
		t.Fatalf("dependent probe status=%#v calls=%d", status.API, discoveryCalls)
	}
}

func TestProbeResponseRejectsOneByteBeyondItsCap(t *testing.T) {
	prefix := `{"product":"dorf","version":"1"}`
	body := prefix + strings.Repeat(" ", maxProbeBody-len(prefix)+1)
	var destination map[string]any
	err := decodeProbeResponse(response(http.StatusOK, "application/json", body), http.StatusOK, "application/json", &destination)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error=%v", err)
	}
}

func TestRestartAndLogsAcceptOnlyFixedTargets(t *testing.T) {
	unitDir := t.TempDir()
	units, err := RenderUnits(testSpec(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range units {
		if err := os.WriteFile(filepath.Join(unitDir, unit.Name), unit.Contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{unitDir: unitDir, shows: readyProperties(unitDir)}
	manager := testManager(runner, unitDir)
	if err := manager.Restart(context.Background(), TargetAll, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{systemctlPath, "restart", WorkerUnit}, {systemctlPath, "restart", ControlAPIUnit}}
	if !reflect.DeepEqual(runner.runs, want) {
		t.Fatalf("restart calls=%v want=%v", runner.runs, want)
	}
	if err := manager.Logs(context.Background(), TargetAPI, 200, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := runner.runs[len(runner.runs)-1]; !reflect.DeepEqual(got, []string{journalctlPath, "--no-pager", "--unit=" + ControlAPIUnit, "--lines=200"}) {
		t.Fatalf("logs call=%v", got)
	}
	if err := manager.Logs(context.Background(), TargetAll, 200, io.Discard, io.Discard); err == nil {
		t.Fatal("logs accepted the all target")
	}
}

type fakeRunner struct {
	unitDir     string
	shows       map[string]unitProperties
	installMode os.FileMode
	runs        [][]string
	outputs     [][]string
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	f.outputs = append(f.outputs, call)
	unit := args[len(args)-1]
	properties := f.shows[unit]
	return renderProperties(properties), nil
}

func (f *fakeRunner) Run(_ context.Context, stdin io.Reader, _, _ io.Writer, name string, args ...string) error {
	call := append([]string{name}, args...)
	f.runs = append(f.runs, call)
	if f.shows == nil {
		f.shows = map[string]unitProperties{}
	}
	command, commandArgs := filepath.Base(name), args
	if command == filepath.Base(sudoPath) {
		command, commandArgs = filepath.Base(commandArgs[2]), commandArgs[3:]
	}
	switch command {
	case "install":
		destination := commandArgs[len(commandArgs)-1]
		contents, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		mode := f.installMode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(destination, contents, mode); err != nil {
			return err
		}
		return os.Chmod(destination, mode)
	case "systemctl":
		if commandArgs[0] == "daemon-reload" {
			for _, unit := range []string{WorkerUnit, ControlAPIUnit} {
				properties := f.shows[unit]
				properties.LoadState = "loaded"
				properties.FragmentPath = filepath.Join(f.unitDir, unit)
				properties.Type = "notify"
				properties.NotifyAccess = "main"
				properties.NeedDaemonReload = "no"
				f.shows[unit] = properties
			}
			return nil
		}
		unit := commandArgs[len(commandArgs)-1]
		properties := f.shows[unit]
		if commandArgs[0] == "enable" {
			properties.UnitFileState = "enabled"
		}
		if commandArgs[0] == "start" || commandArgs[0] == "restart" || (commandArgs[0] == "enable" && len(commandArgs) > 1 && commandArgs[1] == "--now") {
			properties.ActiveState, properties.SubState = "active", "running"
			properties.Type, properties.NotifyAccess = "notify", "main"
		}
		f.shows[unit] = properties
	}
	return nil
}

func missingProperties() map[string]unitProperties {
	return map[string]unitProperties{
		WorkerUnit:     {LoadState: "not-found"},
		ControlAPIUnit: {LoadState: "not-found"},
	}
}

func readyProperties(unitDir string) map[string]unitProperties {
	result := map[string]unitProperties{}
	for _, unit := range []string{WorkerUnit, ControlAPIUnit} {
		result[unit] = unitProperties{
			LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active", SubState: "running",
			Type: "notify", NotifyAccess: "main", NeedDaemonReload: "no", FragmentPath: filepath.Join(unitDir, unit), Result: "success",
		}
	}
	return result
}

func renderProperties(properties unitProperties) string {
	return "LoadState=" + properties.LoadState + "\n" +
		"NeedDaemonReload=" + properties.NeedDaemonReload + "\n" +
		"UnitFileState=" + properties.UnitFileState + "\n" +
		"ActiveState=" + properties.ActiveState + "\n" +
		"SubState=" + properties.SubState + "\n" +
		"Type=" + properties.Type + "\n" +
		"NotifyAccess=" + properties.NotifyAccess + "\n" +
		"FragmentPath=" + properties.FragmentPath + "\n" +
		"Result=" + properties.Result + "\n" +
		"ExecMainCode=" + properties.ExecMainCode + "\n" +
		"ExecMainStatus=" + properties.ExecMainStatus + "\n"
}

type roundTripper func(*http.Request) (*http.Response, error)

func (function roundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func response(status int, mediaType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{mediaType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
