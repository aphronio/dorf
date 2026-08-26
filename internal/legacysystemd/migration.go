// Package legacysystemd owns the read-only handoff from Dorf's superseded
// systemd deployment to its Compose deployment. It never runs a mutating
// service-manager command.
package legacysystemd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	bootstraphelper "github.com/aphronio/dorf/scripts/bootstrap"
)

const (
	defaultUnitDir = "/etc/systemd/system"
	systemctlPath  = "/usr/bin/systemctl"
	inspectTimeout = 5 * time.Second
	maximumState   = 256
	maximumUnit    = 1 << 20
)

var legacyUnits = []string{
	"dorf-control-api.service",
	"dorf-worker.service",
	"dorf-cloudflared.service",
}

var cloudflaredExec = regexp.MustCompile(`^ExecStart="[^"\r\n]+" --no-autoupdate --config "[^"\r\n]+" tunnel run$`)

type commandRunner interface {
	Output(context.Context, string, ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	output, err := command.CombinedOutput()
	return string(output), err
}

// Gate observes the fixed legacy unit names and materializes the exact
// administrator migration only when an intact old Dorf unit remains active or
// enabled. Its fields are private test seams; the zero value is production.
type Gate struct {
	runner          commandRunner
	unitDir         string
	expectedFileUID int
	expectedFileGID int
}

// Check allows Compose reconciliation to continue only when the superseded
// systemd deployment is inert. Detection and materialization are unprivileged;
// the printed helper remains an explicit administrator action.
func (gate Gate) Check(ctx context.Context, dataRoot, version string, operatorUID, operatorGID int, output io.Writer) error {
	if operatorUID <= 0 || operatorGID < 0 {
		return fmt.Errorf("legacy systemd operator identity is invalid")
	}
	units, err := gate.activeOrEnabled(ctx, operatorUID, operatorGID)
	if err != nil {
		return err
	}
	if len(units) == 0 {
		return nil
	}
	artifact, err := bootstraphelper.Materialize(dataRoot, version, bootstraphelper.RetireSystemd)
	if err != nil {
		return fmt.Errorf("materialize exact legacy-systemd retirement helper: %w", err)
	}
	fmt.Fprintln(output, "\nLegacy Dorf systemd services conflict with the Compose deployment:")
	for _, unit := range units {
		fmt.Fprintln(output, "  "+unit)
	}
	fmt.Fprintln(output, "Dorf did not run sudo, systemctl, or the helper.")
	fmt.Fprintln(output, "Inspect the version-matched helper, then run this exact command explicitly:")
	fmt.Fprintln(output, "  "+shellJoin([]string{
		"sudo", "--", artifact.Path,
		"--operator-uid", fmt.Sprintf("%d", operatorUID),
		"--operator-gid", fmt.Sprintf("%d", operatorGID),
		"--acknowledge-retire-legacy-dorf-services",
	}))
	fmt.Fprintln(output, "After it succeeds, rerun the same Dorf command.")
	return fmt.Errorf("legacy Dorf systemd services remain active or enabled: %s; complete the administrator handoff and rerun the same Dorf command", strings.Join(units, ", "))
}

func (gate Gate) activeOrEnabled(ctx context.Context, operatorUID, operatorGID int) ([]string, error) {
	if gate.runner == nil {
		gate.runner = execRunner{}
	}
	if gate.unitDir == "" {
		gate.unitDir = defaultUnitDir
	}
	if !filepath.IsAbs(gate.unitDir) || filepath.Clean(gate.unitDir) != gate.unitDir || gate.unitDir == "/" {
		return nil, fmt.Errorf("legacy systemd unit directory must be one clean absolute path")
	}
	if gate.expectedFileUID < 0 || gate.expectedFileGID < 0 {
		return nil, fmt.Errorf("legacy systemd expected unit owner is invalid")
	}

	inspectCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	var found []string
	for _, unit := range legacyUnits {
		active, unavailable, err := gate.state(inspectCtx, "is-active", unit)
		if err != nil {
			return nil, err
		}
		if unavailable {
			// A host without a running systemd manager cannot have an active
			// legacy systemd runtime. Compose remains supported there and the
			// gate must not turn dormant unit files into a deployment blocker.
			return nil, nil
		}
		enabled, enabledUnavailable, err := gate.state(inspectCtx, "is-enabled", unit)
		if err != nil {
			return nil, err
		}
		if enabledUnavailable {
			return nil, nil
		}
		if !active && !enabled {
			continue
		}
		if err := gate.attestUnit(unit, operatorUID, operatorGID); err != nil {
			return nil, err
		}
		found = append(found, unit)
	}
	return found, nil
}

func (gate Gate) state(ctx context.Context, operation, unit string) (value, unavailable bool, err error) {
	raw, runErr := gate.runner.Output(ctx, systemctlPath, operation, "--", unit)
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return false, false, runErr
	}
	if errors.Is(runErr, os.ErrNotExist) {
		return false, true, nil
	}
	state := strings.TrimSpace(raw)
	if strings.Contains(state, "System has not been booted with systemd as init system") ||
		(strings.HasPrefix(state, "Failed to connect to ") && strings.Contains(state, " bus")) {
		return false, true, nil
	}
	if len(state) > maximumState || strings.ContainsAny(state, "\r\n") {
		return false, false, fmt.Errorf("inspect legacy systemd %s state for %s: invalid bounded response", operation, unit)
	}
	switch operation {
	case "is-active":
		switch state {
		case "active", "reloading", "activating", "deactivating":
			return true, false, nil
		case "inactive", "failed", "unknown", "not-found":
			return false, false, nil
		}
	case "is-enabled":
		switch state {
		case "enabled", "enabled-runtime", "linked", "linked-runtime", "alias":
			return true, false, nil
		case "disabled", "static", "indirect", "generated", "transient", "masked", "masked-runtime", "not-found":
			return false, false, nil
		}
	default:
		return false, false, fmt.Errorf("unsupported legacy systemd observation %q", operation)
	}
	if runErr != nil {
		return false, false, fmt.Errorf("inspect legacy systemd %s state for %s: %w", operation, unit, runErr)
	}
	return false, false, fmt.Errorf("inspect legacy systemd %s state for %s: unexpected state %q", operation, unit, state)
}

func (gate Gate) attestUnit(name string, operatorUID, operatorGID int) error {
	path := filepath.Join(gate.unitDir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s is active or enabled but is not an intact legacy Dorf unit: %w", name, err)
	}
	owner, owned := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o644 || !owned || int(owner.Uid) != gate.expectedFileUID || int(owner.Gid) != gate.expectedFileGID || info.Size() < 1 || info.Size() > maximumUnit {
		return fmt.Errorf("%s is active or enabled but is not an intact legacy Dorf unit", name)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read active or enabled legacy unit %s: %w", name, err)
	}
	var valid bool
	switch name {
	case "dorf-control-api.service", "dorf-worker.service":
		valid = validateManagedUnit(name, contents, operatorUID, operatorGID)
	case "dorf-cloudflared.service":
		valid = validateCloudflaredUnit(contents, operatorUID, operatorGID)
	}
	if !valid {
		return fmt.Errorf("%s is active or enabled but is not an intact legacy Dorf unit for operator %d:%d", name, operatorUID, operatorGID)
	}
	return nil
}

func validateManagedUnit(name string, contents []byte, operatorUID, operatorGID int) bool {
	first, rest, found := bytes.Cut(contents, []byte("\n"))
	if !found || string(first) != "# Managed by Dorf. Local edits are refused." {
		return false
	}
	unitLine, rest, found := bytes.Cut(rest, []byte("\n"))
	if !found || string(unitLine) != "# dorf-unit="+name {
		return false
	}
	digestLine, body, found := bytes.Cut(rest, []byte("\n"))
	if !found || !bytes.HasPrefix(digestLine, []byte("# dorf-sha256=")) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(string(digestLine), "# dorf-sha256="))
	got := sha256.Sum256(body)
	if err != nil || len(want) != sha256.Size || !bytes.Equal(want, got[:]) {
		return false
	}
	text := string(body)
	return countExactLine(text, fmt.Sprintf("User=%d", operatorUID)) == 1 && countExactLine(text, fmt.Sprintf("Group=%d", operatorGID)) == 1
}

func validateCloudflaredUnit(contents []byte, operatorUID, operatorGID int) bool {
	lines := strings.Split(string(contents), "\n")
	expected := []string{
		"[Unit]",
		"Description=Dorf Cloudflare Tunnel",
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=notify",
		fmt.Sprintf("User=%d", operatorUID),
		fmt.Sprintf("Group=%d", operatorGID),
		"NoNewPrivileges=true",
		"",
		"Restart=on-failure",
		"RestartSec=5s",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}
	if len(lines) != len(expected) || !cloudflaredExec.MatchString(lines[10]) {
		return false
	}
	for index := range expected {
		if index == 10 {
			continue
		}
		if lines[index] != expected[index] {
			return false
		}
	}
	return true
}

func countExactLine(contents, line string) int {
	count := 0
	for _, candidate := range strings.Split(contents, "\n") {
		if candidate == line {
			count++
		}
	}
	return count
}

func shellJoin(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = "'" + strings.ReplaceAll(argument, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}
