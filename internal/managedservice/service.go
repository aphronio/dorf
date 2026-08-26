// Package managedservice owns Dorf's two compiled systemd system units and
// their narrow host lifecycle. It deliberately does not own HTTPS ingress,
// deployment configuration, or application process composition.
package managedservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	ControlAPIUnit = "dorf-control-api.service"
	WorkerUnit     = "dorf-worker.service"
	ControlAddress = "127.0.0.1:8745"
	ControlURL     = "http://" + ControlAddress
	DefaultUnitDir = "/etc/systemd/system"
	systemctlPath  = "/usr/bin/systemctl"
	journalctlPath = "/usr/bin/journalctl"
	installPath    = "/usr/bin/install"
	sudoPath       = "/usr/bin/sudo"

	managedHeader = "# Managed by Dorf. Local edits are refused."
	maxUnitBytes  = 1 << 20
	maxProbeBody  = 64 << 10
	statusTimeout = 5 * time.Second
)

var (
	ErrForeignUnit             = errors.New("foreign systemd unit")
	ErrStalePlan               = errors.New("managed-service plan is stale")
	ErrUnsupportedConfigSource = errors.New("managed services require persisted default deployment configuration")
)

// Operator is the exact account selected by setup. Numeric identities avoid
// silently changing service ownership after an account rename.
type Operator struct {
	UID  int
	GID  int
	Home string
}

// Configuration describes configuration authority without carrying any
// configuration value or secret. The current managed-service boundary is the
// protected deployment.json beneath the operator's default HOME. Callers must
// report every active DORF/XDG override by name and whether the database came
// from DORF_DATABASE_URL; those process-only authorities are refused rather
// than silently dropped or copied into a world-readable unit.
type Configuration struct {
	DeploymentPath       string
	ExternalDatabase     bool
	EnvironmentOverrides []string
}

// Spec is the complete input that can affect compiled unit contents.
type Spec struct {
	Binary   string
	Operator Operator
}

func (s Spec) Validate() error {
	if s.Operator.UID < 0 || s.Operator.GID < 0 {
		return fmt.Errorf("managed-service operator UID and GID must be non-negative")
	}
	if err := validateSystemdPath("operator HOME", s.Operator.Home, false); err != nil {
		return err
	}
	if err := validateSystemdPath("Dorf binary", s.Binary, true); err != nil {
		return err
	}
	return nil
}

func (c Configuration) Validate(home string) error {
	wantDeployment := filepath.Join(home, ".config", "dorf", "deployment.json")
	if filepath.Clean(c.DeploymentPath) != wantDeployment || c.DeploymentPath != wantDeployment {
		return fmt.Errorf("%w at %s; current deployment path resolves elsewhere", ErrUnsupportedConfigSource, wantDeployment)
	}
	overrides, err := configurationOverrideNames(c.EnvironmentOverrides)
	if err != nil {
		return err
	}
	if c.ExternalDatabase {
		overrides = append(overrides, "DORF_DATABASE_URL")
		sort.Strings(overrides)
		overrides = slices.Compact(overrides)
	}
	if len(overrides) > 0 {
		return fmt.Errorf("%w; process-only configuration would be lost: %s", ErrUnsupportedConfigSource, strings.Join(overrides, ", "))
	}
	return nil
}

func configurationOverrideNames(names []string) ([]string, error) {
	result := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || name[0] < 'A' || name[0] > 'Z' {
			return nil, fmt.Errorf("managed-service configuration override name %q is invalid", name)
		}
		for _, character := range name[1:] {
			if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
				return nil, fmt.Errorf("managed-service configuration override name %q is invalid", name)
			}
		}
		result = append(result, name)
	}
	sort.Strings(result)
	return slices.Compact(result), nil
}

func validateSystemdPath(label, value string, requireFile bool) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
		return fmt.Errorf("%s must be one clean absolute path", label)
	}
	if requireFile && strings.HasSuffix(value, "/") {
		return fmt.Errorf("%s must identify one file", label)
	}
	for _, character := range value {
		if character <= ' ' || character == 0x7f || strings.ContainsRune(`%$\'":`, character) {
			return fmt.Errorf("%s contains characters unsupported by the compiled systemd unit", label)
		}
	}
	return nil
}

// Unit is one complete compiled systemd unit.
type Unit struct {
	Name     string
	Contents []byte
}

type unitDefinition struct {
	name        string
	description string
	arguments   []string
	writesState bool
}

var unitDefinitions = []unitDefinition{
	{name: WorkerUnit, description: "Dorf durable Job worker", arguments: []string{"worker"}, writesState: true},
	{name: ControlAPIUnit, description: "Dorf private control API", arguments: []string{"serve", "--listen", ControlAddress}},
}

// RenderUnits returns both complete unit files. The ownership envelope hashes
// the generated body so a later release can distinguish an intact older Dorf
// unit from a foreign or locally edited collision without a separate registry.
func RenderUnits(spec Spec) ([]Unit, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	units := make([]Unit, 0, len(unitDefinitions))
	for _, definition := range unitDefinitions {
		body := renderUnitBody(spec, definition)
		digest := sha256.Sum256(body)
		contents := fmt.Sprintf("%s\n# dorf-unit=%s\n# dorf-sha256=%s\n%s", managedHeader, definition.name, hex.EncodeToString(digest[:]), body)
		units = append(units, Unit{Name: definition.name, Contents: []byte(contents)})
	}
	return units, nil
}

func renderUnitBody(spec Spec, definition unitDefinition) []byte {
	arguments := append([]string{spec.Binary}, definition.arguments...)
	deployment := filepath.Join(spec.Operator.Home, ".config", "dorf", "deployment.json")
	githubCredentials := filepath.Join(spec.Operator.Home, ".config", "dorf", "integrations", "github", "credentials.json")
	sharedState := filepath.Join(spec.Operator.Home, ".local", "share", "dorf")
	retainedState := filepath.Join(spec.Operator.Home, ".local", "state", "dorf")
	var body strings.Builder
	fmt.Fprintf(&body, "[Unit]\nDescription=%s\n\n", definition.description)
	body.WriteString("[Service]\nType=notify\nNotifyAccess=main\n")
	fmt.Fprintf(&body, "User=%d\nGroup=%d\nEnvironment=\"HOME=%s\"\n", spec.Operator.UID, spec.Operator.GID, spec.Operator.Home)
	fmt.Fprintf(&body, "ExecStartPre=%s migrate\nExecStart=%s\n", spec.Binary, strings.Join(arguments, " "))
	body.WriteString("Restart=on-failure\nRestartSec=2s\nTimeoutStartSec=2min\nTimeoutStopSec=30s\nUMask=0077\n")
	body.WriteString("NoNewPrivileges=true\nPrivateTmp=true\nPrivateDevices=true\nProtectSystem=strict\nProtectHome=tmpfs\n")
	fmt.Fprintf(&body, "BindReadOnlyPaths=-%s -%s -%s", spec.Binary, deployment, githubCredentials)
	if definition.writesState {
		fmt.Fprintf(&body, "\nBindPaths=-%s -%s\n", sharedState, retainedState)
	} else {
		fmt.Fprintf(&body, " -%s -%s\n", sharedState, retainedState)
	}
	body.WriteString("CapabilityBoundingSet=\nAmbientCapabilities=\nRestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\n")
	body.WriteString("ProtectKernelTunables=true\nProtectKernelModules=true\nProtectControlGroups=true\nLockPersonality=true\nRestrictSUIDSGID=true\n\n")
	body.WriteString("[Install]\nWantedBy=multi-user.target\n")
	return []byte(body.String())
}

type CommandRunner interface {
	Run(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error
	Output(context.Context, string, ...string) (string, error)
}

type ExecRunner struct{}

var commandEnvironment = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}

func (ExecRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = commandEnvironment
	command.Stdin = stdin
	command.Stdout, command.Stderr = stdout, stderr
	return command.Run()
}

func (ExecRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = commandEnvironment
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if len(detail) > 4096 {
			detail = detail[:4096] + "…"
		}
		if detail != "" {
			return "", fmt.Errorf("%s: %w: %s", name, err, detail)
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(output), nil
}

// Manager performs only the fixed Dorf unit operations. UseSudo controls the
// exact non-interactive command prefix; interactive authorization remains the
// caller's setup/CLI responsibility.
type Manager struct {
	Runner          CommandRunner
	HTTPClient      *http.Client
	UseSudo         bool
	ExpectedVersion string
	unitDir         string
	expectedOwner   unitFileOwner // package-test override; production zero is root:root
}

type unitFileOwner struct {
	uid uint32
	gid uint32
}

func (m Manager) configured() (Manager, error) {
	if m.Runner == nil {
		m.Runner = ExecRunner{}
	}
	if m.unitDir == "" {
		m.unitDir = DefaultUnitDir
	}
	if !filepath.IsAbs(m.unitDir) || filepath.Clean(m.unitDir) != m.unitDir || m.unitDir == "/" {
		return Manager{}, fmt.Errorf("managed-service unit directory must be one clean absolute path")
	}
	if m.HTTPClient == nil {
		m.HTTPClient = &http.Client{
			Timeout: 3 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return m, nil
}

type Change struct {
	Unit   string `json:"unit"`
	Action string `json:"action"`
}

type plannedUnit struct {
	unit               Unit
	path               string
	observed           observedFile
	observedProperties planProperties
	install            bool
	update             bool
	reload             bool
	enable             bool
}

// Plan binds presentation and execution to the same observed unit files and
// plan-relevant systemd properties.
// Its mutable details are intentionally private so callers cannot widen the
// approved operations.
type Plan struct {
	unitDir string
	units   []plannedUnit
	changes []Change
}

func (p Plan) Empty() bool { return len(p.changes) == 0 }

func (p Plan) Changes() []Change { return append([]Change{}, p.changes...) }

func (p Plan) Summaries() []string {
	summaries := make([]string, 0, len(p.changes))
	for _, change := range p.changes {
		switch change.Action {
		case "install":
			summaries = append(summaries, "Install "+change.Unit)
		case "update":
			summaries = append(summaries, "Update "+change.Unit)
		case "reload":
			summaries = append(summaries, "Reload systemd for managed Dorf units")
		case "enable":
			summaries = append(summaries, "Enable "+change.Unit+" across reboot")
		}
	}
	return slices.Compact(summaries)
}

// Plan observes both fixed names before returning. Any non-Dorf file,
// modified ownership envelope, symlink, vendor unit, or transient collision
// refuses the entire plan before mutation.
func (m Manager) Plan(ctx context.Context, spec Spec, configuration Configuration) (Plan, error) {
	m, err := m.configured()
	if err != nil {
		return Plan{}, err
	}
	if err := configuration.Validate(spec.Operator.Home); err != nil {
		return Plan{}, err
	}
	units, err := RenderUnits(spec)
	if err != nil {
		return Plan{}, err
	}
	planned := make([]plannedUnit, 0, len(units))
	for _, unit := range units {
		path := filepath.Join(m.unitDir, unit.Name)
		observed, err := readUnitFile(path)
		if err != nil {
			return Plan{}, err
		}
		if observed.exists {
			if err := validateOwnedUnit(unit.Name, observed.contents); err != nil {
				return Plan{}, fmt.Errorf("%w %s at %s: %v", ErrForeignUnit, unit.Name, path, err)
			}
		}
		planned = append(planned, plannedUnit{unit: unit, path: path, observed: observed})
	}

	plan := Plan{unitDir: m.unitDir, units: planned}
	needsReload := false
	for index := range plan.units {
		item := &plan.units[index]
		properties, err := m.showUnit(ctx, item.unit.Name)
		if err != nil {
			return Plan{}, fmt.Errorf("inspect %s: %w", item.unit.Name, err)
		}
		if err := validateLoadedAuthority(item.path, item.observed.exists, properties); err != nil {
			return Plan{}, fmt.Errorf("%w %s: %v", ErrForeignUnit, item.unit.Name, err)
		}
		item.observedProperties = properties.planProperties()
		item.install = !item.observed.exists
		item.update = item.observed.exists && (!bytes.Equal(item.observed.contents, item.unit.Contents) || m.validateUnitMetadata(item.observed) != nil)
		item.reload = item.install || item.update || properties.LoadState != "loaded" || properties.NeedDaemonReload == "yes"
		item.enable = properties.UnitFileState != "enabled"
		needsReload = needsReload || item.reload
		if item.install {
			plan.changes = append(plan.changes, Change{Unit: item.unit.Name, Action: "install"})
		} else if item.update {
			plan.changes = append(plan.changes, Change{Unit: item.unit.Name, Action: "update"})
		}
	}
	if needsReload {
		plan.changes = append(plan.changes, Change{Unit: "systemd", Action: "reload"})
	}
	for _, item := range plan.units {
		if item.enable {
			plan.changes = append(plan.changes, Change{Unit: item.unit.Name, Action: "enable"})
		}
	}
	return plan, nil
}

// Apply executes exactly a previously observed Plan and refuses if either
// unit file or plan-relevant systemd state changed after approval. Process
// restart and readiness remain the reconciliation caller's responsibility.
func (m Manager) Apply(ctx context.Context, plan Plan, stdout, stderr io.Writer) error {
	m, err := m.configured()
	if err != nil {
		return err
	}
	if plan.unitDir == "" || plan.unitDir != m.unitDir || len(plan.units) != len(unitDefinitions) {
		return fmt.Errorf("%w: plan does not belong to this manager", ErrStalePlan)
	}
	for _, item := range plan.units {
		current, err := readUnitFile(item.path)
		if err != nil {
			return err
		}
		if !sameObservedFile(current, item.observed) {
			return fmt.Errorf("%w: %s changed after observation", ErrStalePlan, item.unit.Name)
		}
		properties, err := m.showUnit(ctx, item.unit.Name)
		if err != nil {
			return fmt.Errorf("recheck %s: %w", item.unit.Name, err)
		}
		if properties.planProperties() != item.observedProperties {
			return fmt.Errorf("%w: %s systemd plan state changed after observation", ErrStalePlan, item.unit.Name)
		}
		if err := validateLoadedAuthority(item.path, current.exists, properties); err != nil {
			return fmt.Errorf("%w %s: %v", ErrForeignUnit, item.unit.Name, err)
		}
	}

	for _, item := range plan.units {
		if !item.install && !item.update {
			continue
		}
		if err := m.installUnit(ctx, item, stdout, stderr); err != nil {
			return err
		}
	}
	if slices.ContainsFunc(plan.units, func(item plannedUnit) bool { return item.reload }) {
		if err := m.runAdmin(ctx, stdout, stderr, systemctlPath, "daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd after installing Dorf services: %w", err)
		}
	}
	for _, item := range plan.units {
		if item.enable {
			if err := m.runAdmin(ctx, stdout, stderr, systemctlPath, "enable", item.unit.Name); err != nil {
				return fmt.Errorf("enable %s: %w", item.unit.Name, err)
			}
		}
	}
	return nil
}

func (m Manager) installUnit(ctx context.Context, item plannedUnit, stdout, stderr io.Writer) error {
	if err := m.runAdminInput(ctx, bytes.NewReader(item.unit.Contents), stdout, stderr, installPath, "-m", "0644", "/dev/stdin", item.path); err != nil {
		return fmt.Errorf("install %s: %w", item.unit.Name, err)
	}
	installed, err := readUnitFile(item.path)
	if err != nil {
		return fmt.Errorf("attest installed %s: %w", item.unit.Name, err)
	}
	if !installed.exists || !bytes.Equal(installed.contents, item.unit.Contents) {
		return fmt.Errorf("attest installed %s: contents differ from the compiled unit", item.unit.Name)
	}
	if err := m.validateUnitMetadata(installed); err != nil {
		return fmt.Errorf("attest installed %s: %w", item.unit.Name, err)
	}
	return nil
}

func (m Manager) runAdmin(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	return m.runAdminInput(ctx, nil, stdout, stderr, name, args...)
}

func (m Manager) runAdminInput(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	if m.UseSudo {
		args = append([]string{"-n", "--", name}, args...)
		name = sudoPath
	}
	return m.Runner.Run(ctx, stdin, stdout, stderr, name, args...)
}

type observedFile struct {
	exists   bool
	contents []byte
	metadata unitFileMetadata
}

type unitFileMetadata struct {
	mode  os.FileMode
	owner unitFileOwner
}

func readUnitFile(path string) (observedFile, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return observedFile{}, nil
	}
	if err != nil {
		return observedFile{}, fmt.Errorf("inspect systemd unit %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxUnitBytes {
		return observedFile{}, fmt.Errorf("%w at %s: expected one bounded regular file", ErrForeignUnit, path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return observedFile{}, fmt.Errorf("%w at %s: regular-file metadata is unavailable", ErrForeignUnit, path)
	}
	nlink := uint64(stat.Nlink)
	if nlink != 1 {
		return observedFile{}, fmt.Errorf("%w at %s: expected one link, found %d", ErrForeignUnit, path, nlink)
	}
	metadata := unitFileMetadata{
		mode:  info.Mode(),
		owner: unitFileOwner{uid: stat.Uid, gid: stat.Gid},
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return observedFile{}, fmt.Errorf("read systemd unit %s: %w", path, err)
	}
	return observedFile{exists: true, contents: contents, metadata: metadata}, nil
}

func sameObservedFile(left, right observedFile) bool {
	return left.exists == right.exists && left.metadata == right.metadata && bytes.Equal(left.contents, right.contents)
}

func (m Manager) validateUnitMetadata(observed observedFile) error {
	if observed.metadata.mode != 0o644 {
		return fmt.Errorf("unit file mode is %s; want 0644 with no special bits", observed.metadata.mode)
	}
	if observed.metadata.owner != m.expectedOwner {
		return fmt.Errorf(
			"unit file owner is %d:%d; want %d:%d",
			observed.metadata.owner.uid,
			observed.metadata.owner.gid,
			m.expectedOwner.uid,
			m.expectedOwner.gid,
		)
	}
	return nil
}

func validateOwnedUnit(name string, contents []byte) error {
	first, rest, found := bytes.Cut(contents, []byte("\n"))
	if !found || string(first) != managedHeader {
		return fmt.Errorf("missing exact Dorf ownership header")
	}
	unitLine, rest, found := bytes.Cut(rest, []byte("\n"))
	if !found || string(unitLine) != "# dorf-unit="+name {
		return fmt.Errorf("unit identity does not match")
	}
	digestLine, body, found := bytes.Cut(rest, []byte("\n"))
	if !found || !bytes.HasPrefix(digestLine, []byte("# dorf-sha256=")) {
		return fmt.Errorf("ownership digest is missing")
	}
	want := strings.TrimPrefix(string(digestLine), "# dorf-sha256=")
	decoded, err := hex.DecodeString(want)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("ownership digest is invalid")
	}
	got := sha256.Sum256(body)
	if !bytes.Equal(decoded, got[:]) {
		return fmt.Errorf("owned unit contents were modified")
	}
	return nil
}

type unitProperties struct {
	LoadState        string
	NeedDaemonReload string
	UnitFileState    string
	ActiveState      string
	SubState         string
	Type             string
	NotifyAccess     string
	FragmentPath     string
	Result           string
	ExecMainCode     string
	ExecMainStatus   string
}

// planProperties is the complete systemd observation that can change Plan's
// daemon-reload or enablement decisions. Runtime and notification state is
// intentionally excluded because reconciliation owns one ordered restart.
type planProperties struct {
	LoadState        string
	NeedDaemonReload string
	UnitFileState    string
	FragmentPath     string
}

func (properties unitProperties) planProperties() planProperties {
	return planProperties{
		LoadState:        properties.LoadState,
		NeedDaemonReload: properties.NeedDaemonReload,
		UnitFileState:    properties.UnitFileState,
		FragmentPath:     properties.FragmentPath,
	}
}

var showProperties = []string{
	"LoadState", "NeedDaemonReload", "UnitFileState", "ActiveState", "SubState", "Type", "NotifyAccess",
	"FragmentPath", "Result", "ExecMainCode", "ExecMainStatus",
}

func (m Manager) showUnit(ctx context.Context, name string) (unitProperties, error) {
	argument := "--property=" + strings.Join(showProperties, ",")
	output, err := m.Runner.Output(ctx, systemctlPath, "show", "--no-pager", argument, "--", name)
	if err != nil {
		return unitProperties{}, err
	}
	values := make(map[string]string, len(showProperties))
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !slices.Contains(showProperties, key) {
			return unitProperties{}, fmt.Errorf("systemctl returned an unexpected property %q", line)
		}
		if _, duplicate := values[key]; duplicate {
			return unitProperties{}, fmt.Errorf("systemctl returned duplicate property %s", key)
		}
		values[key] = value
	}
	if values["LoadState"] == "" {
		return unitProperties{}, fmt.Errorf("systemctl did not report LoadState")
	}
	return unitProperties{
		LoadState: values["LoadState"], NeedDaemonReload: values["NeedDaemonReload"], UnitFileState: values["UnitFileState"], ActiveState: values["ActiveState"],
		SubState: values["SubState"], Type: values["Type"], NotifyAccess: values["NotifyAccess"], FragmentPath: values["FragmentPath"],
		Result: values["Result"], ExecMainCode: values["ExecMainCode"], ExecMainStatus: values["ExecMainStatus"],
	}, nil
}

func validateLoadedAuthority(path string, fileExists bool, properties unitProperties) error {
	if properties.LoadState == "not-found" {
		if properties.FragmentPath != "" {
			return fmt.Errorf("not-found unit reported fragment %s", properties.FragmentPath)
		}
		return nil
	}
	if properties.LoadState != "loaded" {
		return fmt.Errorf("LoadState is %s", properties.LoadState)
	}
	if !fileExists {
		return fmt.Errorf("systemd already loaded the name from %s", properties.FragmentPath)
	}
	if filepath.Clean(properties.FragmentPath) != path || properties.FragmentPath != path {
		return fmt.Errorf("systemd loaded fragment %s instead of %s", properties.FragmentPath, path)
	}
	return nil
}

type Target string

const (
	TargetAPI    Target = "api"
	TargetWorker Target = "worker"
	TargetAll    Target = "all"
)

func targetUnits(target Target, allowAll bool) ([]string, error) {
	switch target {
	case TargetAPI:
		return []string{ControlAPIUnit}, nil
	case TargetWorker:
		return []string{WorkerUnit}, nil
	case TargetAll:
		if allowAll {
			return []string{WorkerUnit, ControlAPIUnit}, nil
		}
	}
	expected := "api or worker"
	if allowAll {
		expected += ", or all"
	}
	return nil, fmt.Errorf("managed-service target must be %s", expected)
}

func (m Manager) Restart(ctx context.Context, target Target, stdout, stderr io.Writer) error {
	m, err := m.configured()
	if err != nil {
		return err
	}
	units, err := targetUnits(target, true)
	if err != nil {
		return err
	}
	if err := m.requireOwnedTargets(ctx, units); err != nil {
		return err
	}
	for _, unit := range units {
		if err := m.runAdmin(ctx, stdout, stderr, systemctlPath, "restart", unit); err != nil {
			return fmt.Errorf("restart %s: %w", unit, err)
		}
	}
	return nil
}

func (m Manager) Logs(ctx context.Context, target Target, lines int, stdout, stderr io.Writer) error {
	m, err := m.configured()
	if err != nil {
		return err
	}
	units, err := targetUnits(target, false)
	if err != nil {
		return err
	}
	if lines < 1 || lines > 10_000 {
		return fmt.Errorf("managed-service log lines must be between 1 and 10000")
	}
	if err := m.requireOwnedTargets(ctx, units); err != nil {
		return err
	}
	return m.runAdmin(ctx, stdout, stderr, journalctlPath, "--no-pager", "--unit="+units[0], "--lines="+strconv.Itoa(lines))
}

func (m Manager) requireOwnedTargets(ctx context.Context, units []string) error {
	for _, unit := range units {
		path := filepath.Join(m.unitDir, unit)
		observed, err := readUnitFile(path)
		if err != nil {
			return err
		}
		if !observed.exists {
			return fmt.Errorf("%w %s: unit file is absent", ErrForeignUnit, unit)
		}
		if err := validateOwnedUnit(unit, observed.contents); err != nil {
			return fmt.Errorf("%w %s: %v", ErrForeignUnit, unit, err)
		}
		if err := m.validateUnitMetadata(observed); err != nil {
			return fmt.Errorf("%w %s: %v", ErrForeignUnit, unit, err)
		}
		properties, err := m.showUnit(ctx, unit)
		if err != nil {
			return err
		}
		if err := validateLoadedAuthority(path, true, properties); err != nil {
			return fmt.Errorf("%w %s: %v", ErrForeignUnit, unit, err)
		}
	}
	return nil
}

type ServiceStatus struct {
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

type Check struct {
	Ready  bool   `json:"ready"`
	Detail string `json:"detail"`
}

type APIStatus struct {
	URL            string `json:"url"`
	Version        string `json:"version,omitempty"`
	Discovery      Check  `json:"discovery"`
	Authentication Check  `json:"authentication"`
}

type Status struct {
	ControlAPI ServiceStatus `json:"control_api"`
	Worker     ServiceStatus `json:"worker"`
	API        APIStatus     `json:"api"`
	Converged  bool          `json:"converged"`
	Ready      bool          `json:"ready"`
}

// Status separates runtime readiness from desired unit convergence, then
// independently proves the loopback discovery and authentication boundaries.
// The compiled Type=notify unit reaches active/running only after READY=1.
func (m Manager) Status(ctx context.Context, spec Spec) (Status, error) {
	m, err := m.configured()
	if err != nil {
		return Status{}, err
	}
	units, err := RenderUnits(spec)
	if err != nil {
		return Status{}, err
	}
	statusCtx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	statuses := make(map[string]ServiceStatus, len(units))
	for _, unit := range units {
		statuses[unit.Name] = m.serviceStatus(statusCtx, unit)
	}
	api := APIStatus{URL: ControlURL}
	if statuses[ControlAPIUnit].Ready {
		api = m.apiStatus(statusCtx)
	} else {
		api.Discovery.Detail = "skipped: control API service is not runtime-ready"
		api.Authentication.Detail = "skipped: discovery is not ready"
	}
	status := Status{ControlAPI: statuses[ControlAPIUnit], Worker: statuses[WorkerUnit], API: api}
	status.Converged = status.ControlAPI.Converged && status.Worker.Converged
	status.Ready = status.Converged && status.ControlAPI.Ready && status.Worker.Ready && api.Discovery.Ready && api.Authentication.Ready
	return status, nil
}

func (m Manager) serviceStatus(ctx context.Context, desired Unit) ServiceStatus {
	status := ServiceStatus{Name: desired.Name}
	path := filepath.Join(m.unitDir, desired.Name)
	observed, fileErr := readUnitFile(path)
	var metadataErr error
	if fileErr == nil && observed.exists {
		status.Owned = validateOwnedUnit(desired.Name, observed.contents) == nil
		status.Current = bytes.Equal(observed.contents, desired.Contents)
		metadataErr = m.validateUnitMetadata(observed)
	}
	properties, showErr := m.showUnit(ctx, desired.Name)
	status.LoadState, status.UnitFileState = properties.LoadState, properties.UnitFileState
	status.ActiveState, status.SubState = properties.ActiveState, properties.SubState
	status.Result, status.ExecMainCode, status.ExecMainStatus = properties.Result, properties.ExecMainCode, properties.ExecMainStatus
	authorityErr := validateLoadedAuthority(path, observed.exists, properties)
	loadedContract := properties.Type == "notify" && properties.NotifyAccess == "main"
	bound := fileErr == nil && showErr == nil && authorityErr == nil && status.Owned && properties.LoadState == "loaded" && loadedContract
	status.Converged = bound && status.Current && metadataErr == nil && properties.UnitFileState == "enabled" && properties.NeedDaemonReload == "no"
	status.Ready = bound && properties.ActiveState == "active" && properties.SubState == "running"
	details := []string{}
	if fileErr != nil {
		details = append(details, fileErr.Error())
	} else if !observed.exists {
		details = append(details, "unit file is absent")
	} else if !status.Owned {
		details = append(details, "unit file is foreign or modified")
	}
	if showErr != nil {
		details = append(details, showErr.Error())
	} else if authorityErr != nil {
		details = append(details, authorityErr.Error())
	} else if status.Owned {
		details = append(details, "systemd state "+statePair(properties.ActiveState, properties.SubState))
		if metadataErr != nil {
			details = append(details, metadataErr.Error())
		}
		if !loadedContract {
			details = append(details, "loaded service contract is not notify/main")
		}
		if !status.Current {
			details = append(details, "compiled unit differs")
		}
		if properties.UnitFileState != "enabled" {
			details = append(details, "unit-file state "+emptyState(properties.UnitFileState))
		}
		if properties.NeedDaemonReload != "no" {
			details = append(details, "systemd daemon reload is required")
		}
		if properties.Result != "" && properties.Result != "success" {
			details = append(details, "result "+properties.Result)
		}
		if properties.ExecMainCode != "" || properties.ExecMainStatus != "" {
			details = append(details, "exec-main "+statePair(properties.ExecMainCode, properties.ExecMainStatus))
		}
	}
	status.Detail = strings.Join(details, "; ")
	return status
}

func statePair(first, second string) string {
	return emptyState(first) + "/" + emptyState(second)
}

func emptyState(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func (m Manager) apiStatus(ctx context.Context) APIStatus {
	status := APIStatus{URL: ControlURL, Authentication: Check{Detail: "skipped: discovery is not ready"}}
	discoveryRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet, ControlURL+"/v1", nil)
	discoveryResponse, err := m.HTTPClient.Do(discoveryRequest)
	if err != nil {
		status.Discovery = Check{Detail: "discovery request failed: " + err.Error()}
	} else {
		var value struct {
			Product string `json:"product"`
			Version string `json:"version"`
		}
		err = decodeProbeResponse(discoveryResponse, http.StatusOK, "application/json", &value)
		if err == nil && (value.Product != "dorf" || strings.TrimSpace(value.Version) == "") {
			err = fmt.Errorf("discovery did not identify a Dorf version")
		}
		if err == nil && strings.TrimSpace(m.ExpectedVersion) != "" && value.Version != m.ExpectedVersion {
			err = fmt.Errorf("discovery version is %s; running CLI expects %s", value.Version, m.ExpectedVersion)
		}
		if err != nil {
			status.Discovery = Check{Detail: err.Error()}
			return status
		} else {
			status.Version = value.Version
			status.Discovery = Check{Ready: true, Detail: "dorf " + value.Version}
		}
	}

	authRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet, ControlURL+"/v1/me", nil)
	authRequest.Header.Set("Authorization", "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	authResponse, err := m.HTTPClient.Do(authRequest)
	if err != nil {
		status.Authentication = Check{Detail: "authentication probe failed: " + err.Error()}
		return status
	}
	var problem struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}
	err = decodeProbeResponse(authResponse, http.StatusUnauthorized, "application/problem+json", &problem)
	if err == nil && (problem.Status != http.StatusUnauthorized || problem.Code != "unauthenticated") {
		err = fmt.Errorf("authentication probe did not return typed unauthenticated Problem Details")
	}
	if err != nil {
		status.Authentication = Check{Detail: err.Error()}
	} else {
		status.Authentication = Check{Ready: true, Detail: "typed unauthenticated response"}
	}
	return status
}

func decodeProbeResponse(response *http.Response, wantStatus int, wantMediaType string, destination any) error {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProbeBody+1))
	if err != nil {
		return fmt.Errorf("read readiness response: %w", err)
	}
	if len(body) > maxProbeBody {
		return fmt.Errorf("readiness response exceeds %d bytes", maxProbeBody)
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("HTTP status is %d; want %d", response.StatusCode, wantStatus)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != wantMediaType {
		return fmt.Errorf("response media type is %q; want %s", mediaType, wantMediaType)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode readiness response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("readiness response contains trailing JSON")
	}
	return nil
}
