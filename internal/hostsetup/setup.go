package hostsetup

import (
	"context"
	"encoding/json"
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

// Ubuntu installs the one reviewed clean-host recipe. It deliberately has no
// generic package-manager or setup-workflow abstraction.
func Ubuntu(ctx context.Context, approved bool, stdout, stderr io.Writer) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("automatic host convergence supports only x86_64 Linux")
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("hardware virtualization is unavailable: %w", err)
	}
	if err := kvm.Close(); err != nil {
		return err
	}
	if err := doctor.HostCapacity(); err != nil {
		return err
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return err
	}
	values := parseRelease(string(release))
	if values["ID"] != "ubuntu" || values["VERSION_ID"] != "24.04" {
		return fmt.Errorf("automatic package convergence supports Ubuntu 24.04 only; install PostgreSQL and Incus through this host's reviewed native procedure")
	}
	account, err := user.Current()
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(account.Username) {
		return fmt.Errorf("current username is unsafe for native PostgreSQL/Incus commands")
	}
	_, incusPresent := exec.LookPath("incus")
	_, psqlPresent := exec.LookPath("psql")
	configuredGroups, _ := exec.CommandContext(ctx, "id", "-nG", account.Username).Output()
	groupReady := containsField(string(configuredGroups), "incus-admin")
	serviceReady := commandSucceeds(ctx, []string{"systemctl", "is-active", "--quiet", "incus.service"})
	databaseReady := commandSucceeds(ctx, []string{"psql", "-d", "dorf", "-Atc", "select 1"})
	if incusPresent == nil && psqlPresent == nil && groupReady && serviceReady && databaseReady {
		if !commandSucceeds(ctx, []string{"incus", "info"}) {
			return fmt.Errorf("host packages and group membership are configured; sign out and back in, then rerun the same command")
		}
		if err := initializePristineIncus(ctx, stdout, stderr); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Ubuntu host already ready: PostgreSQL database dorf and local Incus storage/network are available")
		return nil
	}
	fmt.Fprintln(stdout, "Dorf needs administrator permission to install PostgreSQL, Incus, and QEMU; enable only the local Incus service; and add the current user to the root-equivalent incus-admin group. It does not enable the Incus remote API.")
	if !approved {
		return fmt.Errorf("review the host changes, then rerun: dorf host install --yes")
	}
	prefix := []string{}
	if os.Geteuid() != 0 {
		prefix = []string{"sudo"}
		if err := attached(ctx, stdout, stderr, "sudo", "-v"); err != nil {
			return fmt.Errorf("administrator authentication: %w", err)
		}
	}
	if err := attachedCommand(ctx, stdout, stderr, appendArgs(prefix, "apt-get", "update")); err != nil {
		return err
	}
	if err := attachedCommand(ctx, stdout, stderr, appendArgs(prefix, "apt-get", "install", "--yes", "postgresql", "postgresql-client", "incus", "qemu-system")); err != nil {
		return err
	}
	if err := attachedCommand(ctx, stdout, stderr, appendArgs(prefix, "systemctl", "enable", "--now", "incus.service")); err != nil {
		return err
	}
	if err := attachedCommand(ctx, stdout, stderr, appendArgs(prefix, "usermod", "-aG", "incus-admin", account.Username)); err != nil {
		return err
	}
	postgresPrefix := appendArgs(prefix, "-u", "postgres")
	if len(prefix) == 0 {
		postgresPrefix = []string{"runuser", "-u", "postgres", "--"}
	} else {
		postgresPrefix = appendArgs(prefix, "-u", "postgres")
	}
	roleQuery := "select 1 from pg_roles where rolname='" + account.Username + "'"
	if !commandSucceeds(ctx, appendArgs(postgresPrefix, "psql", "-d", "postgres", "-Atc", roleQuery)) {
		if err := attachedCommand(ctx, stdout, stderr, appendArgs(postgresPrefix, "createuser", "--createdb", account.Username)); err != nil {
			return err
		}
	}
	if !commandSucceeds(ctx, []string{"psql", "-d", "dorf", "-Atc", "select 1"}) {
		if err := attached(ctx, stdout, stderr, "createdb", "dorf"); err != nil {
			return fmt.Errorf("create local Dorf database (a new login may be required first): %w", err)
		}
	}
	if !commandSucceeds(ctx, []string{"incus", "info"}) {
		return fmt.Errorf("packages and group membership are configured; sign out and back in, then rerun the same command to initialize Incus")
	}
	if err := initializePristineIncus(ctx, stdout, stderr); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Ubuntu host ready: PostgreSQL database dorf and local Incus storage/network are available")
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
