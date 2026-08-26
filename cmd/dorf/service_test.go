package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/composeproject"
	"github.com/aphronio/dorf/internal/composeservice"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/release"
)

func TestComposeForegroundCommandsDispatchBeforeDatabaseLoading(t *testing.T) {
	for _, command := range []string{"_compose-provider-gateway", "_compose-cloudflared", "_compose-control-reader-health"} {
		handled, err := composeForegroundCommand(context.Background(), []string{command, "unexpected"}, io.Discard, io.Discard)
		if !handled || err == nil || !strings.Contains(err.Error(), "does not accept arguments") {
			t.Fatalf("command=%s handled=%t err=%v", command, handled, err)
		}
	}
	if handled, err := composeForegroundCommand(context.Background(), []string{"worker"}, io.Discard, io.Discard); handled || err != nil {
		t.Fatalf("ordinary command handled=%t err=%v", handled, err)
	}
}

func TestServiceRestartAndLogsKeepFixedTargetsAndBounds(t *testing.T) {
	manager := &composeServiceTestOperations{}
	spec := composeServiceTestSpec(t.TempDir())
	var stdout, stderr strings.Builder
	if err := composeServiceRestartCommand(context.Background(), manager, spec, []string{"all"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := composeServiceLogsCommand(context.Background(), manager, spec, []string{"api", "--lines", "17"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manager.restarts, []composeservice.Target{composeservice.TargetAll}) ||
		!reflect.DeepEqual(manager.logs, []composeServiceTestLog{{target: composeservice.TargetAPI, lines: 17}}) ||
		!reflect.DeepEqual(manager.operationSpecs, []composeservice.Spec{spec, spec}) {
		t.Fatalf("restarts=%v logs=%v specs=%#v", manager.restarts, manager.logs, manager.operationSpecs)
	}
	before := len(manager.logs)
	if err := composeServiceLogsCommand(context.Background(), manager, spec, []string{"worker", "--lines", "10001"}, &stdout, &stderr); err == nil {
		t.Fatal("logs accepted an unbounded line count")
	}
	if err := composeServiceLogsCommand(context.Background(), manager, spec, []string{"all"}, &stdout, &stderr); err == nil {
		t.Fatal("logs accepted the aggregate target")
	}
	if len(manager.logs) != before {
		t.Fatalf("invalid log request reached manager: %v", manager.logs[before:])
	}
}

func TestServiceStatusShowsComposeServicesAndMachineDiagnostics(t *testing.T) {
	manager := &composeServiceTestOperations{status: readyComposeServiceTestStatus()}
	spec := composeServiceTestSpec(t.TempDir())
	var machine, stderr strings.Builder
	if err := composeServiceStatusCommand(context.Background(), manager, spec, []string{"--output", "json"}, &machine, &stderr); err != nil {
		t.Fatal(err)
	}
	var status composeservice.Status
	decoder := json.NewDecoder(strings.NewReader(machine.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&status); err != nil {
		t.Fatalf("decode status: %v\n%s", err, machine.String())
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("status has trailing JSON: %v", err)
	}
	if !status.Ready || !status.Current || !status.Converged || status.Postgres.Name != "postgres" ||
		status.Worker.Name != "worker" || status.ControlReader.Name != "control-reader" || status.ControlAPI.Name != "control-api" ||
		status.API.URL != composeservice.ControlURL || status.API.Version != "1.2.3" {
		t.Fatalf("machine status=%#v", status)
	}

	var human strings.Builder
	if err := composeServiceStatusCommand(context.Background(), manager, spec, nil, &human, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Dorf Compose services: ready · converged · current",
		"PostgreSQL: ready · running · healthy · current",
		"Worker: ready · running · current",
		"Control reader: ready · running · current",
		"Control API: ready · running · current",
		"Discovery: ready · dorf 1.2.3",
	} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("human status omitted %q:\n%s", want, human.String())
		}
	}
}

func TestServiceReconcileApprovesAppliesAndReportsWithoutRestart(t *testing.T) {
	manager := &composeServiceTestOperations{status: readyComposeServiceTestStatus()}
	spec := composeServiceTestSpec(t.TempDir())
	plan := composeServiceReconcilePlan{
		Summaries: []string{
			"Acquire, verify, and load official Dorf release image · ghcr.io/aphronio/dorf:1.2.3",
			"Prepare and attest exact PostgreSQL image · sha256:postgres",
		},
		Resolve: func(context.Context) (composeservice.Spec, error) { return spec, nil },
	}
	var stdout, stderr strings.Builder
	if err := reconcileComposeServicesWith(context.Background(), manager, plan, composeServiceReconcileOptions{Yes: true}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if manager.applyCalls != 1 || manager.statusCalls != 1 || len(manager.restarts) != 0 {
		t.Fatalf("apply=%d status=%d restarts=%v", manager.applyCalls, manager.statusCalls, manager.restarts)
	}
	for _, want := range []string{
		"Acquire, verify, and load official Dorf release image · ghcr.io/aphronio/dorf:1.2.3",
		"Prepare and attest exact PostgreSQL image · sha256:postgres",
		"Dorf Compose services: ready",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("reconcile output omitted %q:\n%s", want, stdout.String())
		}
	}
}

func TestServiceReconcileRequiresApprovalBeforeApply(t *testing.T) {
	manager := &composeServiceTestOperations{}
	resolved := false
	plan := composeServiceReconcilePlan{
		Summaries: []string{"load exact official image"},
		Resolve: func(context.Context) (composeservice.Spec, error) {
			resolved = true
			return composeServiceTestSpec(t.TempDir()), nil
		},
	}
	var stdout strings.Builder
	err := reconcileComposeServicesWith(context.Background(), manager, plan, composeServiceReconcileOptions{}, &stdout, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rerun dorf service reconcile --yes") {
		t.Fatalf("approval error=%v", err)
	}
	if resolved || manager.applyCalls != 0 || manager.statusCalls != 0 {
		t.Fatalf("unapproved reconcile resolved or mutated state: resolved=%t apply=%d status=%d", resolved, manager.applyCalls, manager.statusCalls)
	}
}

func TestServiceReconcileStopsWhenApprovedImageResolutionFails(t *testing.T) {
	manager := &composeServiceTestOperations{}
	plan := composeServiceReconcilePlan{
		Summaries: []string{"acquire exact image"},
		Resolve: func(context.Context) (composeservice.Spec, error) {
			return composeservice.Spec{}, errors.New("official image unavailable")
		},
	}
	err := reconcileComposeServicesWith(context.Background(), manager, plan, composeServiceReconcileOptions{Yes: true}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "official image unavailable") {
		t.Fatalf("resolution error=%v", err)
	}
	if manager.applyCalls != 0 || manager.statusCalls != 0 {
		t.Fatalf("failed resolution reached lifecycle: apply=%d status=%d", manager.applyCalls, manager.statusCalls)
	}
}

func TestComposeApplyStopsAtLegacySystemdHandoff(t *testing.T) {
	manager := &composeServiceTestOperations{legacyErr: errors.New("administrator handoff required")}
	err := applyComposeServiceSpec(context.Background(), manager, composeServiceTestSpec(t.TempDir()), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "administrator handoff required") {
		t.Fatalf("legacy gate error = %v", err)
	}
	if manager.legacyChecks != 1 || manager.applyCalls != 0 || manager.statusCalls != 0 {
		t.Fatalf("legacy checks=%d apply=%d status=%d", manager.legacyChecks, manager.applyCalls, manager.statusCalls)
	}
}

func TestServiceReconcileLocalImageIsExplicitAndNamed(t *testing.T) {
	options, err := parseComposeServiceReconcileOptions([]string{"--yes", "--local-image", "dorf-proof:1.2.3"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !options.Yes || options.LocalImage != "dorf-proof:1.2.3" {
		t.Fatalf("options=%+v", options)
	}
	var help strings.Builder
	if !serviceSubcommandHelp([]string{"reconcile", "--help"}, &help) || !strings.Contains(help.String(), "--local-image REF") {
		t.Fatalf("reconcile help omitted explicit contributor path: %q", help.String())
	}
}

func TestSetupFinalComposeReusesPreparedImageWithoutAnotherApproval(t *testing.T) {
	root := t.TempDir()
	image := release.ComposeImage{
		Version: "1.2.3", Reference: "dorf-proof:1.2.3",
		ImageID: "sha256:" + strings.Repeat("e", 64), BinarySHA256: strings.Repeat("f", 64),
	}
	source := composeDeploymentSource{
		Version: "1.2.3", BinaryPath: "/proof/dorf", ProjectDir: filepath.Join(root, "compose"),
		Project: composeproject.Spec{
			UID: os.Geteuid(), GID: os.Getegid(), ConfigDir: filepath.Join(root, "config"),
			DataDir: filepath.Join(root, "data"), StateDir: filepath.Join(root, "state"),
			Deployment: deployment.Config{ControlReaderKey: strings.Repeat("c", 64), Database: deployment.Database{
				Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret",
				Image: "postgres:17", ImageID: "sha256:" + strings.Repeat("a", 64),
			}},
			Gateway: &composeproject.OptionalService{
				StatePath: filepath.Join(root, "data", "provider-gateway"), Digest: strings.Repeat("b", 64), PublishAddress: "10.20.30.1",
			},
		},
	}
	manager := &composeServiceTestOperations{status: readyComposeServiceTestStatus()}
	var stdout strings.Builder
	if err := reconcileSetupFinalServicesWith(context.Background(), manager, source, image, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if manager.applyCalls != 1 || len(manager.operationSpecs) != 1 || manager.operationSpecs[0].Project.Image != image {
		t.Fatalf("apply=%d specs=%#v", manager.applyCalls, manager.operationSpecs)
	}
	if strings.Contains(stdout.String(), "Dorf Compose changes:") || strings.Contains(stdout.String(), "Acquire, verify") {
		t.Fatalf("final reconciliation asked for a second approval or acquisition:\n%s", stdout.String())
	}
}

func TestSetupResumeUsesOnlyTheProtectedMatchingImageAuthority(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	image := release.ComposeImage{
		Version: "1.2.3", Reference: "dorf-proof:1.2.3",
		ImageID: "sha256:" + strings.Repeat("e", 64), BinarySHA256: hex.EncodeToString(digest[:]),
	}
	projectDir := filepath.Join(t.TempDir(), "compose")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(projectDir, composeproject.ImageFile)
	if err := os.WriteFile(imagePath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	source := composeDeploymentSource{
		Version: "1.2.3", BinaryPath: binary, ProjectDir: projectDir,
		Project: composeproject.Spec{UID: os.Geteuid(), GID: os.Getegid()},
	}
	attestFound := func(context.Context, string, release.ComposeImage) (bool, error) { return true, nil }
	got, resumed, err := setupComposeImageResume(context.Background(), source, image.Reference, attestFound)
	if err != nil || !resumed || got != image {
		t.Fatalf("image=%#v resumed=%t error=%v", got, resumed, err)
	}
	if _, resumed, err := setupComposeImageResume(context.Background(), source, "another:1.2.3", attestFound); err != nil || resumed {
		t.Fatalf("mismatched local image resumed=%t error=%v", resumed, err)
	}
	if _, resumed, err := setupComposeImageResume(context.Background(), source, image.Reference, func(context.Context, string, release.ComposeImage) (bool, error) { return false, nil }); err == nil || resumed || !strings.Contains(err.Error(), "reload that exact image") {
		t.Fatalf("pruned local image resumed=%t error=%v", resumed, err)
	}
	if err := os.Chmod(imagePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := setupComposeImageResume(context.Background(), source, image.Reference, attestFound); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("unprotected image authority error=%v", err)
	}
}

func TestSetupResumeReacquiresOnlyTheSamePrunedOfficialImage(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	image := release.ComposeImage{
		Version: "1.2.3", ReleaseTag: "v1.2.3", Reference: "ghcr.io/aphronio/dorf:1.2.3",
		ImageID: "sha256:" + strings.Repeat("e", 64), BinarySHA256: hex.EncodeToString(digest[:]),
		GitHubAssetSHA256: strings.Repeat("a", 64), ArchiveChecksumSHA256: strings.Repeat("a", 64),
		ChecksumAssetGitHubSHA256: strings.Repeat("b", 64),
	}
	projectDir := filepath.Join(t.TempDir(), "compose")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, composeproject.ImageFile), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	source := composeDeploymentSource{
		Version: "1.2.3", BinaryPath: binary, ProjectDir: projectDir,
		Project: composeproject.Spec{UID: os.Geteuid(), GID: os.Getegid()},
	}

	got, resumed, err := setupComposeImageResume(context.Background(), source, "", func(context.Context, string, release.ComposeImage) (bool, error) { return false, nil })
	if err != nil || resumed || got != image {
		t.Fatalf("pruned official image=%#v resumed=%t error=%v", got, resumed, err)
	}
	if err := requireMatchingReacquiredComposeImage(got, got); err != nil {
		t.Fatal(err)
	}
	changed := got
	changed.ImageID = "sha256:" + strings.Repeat("f", 64)
	if err := requireMatchingReacquiredComposeImage(got, changed); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed reacquisition error=%v", err)
	}
}

func TestBootstrapAdministratorCommandQuotesEveryArgument(t *testing.T) {
	got := shellJoin([]string{"sudo", "--", "/tmp/operator's helper.sh", "--user", "alice; id"})
	want := `'sudo' '--' '/tmp/operator'"'"'s helper.sh' '--user' 'alice; id'`
	if got != want {
		t.Fatalf("shell command = %q, want %q", got, want)
	}
}

func TestSetupDataRootProtectsOwnedDirectoryAndRejectsLinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dorf")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureSetupDataRoot(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("protected root mode=%v error=%v", info.Mode(), err)
	}
	link := filepath.Join(t.TempDir(), "dorf-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureSetupDataRoot(link); err == nil || !strings.Contains(err.Error(), "real operator-owned") {
		t.Fatalf("linked data root error=%v", err)
	}
}

func TestSetupIncusReadinessHandoffOnlyOffersHelperForDefaultSocket(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cause := errors.New("endpoint unavailable")
	for _, authority := range []*deployment.Incus{
		{Endpoint: "unix:///run/incus/custom.socket"},
	} {
		var output strings.Builder
		err := setupIncusReadinessHandoff(authority, cause, &output)
		if err == nil || !strings.Contains(err.Error(), authority.Endpoint) || !strings.Contains(err.Error(), "repair that endpoint") {
			t.Fatalf("endpoint=%q error=%v", authority.Endpoint, err)
		}
		if output.Len() != 0 {
			t.Fatalf("custom endpoint %q printed administrator helper:\n%s", authority.Endpoint, output.String())
		}
	}
	if entries, err := os.ReadDir(dataHome); err != nil || len(entries) != 0 {
		t.Fatalf("custom endpoints materialized bootstrap files: entries=%v err=%v", entries, err)
	}
	remote := &deployment.Incus{Endpoint: "https://incus.example:8443"}
	var remoteOutput strings.Builder
	remoteErr := setupIncusReadinessHandoff(remote, cause, &remoteOutput)
	if remoteErr == nil || !strings.Contains(remoteErr.Error(), remote.Endpoint) || !strings.Contains(remoteErr.Error(), "not supported") {
		t.Fatalf("remote endpoint error=%v", remoteErr)
	}
	if remoteOutput.Len() != 0 {
		t.Fatalf("remote endpoint printed administrator helper:\n%s", remoteOutput.String())
	}

	var output strings.Builder
	defaultAuthority := &deployment.Incus{Endpoint: "unix://" + incus.DefaultUnixSocket}
	err := setupIncusReadinessHandoff(defaultAuthority, cause, &output)
	if err == nil || !strings.Contains(err.Error(), "administrator handoff") {
		t.Fatalf("default endpoint error=%v", err)
	}
	for _, want := range []string{"needs administrator preparation", "sudo", "--initialize-pristine"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("default endpoint handoff omitted %q:\n%s", want, output.String())
		}
	}
}

func TestServiceReadinessWaitsForAPIProbe(t *testing.T) {
	starting := readyComposeServiceTestStatus()
	starting.Ready = false
	starting.API = composeservice.APIStatus{URL: composeservice.ControlURL, Detail: "connection refused"}
	manager := &composeServiceTestOperations{statuses: []composeservice.Status{starting, readyComposeServiceTestStatus()}}
	status, err := waitForComposeServicesReady(context.Background(), manager, composeServiceTestSpec(t.TempDir()), 50*time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || manager.statusCalls != 2 {
		t.Fatalf("ready=%t status calls=%d", status.Ready, manager.statusCalls)
	}
}

func TestExistingComposeProjectIsUpdaterNoOpGate(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "compose")
	installed, err := existingComposeDeployment(projectDir)
	if err != nil || installed {
		t.Fatalf("absent project installed=%t err=%v", installed, err)
	}
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	installed, err = existingComposeDeployment(projectDir)
	if err != nil || !installed {
		t.Fatalf("partial project installed=%t err=%v", installed, err)
	}

	symlink := filepath.Join(t.TempDir(), "compose-link")
	if err := os.Symlink(projectDir, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := existingComposeDeployment(symlink); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink project error=%v", err)
	}
	file := filepath.Join(t.TempDir(), "compose-file")
	if err := os.WriteFile(file, []byte("not a project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := existingComposeDeployment(file); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("file project error=%v", err)
	}
}

func TestComposeServiceRootDispatchPrecedesDeploymentComposition(t *testing.T) {
	handled, err := composeServiceRootCommand(context.Background(), []string{"doctor"}, io.Discard, io.Discard)
	if handled || err != nil {
		t.Fatalf("unrelated command handled=%t err=%v", handled, err)
	}
	var stderr strings.Builder
	err = run(context.Background(), []string{"service", "--help"}, io.Discard, &stderr)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(stderr.String(), "dorf service") {
		t.Fatalf("service help err=%v stderr=%q", err, stderr.String())
	}
	for _, subcommand := range []string{"reconcile", "status", "restart", "logs"} {
		stderr.Reset()
		err = run(context.Background(), []string{"service", subcommand, "--help"}, io.Discard, &stderr)
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("service %s help resolved deployment first: err=%v stderr=%q", subcommand, err, stderr.String())
		}
	}
}

func composeServiceTestSpec(root string) composeservice.Spec {
	return composeservice.Spec{
		ProjectDir: filepath.Join(root, "compose"),
		Project: composeproject.Project{
			Image: release.ComposeImage{Version: "1.2.3", Reference: "dorf-local:test", ImageID: "sha256:" + strings.Repeat("e", 64), BinarySHA256: strings.Repeat("f", 64)},
			Runtime: composeproject.Runtime{Deployment: deployment.Config{Database: deployment.Database{
				ImageID: "sha256:postgres",
			}}},
		},
	}
}

func readyComposeServiceTestStatus() composeservice.Status {
	return composeservice.Status{
		Postgres:      composeservice.ServiceStatus{Name: "postgres", State: "running", Health: "healthy", Current: true, Ready: true, Detail: "ready"},
		Worker:        composeservice.ServiceStatus{Name: "worker", State: "running", Current: true, Ready: true, Detail: "ready"},
		ControlReader: composeservice.ServiceStatus{Name: "control-reader", State: "running", Current: true, Ready: true, Detail: "ready"},
		ControlAPI:    composeservice.ServiceStatus{Name: "control-api", State: "running", Current: true, Ready: true, Detail: "ready"},
		API: composeservice.APIStatus{
			URL: composeservice.ControlURL, Version: "1.2.3", Ready: true, Detail: "dorf 1.2.3",
		},
		Current: true, Converged: true, Ready: true,
	}
}

type composeServiceTestLog struct {
	target composeservice.Target
	lines  int
}

type composeServiceTestOperations struct {
	status         composeservice.Status
	statuses       []composeservice.Status
	restarts       []composeservice.Target
	logs           []composeServiceTestLog
	operationSpecs []composeservice.Spec
	applyCalls     int
	statusCalls    int
	legacyChecks   int
	legacyErr      error
}

func (operations *composeServiceTestOperations) CheckLegacySystemd(context.Context, composeservice.Spec, io.Writer) error {
	operations.legacyChecks++
	return operations.legacyErr
}

func (operations *composeServiceTestOperations) Apply(_ context.Context, spec composeservice.Spec, _, _ io.Writer) error {
	operations.operationSpecs = append(operations.operationSpecs, spec)
	operations.applyCalls++
	return nil
}

func (operations *composeServiceTestOperations) Status(context.Context, composeservice.Spec) (composeservice.Status, error) {
	operations.statusCalls++
	if len(operations.statuses) > 0 {
		status := operations.statuses[0]
		operations.statuses = operations.statuses[1:]
		return status, nil
	}
	return operations.status, nil
}

func (operations *composeServiceTestOperations) Restart(_ context.Context, spec composeservice.Spec, target composeservice.Target, _, _ io.Writer) error {
	operations.operationSpecs = append(operations.operationSpecs, spec)
	operations.restarts = append(operations.restarts, target)
	return nil
}

func (operations *composeServiceTestOperations) Logs(_ context.Context, spec composeservice.Spec, target composeservice.Target, lines int, _, _ io.Writer) error {
	operations.operationSpecs = append(operations.operationSpecs, spec)
	operations.logs = append(operations.logs, composeServiceTestLog{target: target, lines: lines})
	return nil
}
