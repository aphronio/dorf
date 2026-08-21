package hostsetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"runtime"
	"strings"

	"github.com/aphronio/dorf/internal/doctor"
)

// ErrKVMUnavailable identifies a host that cannot run the local Incus profile.
// Callers may present provider-aware remediation without matching error text.
var ErrKVMUnavailable = errors.New("KVM hardware virtualization is unavailable")

// KVMDevicePresent is the cheap, read-only availability check used to avoid
// offering local Incus setup on hosts that cannot provide it. Permission and
// capacity remain part of the authoritative setup check after selection.
func KVMDevicePresent() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// HostPlan is the exact set of supported host changes observed as missing.
// Its summaries are both the user-facing approval text and the authority for
// ApplyHost, so presentation cannot drift from execution.
type HostPlan struct {
	username      string
	requireDocker bool
	requireIncus  bool
	packages      []string
	services      []string
	groups        []string
	summaries     []string
	needsRelogin  bool
}

func (p HostPlan) Empty() bool { return len(p.summaries) == 0 }

func (p HostPlan) Summaries() []string {
	return append([]string{}, p.summaries...)
}

// RuntimeNames reports the host runtimes this plan proves ready.
func (p HostPlan) RuntimeNames() []string {
	var names []string
	if p.requireDocker {
		names = append(names, "Docker")
	}
	if p.requireIncus {
		names = append(names, "Incus", "QEMU", "KVM")
	}
	return names
}

func (p HostPlan) Description() string {
	lines := make([]string, 0, len(p.summaries))
	for _, summary := range p.summaries {
		lines = append(lines, "  • "+summary)
	}
	return strings.Join(lines, "\n")
}

type hostObservation struct {
	username      string
	ubuntu2404    bool
	dockerCommand bool
	dockerService bool
	dockerGroup   bool
	dockerAccess  bool
	incusCommand  bool
	incusService  bool
	incusGroup    bool
	incusAccess   bool
	qemuCommand   bool
	kvmGroup      bool
	kvmAccess     bool
}

// ObserveHost checks the supported local runtime prerequisites without
// mutating them and returns only the changes that are actually needed.
func ObserveHost(ctx context.Context, requireDocker, requireIncus bool) (HostPlan, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return HostPlan{}, fmt.Errorf("automatic host setup supports only x86_64 Linux")
	}
	kvmAccess := false
	if requireIncus {
		kvm, kvmErr := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if kvmErr == nil {
			kvmAccess = kvm.Close() == nil
		} else if os.IsNotExist(kvmErr) {
			return HostPlan{}, fmt.Errorf("%w: %v", ErrKVMUnavailable, kvmErr)
		}
	}
	if err := doctor.HostCapacity(requireIncus); err != nil {
		return HostPlan{}, err
	}
	account, err := user.Current()
	if err != nil {
		return HostPlan{}, err
	}
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(account.Username) {
		return HostPlan{}, fmt.Errorf("current username is unsafe for host setup commands")
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return HostPlan{}, err
	}
	values := parseRelease(string(release))
	configuredGroups, _ := exec.CommandContext(ctx, "id", "-nG", account.Username).Output()
	_, dockerErr := exec.LookPath("docker")
	_, incusErr := exec.LookPath("incus")
	_, qemuErr := exec.LookPath("qemu-system-x86_64")
	observation := hostObservation{
		username:      account.Username,
		ubuntu2404:    values["ID"] == "ubuntu" && values["VERSION_ID"] == "24.04",
		dockerCommand: dockerErr == nil,
		dockerService: commandSucceeds(ctx, []string{"systemctl", "is-active", "--quiet", "docker.service"}),
		dockerGroup:   containsField(string(configuredGroups), "docker") || account.Username == "root",
		dockerAccess:  commandSucceeds(ctx, []string{"docker", "info", "--format", "{{.ServerVersion}}"}),
		incusCommand:  incusErr == nil,
		incusService:  commandSucceeds(ctx, []string{"systemctl", "is-active", "--quiet", "incus.service"}),
		incusGroup:    containsField(string(configuredGroups), "incus-admin") || account.Username == "root",
		incusAccess:   commandSucceeds(ctx, []string{"incus", "info"}),
		qemuCommand:   qemuErr == nil,
		kvmGroup:      containsField(string(configuredGroups), "kvm") || account.Username == "root",
		kvmAccess:     kvmAccess,
	}
	return deriveHostPlan(observation, requireDocker, requireIncus)
}

func deriveHostPlan(observation hostObservation, requireDocker, requireIncus bool) (HostPlan, error) {
	plan := HostPlan{username: observation.username, requireDocker: requireDocker, requireIncus: requireIncus}
	if requireDocker && !observation.dockerAccess && observation.dockerCommand && observation.dockerService && observation.dockerGroup {
		return HostPlan{}, fmt.Errorf("Docker is installed but inaccessible; if group membership just changed, sign out and back in, then rerun dorf setup")
	}
	if requireIncus && !observation.incusAccess && observation.incusCommand && observation.incusService && observation.incusGroup {
		return HostPlan{}, fmt.Errorf("Incus is installed but inaccessible; if group membership just changed, sign out and back in, then rerun dorf setup")
	}
	addPackage := func(packageName, summary string) {
		plan.packages = append(plan.packages, packageName)
		plan.summaries = append(plan.summaries, summary)
	}
	addService := func(service, summary string) {
		plan.services = append(plan.services, service)
		plan.summaries = append(plan.summaries, summary)
	}
	addGroup := func(group, summary string) {
		plan.groups = append(plan.groups, group)
		plan.summaries = append(plan.summaries, summary)
		plan.needsRelogin = true
	}
	if requireDocker && !observation.dockerAccess {
		if !observation.dockerCommand {
			addPackage("docker.io", "Install Docker Engine")
		}
		if !observation.dockerService {
			addService("docker.service", "Enable and start Docker")
		}
		if !observation.dockerGroup && observation.username != "root" {
			addGroup("docker", "Grant "+observation.username+" root-equivalent Docker access")
		}
	}
	if requireIncus && !observation.incusAccess {
		if !observation.incusCommand {
			addPackage("incus", "Install Incus")
		}
		if !observation.incusService {
			addService("incus.service", "Enable and start Incus")
		}
		if !observation.incusGroup && observation.username != "root" {
			addGroup("incus-admin", "Grant "+observation.username+" root-equivalent Incus access")
		}
	}
	if requireIncus && !observation.qemuCommand {
		addPackage("qemu-system-x86", "Install QEMU")
	}
	if requireIncus && !observation.kvmAccess {
		if observation.kvmGroup {
			return HostPlan{}, fmt.Errorf("hardware virtualization is present but inaccessible; if kvm group membership just changed, sign out and back in, then rerun dorf setup")
		}
		if observation.username != "root" {
			addGroup("kvm", "Grant "+observation.username+" access to hardware virtualization")
		}
	}
	if !plan.Empty() && !observation.ubuntu2404 {
		return HostPlan{}, fmt.Errorf("this host is missing %s; automatic host changes support Ubuntu 24.04 only", strings.Join(plan.summaries, ", "))
	}
	return plan, nil
}

// ApplyHost executes exactly the approved plan, then validates only the
// selected host capabilities. Incus initialization is never a common setup
// side effect.
func ApplyHost(ctx context.Context, plan HostPlan, stdout, stderr io.Writer) error {
	prefix := []string{}
	if !plan.Empty() && os.Geteuid() != 0 {
		prefix = []string{"sudo"}
		if !commandSucceeds(ctx, []string{"sudo", "-n", "true"}) {
			if err := attached(ctx, stdout, stderr, "sudo", "-v"); err != nil {
				return fmt.Errorf("administrator authentication: %w", err)
			}
		}
	}
	if len(plan.packages) > 0 {
		if err := attachedCommand(ctx, stdout, stderr, appendArgs(prefix, "apt-get", "update")); err != nil {
			return err
		}
		if err := attachedCommand(ctx, stdout, stderr, appendArgs(prefix, append([]string{"apt-get", "install", "--yes"}, plan.packages...)...)); err != nil {
			return err
		}
	}
	for _, service := range plan.services {
		if err := attachedCommand(ctx, stdout, stderr, appendArgs(prefix, "systemctl", "enable", "--now", service)); err != nil {
			return err
		}
	}
	for _, group := range plan.groups {
		if err := attachedCommand(ctx, stdout, stderr, appendArgs(prefix, "usermod", "-aG", group, plan.username)); err != nil {
			return err
		}
	}
	if plan.needsRelogin {
		return fmt.Errorf("host changes applied; sign out and back in so the new group access takes effect, then run dorf setup again")
	}
	if contains(plan.packages, "docker.io") || contains(plan.services, "docker.service") || contains(plan.groups, "docker") {
		if !commandSucceeds(ctx, []string{"docker", "info", "--format", "{{.ServerVersion}}"}) {
			return fmt.Errorf("Docker is not accessible to the current user")
		}
	}
	if plan.requireIncus {
		if !commandSucceeds(ctx, []string{"incus", "info"}) {
			return fmt.Errorf("Incus is not accessible to the current user")
		}
		if err := initializePristineIncus(ctx, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

func initializePristineIncus(ctx context.Context, stdout, stderr io.Writer) error {
	storage, err := jsonList(ctx, "incus", "storage", "list", "--format=json")
	if err != nil {
		return err
	}
	networks, err := jsonList(ctx, "incus", "network", "list", "--format=json")
	if err != nil {
		return err
	}
	managedNetworks := 0
	for _, value := range networks {
		record, ok := value.(map[string]any)
		if !ok {
			continue
		}
		managed, _ := record["managed"].(bool)
		if managed {
			managedNetworks++
		}
	}
	if len(storage) == 0 && managedNetworks == 0 {
		if err := attached(ctx, stdout, stderr, "incus", "admin", "init", "--minimal"); err != nil {
			return err
		}
	} else if len(storage) == 0 || managedNetworks == 0 {
		return fmt.Errorf("Incus is partially initialized; preserve operator-owned resources and finish it explicitly")
	}
	remote, err := exec.CommandContext(ctx, "incus", "config", "get", "core.https_address").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(remote)) != "" {
		return fmt.Errorf("Incus remote API is enabled; Dorf supports only the local daemon")
	}
	return nil
}

func parseRelease(raw string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			result[key] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return result
}

func appendArgs(prefix []string, values ...string) []string {
	result := append([]string{}, prefix...)
	return append(result, values...)
}

func attachedCommand(ctx context.Context, stdout, stderr io.Writer, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty host command")
	}
	return attached(ctx, stdout, stderr, argv[0], argv[1:]...)
}

func attached(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func commandSucceeds(ctx context.Context, argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Run() == nil
}

func containsField(raw, wanted string) bool {
	for _, field := range strings.Fields(raw) {
		if field == wanted {
			return true
		}
	}
	return false
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func jsonList(ctx context.Context, name string, args ...string) ([]any, error) {
	raw, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil, err
	}
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}
