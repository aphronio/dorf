package legacysystemd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGateMaterializesResumableHandoffForAttestedActiveOrEnabledUnits(t *testing.T) {
	unitDir := t.TempDir()
	writeLegacyManagedUnit(t, unitDir, "dorf-worker.service")
	writeLegacyCloudflaredUnit(t, unitDir)
	runner := &stateRunner{states: map[string]map[string]string{
		"dorf-control-api.service": {"is-active": "inactive", "is-enabled": "disabled"},
		"dorf-worker.service":      {"is-active": "active", "is-enabled": "enabled"},
		"dorf-cloudflared.service": {"is-active": "inactive", "is-enabled": "enabled"},
	}}
	gate := Gate{runner: runner, unitDir: unitDir, expectedFileUID: os.Geteuid(), expectedFileGID: os.Getegid()}
	dataRoot := filepath.Join(t.TempDir(), "dorf-data")
	var output strings.Builder
	err := gate.Check(context.Background(), dataRoot, "0.5.2", os.Geteuid(), os.Getegid(), &output)
	if err == nil || !strings.Contains(err.Error(), "legacy Dorf systemd services remain active or enabled") {
		t.Fatalf("Check() error = %v", err)
	}
	for _, want := range []string{
		"dorf-worker.service",
		"dorf-cloudflared.service",
		"Dorf did not run sudo, systemctl, or the helper",
		"--acknowledge-retire-legacy-dorf-services",
		fmt.Sprintf("--operator-uid' '%d", os.Geteuid()),
		fmt.Sprintf("--operator-gid' '%d", os.Getegid()),
		"After it succeeds, rerun the same Dorf command",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("handoff omitted %q:\n%s", want, output.String())
		}
	}
	entries, err := filepath.Glob(filepath.Join(dataRoot, "bootstrap", "v0.5.2", "*", "retire-systemd.sh"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("materialized retirement helper = %v, %v", entries, err)
	}
	if calls := strings.Join(runner.calls, "\n"); strings.Contains(calls, " stop ") || strings.Contains(calls, " disable ") {
		t.Fatalf("read-only gate mutated systemd:\n%s", calls)
	}
}

func TestGateIsInertWhenEveryLegacyUnitIsInactiveAndDisabled(t *testing.T) {
	runner := &stateRunner{states: map[string]map[string]string{}}
	for _, unit := range legacyUnits {
		runner.states[unit] = map[string]string{"is-active": "inactive", "is-enabled": "disabled"}
	}
	root := filepath.Join(t.TempDir(), "absent-data-root")
	if err := (Gate{runner: runner}).Check(context.Background(), root, "0.5.2", os.Geteuid(), os.Getegid(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("inert gate materialized state: %v", err)
	}
}

func TestGateIsInertWhenSystemdIsNotTheHostInitSystem(t *testing.T) {
	runner := &unavailableRunner{}
	root := filepath.Join(t.TempDir(), "absent-data-root")
	if err := (Gate{runner: runner}).Check(context.Background(), root, "0.5.2", os.Geteuid(), os.Getegid(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "/usr/bin/systemctl is-active -- dorf-control-api.service" {
		t.Fatalf("systemd absence calls = %v", runner.calls)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("absent systemd materialized state: %v", err)
	}
}

func TestGateRefusesForeignUnitWithoutMaterializingAdministratorAction(t *testing.T) {
	unitDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(unitDir, "dorf-worker.service"), []byte("[Service]\nExecStart=/usr/bin/foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &stateRunner{states: map[string]map[string]string{
		"dorf-control-api.service": {"is-active": "inactive", "is-enabled": "disabled"},
		"dorf-worker.service":      {"is-active": "active", "is-enabled": "enabled"},
		"dorf-cloudflared.service": {"is-active": "inactive", "is-enabled": "disabled"},
	}}
	root := filepath.Join(t.TempDir(), "dorf-data")
	err := (Gate{runner: runner, unitDir: unitDir, expectedFileUID: os.Geteuid(), expectedFileGID: os.Getegid()}).Check(context.Background(), root, "0.5.2", os.Geteuid(), os.Getegid(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not an intact legacy Dorf unit") {
		t.Fatalf("foreign unit error = %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("foreign unit materialized helper: %v", err)
	}
}

func TestGateRefusesLegacyUnitForAnotherOperatorIdentity(t *testing.T) {
	unitDir := t.TempDir()
	writeLegacyManagedUnit(t, unitDir, "dorf-worker.service")
	runner := &stateRunner{states: map[string]map[string]string{
		"dorf-control-api.service": {"is-active": "inactive", "is-enabled": "disabled"},
		"dorf-worker.service":      {"is-active": "active", "is-enabled": "enabled"},
		"dorf-cloudflared.service": {"is-active": "inactive", "is-enabled": "disabled"},
	}}
	root := filepath.Join(t.TempDir(), "dorf-data")
	err := (Gate{runner: runner, unitDir: unitDir, expectedFileUID: os.Geteuid(), expectedFileGID: os.Getegid()}).Check(
		context.Background(), root, "0.5.2", os.Geteuid()+1, os.Getegid(), io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "not an intact legacy Dorf unit for operator") {
		t.Fatalf("different operator error = %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("different operator materialized helper: %v", err)
	}
}

func TestGateRefusesModifiedLegacyCloudflaredUnit(t *testing.T) {
	tests := map[string]func(string) string{
		"extra directive": func(unit string) string {
			return strings.Replace(unit, "NoNewPrivileges=true\n", "NoNewPrivileges=true\nEnvironment=FOREIGN=1\n", 1)
		},
		"missing dependency": func(unit string) string {
			return strings.Replace(unit, "After=network-online.target\n", "", 1)
		},
		"changed restart delay": func(unit string) string {
			return strings.Replace(unit, "RestartSec=5s", "RestartSec=1s", 1)
		},
		"missing final newline": func(unit string) string {
			return strings.TrimSuffix(unit, "\n")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			unitDir := t.TempDir()
			path := filepath.Join(unitDir, "dorf-cloudflared.service")
			if err := os.WriteFile(path, []byte(mutate(legacyCloudflaredUnitContents())), 0o644); err != nil {
				t.Fatal(err)
			}
			runner := &stateRunner{states: map[string]map[string]string{
				"dorf-control-api.service": {"is-active": "inactive", "is-enabled": "disabled"},
				"dorf-worker.service":      {"is-active": "inactive", "is-enabled": "disabled"},
				"dorf-cloudflared.service": {"is-active": "active", "is-enabled": "enabled"},
			}}
			dataRoot := filepath.Join(t.TempDir(), "dorf-data")
			err := (Gate{runner: runner, unitDir: unitDir, expectedFileUID: os.Geteuid(), expectedFileGID: os.Getegid()}).Check(
				context.Background(), dataRoot, "0.5.2", os.Geteuid(), os.Getegid(), io.Discard,
			)
			if err == nil || !strings.Contains(err.Error(), "not an intact legacy Dorf unit") {
				t.Fatalf("modified Cloudflare unit error = %v", err)
			}
			if _, err := os.Lstat(dataRoot); !os.IsNotExist(err) {
				t.Fatalf("modified Cloudflare unit materialized helper: %v", err)
			}
		})
	}
}

func TestGateInspectsOnlyTheThreeUnitsShippedByTheSupersededDeployment(t *testing.T) {
	want := []string{"dorf-control-api.service", "dorf-worker.service", "dorf-cloudflared.service"}
	if !slices.Equal(legacyUnits, want) {
		t.Fatalf("legacy units = %v, want %v", legacyUnits, want)
	}
	runner := &stateRunner{states: map[string]map[string]string{}}
	for _, unit := range want {
		runner.states[unit] = map[string]string{"is-active": "inactive", "is-enabled": "disabled"}
	}
	if err := (Gate{runner: runner}).Check(context.Background(), filepath.Join(t.TempDir(), "data"), "0.5.2", os.Geteuid(), os.Getegid(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.calls, "\n"); got != strings.Join([]string{
		"/usr/bin/systemctl is-active -- dorf-control-api.service",
		"/usr/bin/systemctl is-enabled -- dorf-control-api.service",
		"/usr/bin/systemctl is-active -- dorf-worker.service",
		"/usr/bin/systemctl is-enabled -- dorf-worker.service",
		"/usr/bin/systemctl is-active -- dorf-cloudflared.service",
		"/usr/bin/systemctl is-enabled -- dorf-cloudflared.service",
	}, "\n") {
		t.Fatalf("read-only calls:\n%s", got)
	}
}

func writeLegacyManagedUnit(t *testing.T, directory, name string) {
	t.Helper()
	body := []byte(fmt.Sprintf("[Unit]\nDescription=Dorf legacy fixture\n\n[Service]\nUser=%d\nGroup=%d\nExecStart=/usr/local/bin/dorf worker\n", os.Geteuid(), os.Getegid()))
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	contents := []byte("# Managed by Dorf. Local edits are refused.\n# dorf-unit=" + name + "\n# dorf-sha256=" + digest + "\n" + string(body))
	if err := os.WriteFile(filepath.Join(directory, name), contents, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLegacyCloudflaredUnit(t *testing.T, directory string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "dorf-cloudflared.service"), []byte(legacyCloudflaredUnitContents()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func legacyCloudflaredUnitContents() string {
	return fmt.Sprintf(`[Unit]
Description=Dorf Cloudflare Tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=%d
Group=%d
NoNewPrivileges=true
ExecStart="/tmp/cloudflared" --no-autoupdate --config "/tmp/config.yml" tunnel run
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
`, os.Geteuid(), os.Getegid())
}

type stateRunner struct {
	states map[string]map[string]string
	calls  []string
}

func (runner *stateRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	runner.calls = append(runner.calls, name+" "+strings.Join(args, " "))
	operation, unit := args[0], args[len(args)-1]
	state := runner.states[unit][operation]
	if state == "active" || state == "enabled" {
		return state + "\n", nil
	}
	return state + "\n", fmt.Errorf("systemctl exit for %s", state)
}

type unavailableRunner struct{ calls []string }

func (runner *unavailableRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	runner.calls = append(runner.calls, name+" "+strings.Join(args, " "))
	return "Failed to connect to system scope bus via local transport: Operation not permitted\n", fmt.Errorf("systemd unavailable")
}
