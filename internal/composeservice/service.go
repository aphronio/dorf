// Package composeservice owns Dorf's unprivileged Docker Compose lifecycle.
// It consumes one rendered project and leaves image acquisition to release.
package composeservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aphronio/dorf/internal/composeproject"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/dockerexec"
)

const (
	ControlAddress = "127.0.0.1:8745"
	ControlURL     = "http://" + ControlAddress

	databaseVolume       = "dorf-postgres-data"
	legacyDatabase       = "dorf-postgres"
	ownerLabel           = "dev.dorf.owner"
	projectName          = "dorf"
	probeBodyLimit       = 64 << 10
	dockerProbeTimeout   = 30 * time.Second
	databasePullTimeout  = 10 * time.Minute
	actionTimeout        = 3 * time.Minute
	migrationTimeout     = 5 * time.Minute
	dockerWaitDelay      = 250 * time.Millisecond
	maximumDetail        = 4096
	controlAuthChallenge = `Bearer realm="dorf"`
)

var exactImageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var exactConfigHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

var fixedComposeServices = []string{
	"postgres",
	"migrate",
	"worker",
	"control-reader",
	"control-api",
	"provider-gateway",
	"cloudflared",
}

// Dependents come first so reconciliation removes the ingress consumer before
// the provider gateway it addresses.
var optionalComposeServices = []string{"cloudflared", "provider-gateway"}

// Spec is the complete input for one rendered Compose project.
type Spec struct {
	ProjectDir string
	Project    composeproject.Project
}

func (spec Spec) Validate() error {
	runtime := spec.Project.Runtime
	for label, path := range map[string]string{"project": spec.ProjectDir, "configuration": runtime.ConfigDir, "data": runtime.DataDir, "state": runtime.StateDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return fmt.Errorf("Compose %s directory must be one clean absolute path", label)
		}
	}
	if runtime.UID <= 0 || runtime.GID < 0 || runtime.ProjectVersion != composeproject.ProjectVersion {
		return fmt.Errorf("Compose renderer identity is incomplete or unsupported")
	}
	if runtime.DeploymentPath != filepath.Join(runtime.ConfigDir, "deployment.json") {
		return fmt.Errorf("deployment path differs from the renderer-owned configuration directory")
	}
	if err := spec.Project.Image.Validate(); err != nil {
		return err
	}
	if err := runtime.Deployment.Database.Validate(); err != nil {
		return err
	}
	if !exactImageID.MatchString(runtime.Deployment.Database.ImageID) {
		return fmt.Errorf("PostgreSQL image identity must be one exact sha256 image ID")
	}
	for label, path := range map[string]string{"configuration": runtime.ConfigDir, "data": runtime.DataDir, "state": runtime.StateDir} {
		if pathsOverlap(spec.ProjectDir, path) {
			return fmt.Errorf("Compose project directory must be disjoint from the Dorf %s directory", label)
		}
	}
	for _, name := range []string{composeproject.ComposeFile, composeproject.EnvironmentFile, composeproject.ImageFile, composeproject.ControlDeploymentFile} {
		if _, ok := spec.Project.Files[name]; !ok {
			return fmt.Errorf("generated Compose project is missing %s", name)
		}
	}
	return nil
}

func pathsOverlap(first, second string) bool {
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return contains(first, second) || contains(second, first)
}

func projectCurrent(spec Spec) (bool, error) {
	if err := spec.Validate(); err != nil {
		return false, err
	}
	runtime := spec.Project.Runtime
	for _, path := range []string{runtime.ConfigDir, runtime.DataDir, runtime.StateDir, spec.ProjectDir} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect %s: %w", path, err)
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || int(owner.Uid) != runtime.UID || int(owner.Gid) != runtime.GID {
			return false, fmt.Errorf("%s must be one operator-owned directory", path)
		}
		if info.Mode().Perm() != 0o700 {
			return false, nil
		}
	}
	for _, path := range projectArtifactPaths(spec.Project) {
		target, want := filepath.Join(spec.ProjectDir, path), spec.Project.Files[path]
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect Compose artifact %s: %w", target, err)
		}
		owner, owned := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !owned || int(owner.Uid) != runtime.UID || int(owner.Gid) != runtime.GID {
			return false, fmt.Errorf("Compose artifact %s is not one regular file", target)
		}
		contents, err := os.ReadFile(target)
		if err != nil {
			return false, fmt.Errorf("read Compose artifact %s: %w", target, err)
		}
		if info.Mode().Perm() != want.Mode.Perm() || !bytes.Equal(contents, want.Contents) {
			return false, nil
		}
	}
	for path, entries := range map[string][]string{
		"control-config":      {"dorf"},
		"control-config/dorf": {"deployment.json"},
	} {
		current, err := exactGeneratedDirectory(filepath.Join(spec.ProjectDir, filepath.FromSlash(path)), runtime.UID, runtime.GID, entries)
		if err != nil || !current {
			return current, err
		}
	}
	return true, nil
}

func exactGeneratedDirectory(path string, uid, gid int, expected []string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect generated Compose directory %s: %w", path, err)
	}
	owner, owned := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !owned || int(owner.Uid) != uid || int(owner.Gid) != gid {
		return false, fmt.Errorf("generated Compose directory %s is not operator-owned", path)
	}
	if info.Mode().Perm() != 0o700 {
		return false, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("inspect generated Compose directory %s: %w", path, err)
	}
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		actual = append(actual, entry.Name())
	}
	sort.Strings(actual)
	want := append([]string(nil), expected...)
	sort.Strings(want)
	if !reflect.DeepEqual(actual, want) {
		return false, nil
	}
	return true, nil
}

func projectArtifactPaths(project composeproject.Project) []string {
	paths := make([]string, 0, len(project.Files))
	for path := range project.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func desiredCurrent(spec Spec) error {
	current, err := projectCurrent(spec)
	if err != nil {
		return err
	}
	stored, found, err := deployment.Load(spec.Project.Runtime.DeploymentPath)
	if err != nil || !found || !sameDeployment(stored, spec.Project.Runtime.Deployment) {
		return fmt.Errorf("deployment drifted; reconcile services first")
	}
	if !current {
		return fmt.Errorf("Compose project drifted; reconcile services first")
	}
	return nil
}

type CommandRunner interface {
	Run(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error
	Output(context.Context, string, ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	command, err := composeCommand(ctx, name, args...)
	if err != nil {
		return err
	}
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	return command.Run()
}

func (ExecRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	command, err := composeCommand(ctx, name, args...)
	if err != nil {
		return "", err
	}
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), nil
	}
	detail := strings.TrimSpace(string(output))
	if len(detail) > maximumDetail {
		detail = detail[:maximumDetail] + "…"
	}
	if detail == "" {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return "", fmt.Errorf("%s: %w: %s", name, err, detail)
}

func composeCommand(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	return composeCommandWithResolver(ctx, dockerexec.Resolve, name, args...)
}

func composeCommandWithResolver(ctx context.Context, resolve func() (string, error), name string, args ...string) (*exec.Cmd, error) {
	if name != "docker" {
		return nil, fmt.Errorf("Compose runner only executes docker")
	}
	environment, err := sanitizedCommandEnvironment(os.Environ())
	if err != nil {
		return nil, err
	}
	executable, err := resolve()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = environment
	command.WaitDelay = dockerWaitDelay
	return command, nil
}

func sanitizedCommandEnvironment(environment []string) ([]string, error) {
	values := make(map[string]string)
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}
	result := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	if configured := values["DOCKER_HOST"]; configured != "" {
		host := strings.TrimSpace(configured)
		endpoint, err := url.Parse(host)
		if host != configured || err != nil || endpoint.Scheme != "unix" || endpoint.Host != "" || endpoint.User != nil || endpoint.Opaque != "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || !filepath.IsAbs(endpoint.Path) || endpoint.Path == "/" || filepath.Clean(endpoint.Path) != endpoint.Path || host != "unix://"+endpoint.Path {
			return nil, fmt.Errorf("Dorf Compose requires a local absolute unix:// Docker endpoint")
		}
		result = append(result, "DOCKER_HOST="+host)
	} else {
		result = append(result, "DOCKER_CONTEXT=default")
	}
	for _, name := range []string{"HOME", "XDG_RUNTIME_DIR"} {
		if value, found := values[name]; found {
			result = append(result, name+"="+value)
		}
	}
	return result, nil
}

type Manager struct {
	Runner     CommandRunner
	HTTPClient *http.Client
}

func (manager Manager) configured() Manager {
	if manager.Runner == nil {
		manager.Runner = ExecRunner{}
	}
	if manager.HTTPClient == nil {
		manager.HTTPClient = &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return manager
}

// CheckDocker is the single read-only readiness check used before setup or
// reconciliation spends time acquiring images.
func (manager Manager) CheckDocker(ctx context.Context) error {
	manager = manager.configured()
	probeCtx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	server, err := manager.Runner.Output(probeCtx, "docker", "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		return fmt.Errorf("Docker Engine is unavailable: %w", err)
	}
	if strings.TrimSpace(server) == "" {
		return fmt.Errorf("Docker Engine readiness returned no server version")
	}
	compose, err := manager.Runner.Output(probeCtx, "docker", "compose", "version", "--short")
	if err != nil {
		return fmt.Errorf("Docker Compose is unavailable: %w", err)
	}
	if strings.TrimSpace(compose) == "" {
		return fmt.Errorf("Docker Compose readiness returned no version")
	}
	return nil
}

// Apply performs all fallible preparation, including exact image attestation,
// before stopping an old runtime. The caller owns image acquisition and the
// single approval that precedes it.
func (manager Manager) Apply(ctx context.Context, spec Spec, stdout, stderr io.Writer) error {
	manager = manager.configured()
	if err := spec.Validate(); err != nil {
		return err
	}
	runtime, database := spec.Project.Runtime, spec.Project.Runtime.Deployment.Database
	if runtime.UID != os.Geteuid() || runtime.GID != os.Getegid() {
		return fmt.Errorf("Compose operator %d:%d differs from current process %d:%d", runtime.UID, runtime.GID, os.Geteuid(), os.Getegid())
	}
	probeCtx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	err := manager.preflightExistingTargets(probeCtx)
	cancel()
	if err != nil {
		return err
	}
	for _, path := range []string{runtime.ConfigDir, runtime.DataDir, runtime.StateDir, spec.ProjectDir} {
		if err := ensureDirectory(path, runtime.UID, runtime.GID); err != nil {
			return err
		}
	}
	lock, err := acquireLock(ctx, spec.ProjectDir)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	stored, found, err := deployment.Load(runtime.DeploymentPath)
	if err != nil {
		return err
	}
	if !found || !sameDeployment(stored, runtime.Deployment) {
		return fmt.Errorf("Dorf deployment changed after rendering; rerun reconcile")
	}
	pullCtx, cancel := context.WithTimeout(ctx, databasePullTimeout)
	err = manager.ensureDatabaseImage(pullCtx, database, stdout, stderr)
	cancel()
	if err != nil {
		return err
	}
	probeCtx, cancel = context.WithTimeout(ctx, dockerProbeTimeout)
	legacy, err := manager.inspectLegacyDatabase(probeCtx, database)
	if err == nil {
		err = manager.ensureDatabaseVolume(probeCtx, database.VolumeState == deployment.DatabaseVolumePending && legacy == nil, stdout, stderr)
	}
	cancel()
	if err != nil {
		return err
	}
	if err := spec.Project.Materialize(spec.ProjectDir); err != nil {
		return err
	}
	probeCtx, cancel = context.WithTimeout(ctx, dockerProbeTimeout)
	err = manager.attestImage(probeCtx, spec)
	cancel()
	if err != nil {
		return err
	}
	actionCtx, cancel := context.WithTimeout(ctx, actionTimeout)
	err = manager.removeObsoleteOptionalServices(actionCtx, spec, stdout, stderr)
	cancel()
	if err != nil {
		return err
	}
	actionCtx, cancel = context.WithTimeout(ctx, actionTimeout)
	stopServices := append(desiredApplicationServices(spec), "postgres")
	err = manager.runCompose(actionCtx, spec.ProjectDir, stdout, stderr, append([]string{"stop"}, stopServices...)...)
	if err == nil && legacy != nil && legacy.Running {
		err = manager.Runner.Run(actionCtx, nil, stdout, stderr, "docker", "container", "stop", legacy.ID)
		legacy.Running = false
	}
	cancel()
	if err != nil {
		return fmt.Errorf("stop old Dorf runtime: %w", err)
	}
	probeCtx, cancel = context.WithTimeout(ctx, dockerProbeTimeout)
	err = manager.requireUnusedDatabaseVolume(probeCtx)
	cancel()
	if err != nil {
		return err
	}
	migrateCtx, cancel := context.WithTimeout(ctx, migrationTimeout)
	err = manager.runCompose(migrateCtx, spec.ProjectDir, stdout, stderr, "up", "--detach", "postgres", "migrate")
	if err == nil {
		err = manager.runCompose(migrateCtx, spec.ProjectDir, stdout, stderr, "wait", "migrate")
	}
	if err == nil {
		var raw string
		raw, err = manager.outputCompose(migrateCtx, spec.ProjectDir, "ps", "--all", "--format", "json", "migrate")
		if records, decodeErr := decodeComposePS(raw); err == nil && decodeErr == nil {
			migration, unique := uniqueRecord(records, "migrate")
			if !unique || migration.State != "exited" || migration.ExitCode != 0 {
				err = fmt.Errorf("migration exited unsuccessfully")
			}
		} else if err == nil {
			err = decodeErr
		}
	}
	cancel()
	if err != nil {
		return fmt.Errorf("run Dorf migration: %w", err)
	}
	if err := deployment.MarkDatabaseVolumeInitialized(runtime.DeploymentPath, database); err != nil {
		return fmt.Errorf("record initialized PostgreSQL volume: %w", err)
	}
	actionCtx, cancel = context.WithTimeout(ctx, actionTimeout)
	applicationServices := desiredApplicationServices(spec)
	err = manager.runCompose(actionCtx, spec.ProjectDir, stdout, stderr, append([]string{"up", "--detach", "--wait"}, applicationServices...)...)
	cancel()
	if err != nil {
		return fmt.Errorf("converge Dorf services: %w", err)
	}
	probeCtx, cancel = context.WithTimeout(ctx, dockerProbeTimeout)
	err = manager.attestRunning(probeCtx, spec, append([]string{"postgres"}, applicationServices...))
	cancel()
	if err != nil {
		return fmt.Errorf("attest converged Dorf services: %w", err)
	}
	if legacy != nil {
		actionCtx, cancel = context.WithTimeout(ctx, actionTimeout)
		err = manager.Runner.Run(actionCtx, nil, stdout, stderr, "docker", "container", "rm", legacy.ID)
		cancel()
		if err != nil {
			return fmt.Errorf("remove stopped legacy PostgreSQL container: %w", err)
		}
	}
	return nil
}

func ensureDirectory(path string, uid, gid int) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || int(owner.Uid) != uid || int(owner.Gid) != gid {
		return fmt.Errorf("%s must be one operator-owned directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect %s: %w", path, err)
	}
	return nil
}

func acquireLock(ctx context.Context, directory string) (*os.File, error) {
	file, err := os.Open(directory)
	if err != nil {
		return nil, fmt.Errorf("open Compose lifecycle lock: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !opened.IsDir() {
		file.Close()
		return nil, fmt.Errorf("inspect Compose lifecycle lock")
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			current, inspectErr := os.Lstat(directory)
			if inspectErr != nil || !current.IsDir() || !os.SameFile(opened, current) {
				releaseLock(file)
				return nil, fmt.Errorf("Compose project changed while its lifecycle was locked")
			}
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			file.Close()
			return nil, fmt.Errorf("lock Compose lifecycle: %w", err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func releaseLock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func (manager Manager) ensureDatabaseImage(ctx context.Context, database deployment.Database, stdout, stderr io.Writer) error {
	identity, err := manager.Runner.Output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", database.ImageID)
	if err == nil && strings.TrimSpace(identity) == database.ImageID {
		return nil
	}
	if err == nil || !dockerAbsent(err, "image") {
		return fmt.Errorf("inspect exact PostgreSQL image: %v", err)
	}
	if err := manager.Runner.Run(ctx, nil, stdout, stderr, "docker", "image", "pull", database.Image); err != nil {
		return fmt.Errorf("recover exact PostgreSQL image: %w", err)
	}
	identity, err = manager.Runner.Output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", database.Image)
	if err != nil || strings.TrimSpace(identity) != database.ImageID {
		return fmt.Errorf("reviewed PostgreSQL tag no longer resolves to %s", database.ImageID)
	}
	return nil
}

func (manager Manager) ensureDatabaseVolume(ctx context.Context, allowCreate bool, stdout, stderr io.Writer) error {
	owner, err := manager.Runner.Output(ctx, "docker", "volume", "inspect", "--format", `{{ index .Labels "dev.dorf.owner" }}`, databaseVolume)
	if err != nil && dockerAbsent(err, "volume") && allowCreate {
		err = manager.Runner.Run(ctx, nil, stdout, stderr, "docker", "volume", "create", "--label", ownerLabel+"=database", databaseVolume)
		if err == nil {
			owner = "database"
		}
	}
	if err != nil {
		return fmt.Errorf("inspect durable PostgreSQL volume: %w", err)
	}
	if strings.TrimSpace(owner) != "database" {
		return fmt.Errorf("Docker volume %s is not owned by Dorf", databaseVolume)
	}
	return nil
}

type legacyContainer struct {
	ID      string
	Running bool
}

func (manager Manager) inspectLegacyDatabase(ctx context.Context, database deployment.Database) (*legacyContainer, error) {
	raw, err := manager.Runner.Output(ctx, "docker", "container", "inspect", "--format", "{{json .}}", legacyDatabase)
	if err != nil {
		if dockerAbsent(err, "container") {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect legacy PostgreSQL container: %w", err)
	}
	var value struct {
		ID, Image string
		Config    struct {
			Labels map[string]string
			Env    []string
		}
		State      struct{ Running bool }
		HostConfig struct {
			PortBindings map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string
			}
		}
		Mounts []struct{ Name, Destination string }
	}
	if json.Unmarshal([]byte(raw), &value) != nil || value.ID == "" || value.Image != database.ImageID || value.Config.Labels[ownerLabel] != "database" {
		return nil, fmt.Errorf("legacy PostgreSQL container is not the exact Dorf database")
	}
	environment := map[string]string{}
	for _, entry := range value.Config.Env {
		name, content, found := strings.Cut(entry, "=")
		if found {
			environment[name] = content
		}
	}
	bindings := value.HostConfig.PortBindings["5432/tcp"]
	if environment["POSTGRES_USER"] != database.User || environment["POSTGRES_DB"] != database.Name || environment["POSTGRES_PASSWORD"] != database.Password || len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" || bindings[0].HostPort != strconv.Itoa(database.Port) || !hasDatabaseMount(value.Mounts) {
		return nil, fmt.Errorf("legacy PostgreSQL container differs from the recorded deployment")
	}
	return &legacyContainer{ID: value.ID, Running: value.State.Running}, nil
}

func hasDatabaseMount(mounts []struct{ Name, Destination string }) bool {
	count := 0
	for _, mount := range mounts {
		if mount.Name == databaseVolume && mount.Destination == "/var/lib/postgresql/data" {
			count++
		}
	}
	return count == 1
}

func (manager Manager) requireUnusedDatabaseVolume(ctx context.Context) error {
	raw, err := manager.Runner.Output(ctx, "docker", "container", "ls", "--no-trunc", "--filter", "volume="+databaseVolume, "--format", "{{.ID}}")
	if err != nil {
		return fmt.Errorf("enumerate PostgreSQL volume attachments: %w", err)
	}
	if ids := strings.Fields(raw); len(ids) != 0 {
		return fmt.Errorf("PostgreSQL volume is still used by running container %s", ids[0])
	}
	return nil
}

type containerInfo struct {
	ID, Name, Image string
	Labels          map[string]string
}

func (manager Manager) inspectContainerReference(ctx context.Context, reference string) (containerInfo, error) {
	raw, err := manager.Runner.Output(ctx, "docker", "container", "inspect", "--format", "{{json .}}", reference)
	if err != nil {
		return containerInfo{}, fmt.Errorf("inspect container %s: %w", reference, err)
	}
	var value struct {
		ID, Name, Image string
		Config          struct{ Labels map[string]string }
	}
	if json.Unmarshal([]byte(raw), &value) != nil || value.ID == "" || value.Name == "" || value.Config.Labels == nil {
		return containerInfo{}, fmt.Errorf("inspect container %s: mismatched Docker response", reference)
	}
	return containerInfo{ID: value.ID, Name: value.Name, Image: value.Image, Labels: value.Config.Labels}, nil
}

func (manager Manager) inspectContainer(ctx context.Context, id string) (containerInfo, error) {
	container, err := manager.inspectContainerReference(ctx, id)
	if err != nil {
		return containerInfo{}, err
	}
	if container.ID != id {
		return containerInfo{}, fmt.Errorf("inspect container %s: mismatched Docker response", id)
	}
	return container, nil
}

func (manager Manager) preflightExistingTargets(ctx context.Context) error {
	knownServices := make(map[string]struct{}, len(fixedComposeServices))
	for _, service := range fixedComposeServices {
		knownServices[service] = struct{}{}
	}
	raw, err := manager.Runner.Output(ctx, "docker", "container", "ls", "--all", "--no-trunc", "--filter", "label=com.docker.compose.project="+projectName, "--format", "{{.ID}}")
	if err != nil {
		return fmt.Errorf("inspect existing Dorf Compose project: %w", err)
	}
	seen := make(map[string]struct{})
	for _, id := range strings.Fields(raw) {
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("existing Dorf Compose project returned duplicate container %s", id)
		}
		seen[id] = struct{}{}
		container, err := manager.inspectContainer(ctx, id)
		if err != nil {
			return err
		}
		service := container.Labels["com.docker.compose.service"]
		if _, known := knownServices[service]; !known {
			return fmt.Errorf("existing Dorf Compose container %s is foreign or has inconsistent ownership", container.Name)
		}
		if err := validateExistingTarget(container, service); err != nil {
			return err
		}
	}
	for _, service := range fixedComposeServices {
		name := fixedContainerName(service)
		container, err := manager.inspectContainerReference(ctx, name)
		if err != nil {
			if dockerAbsent(err, "container") {
				continue
			}
			return err
		}
		if err := validateExistingTarget(container, service); err != nil {
			return err
		}
	}
	return nil
}

func fixedContainerName(service string) string {
	return projectName + "-" + service + "-1"
}

func validateExistingTarget(container containerInfo, service string) error {
	labels := container.Labels
	projectVersion, err := strconv.Atoi(labels["dev.dorf.project-version"])
	validVersion := err == nil && projectVersion > 0 && projectVersion <= composeproject.ProjectVersion && labels["dev.dorf.project-version"] == strconv.Itoa(projectVersion)
	valid := container.Name == "/"+fixedContainerName(service) && exactImageID.MatchString(container.Image) &&
		labels[ownerLabel] == "deployment" && labels["com.docker.compose.project"] == projectName && labels["com.docker.compose.service"] == service &&
		labels["com.docker.compose.container-number"] == "1" && labels["com.docker.compose.oneoff"] == "False" &&
		exactConfigHash.MatchString(labels["com.docker.compose.config-hash"]) && validVersion && strings.TrimSpace(labels["dev.dorf.release"]) != ""
	if !valid {
		return fmt.Errorf("existing Compose target %s is foreign or has inconsistent ownership", fixedContainerName(service))
	}
	return nil
}

func desiredOptionalService(spec Spec, service string) bool {
	switch service {
	case "provider-gateway":
		return spec.Project.Runtime.Gateway != nil
	case "cloudflared":
		return spec.Project.Runtime.Cloudflare != nil
	default:
		return false
	}
}

// obsoleteOptionalContainers resolves only fixed Dorf-owned names, validates
// every ownership label, and returns immutable IDs. Callers can therefore
// observe or remove an old optional service without granting Compose a broad
// orphan-cleanup target.
func (manager Manager) obsoleteOptionalContainers(ctx context.Context, spec Spec) ([]containerInfo, error) {
	var obsolete []containerInfo
	seen := make(map[string]struct{}, len(optionalComposeServices))
	for _, service := range optionalComposeServices {
		if desiredOptionalService(spec, service) {
			continue
		}
		container, err := manager.inspectContainerReference(ctx, fixedContainerName(service))
		if err != nil {
			if dockerAbsent(err, "container") {
				continue
			}
			return nil, err
		}
		if err := validateExistingTarget(container, service); err != nil {
			return nil, err
		}
		if _, duplicate := seen[container.ID]; duplicate {
			return nil, fmt.Errorf("obsolete Compose services resolved to duplicate container %s", container.ID)
		}
		seen[container.ID] = struct{}{}
		obsolete = append(obsolete, container)
	}
	return obsolete, nil
}

func (manager Manager) removeObsoleteOptionalServices(ctx context.Context, spec Spec, stdout, stderr io.Writer) error {
	obsolete, err := manager.obsoleteOptionalContainers(ctx, spec)
	if err != nil {
		return fmt.Errorf("inspect obsolete Compose services: %w", err)
	}
	if len(obsolete) == 0 {
		return nil
	}
	args := []string{"container", "rm", "--force"}
	for _, container := range obsolete {
		args = append(args, container.ID)
	}
	if err := manager.Runner.Run(ctx, nil, stdout, stderr, "docker", args...); err != nil {
		return fmt.Errorf("remove obsolete Compose services: %w", err)
	}
	return nil
}

func dockerAbsent(err error, kind string) bool {
	detail := strings.ToLower(err.Error())
	return strings.Contains(detail, "no such "+kind) || strings.Contains(detail, kind+" not found")
}

func sameDeployment(first, second deployment.Config) bool {
	first.Database.VolumeState = ""
	second.Database.VolumeState = ""
	return reflect.DeepEqual(first, second)
}

type dockerImage struct {
	ID           string `json:"Id"`
	OS           string `json:"Os"`
	Architecture string
	Config       struct{ Labels map[string]string }
}

func (manager Manager) attestImage(ctx context.Context, spec Spec) error {
	raw, err := manager.Runner.Output(ctx, "docker", "image", "inspect", "--format", "{{json .}}", spec.Project.Image.ImageID)
	if err != nil {
		return fmt.Errorf("inspect exact Dorf image: %w", err)
	}
	var image dockerImage
	if json.Unmarshal([]byte(raw), &image) != nil || image.ID != spec.Project.Image.ImageID || image.OS != "linux" || image.Architecture != "amd64" || image.Config.Labels["org.opencontainers.image.version"] != spec.Project.Image.Version || image.Config.Labels["dev.dorf.binary-sha256"] != spec.Project.Image.BinarySHA256 {
		return fmt.Errorf("exact Dorf image differs from its acquired authority")
	}
	return nil
}

type Target string

const (
	TargetAPI        Target = "api"
	TargetWorker     Target = "worker"
	TargetGateway    Target = "gateway"
	TargetCloudflare Target = "cloudflare"
	TargetAll        Target = "all"
)

func desiredApplicationServices(spec Spec) []string {
	services := make([]string, 0, 5)
	if spec.Project.Runtime.Gateway != nil {
		services = append(services, "provider-gateway")
	}
	if spec.Project.Runtime.Cloudflare != nil {
		services = append(services, "cloudflared")
	}
	return append(services, "worker", "control-reader", "control-api")
}

func targetServices(spec Spec, target Target, all bool) ([]string, error) {
	switch target {
	case TargetAPI:
		return []string{"control-api"}, nil
	case TargetWorker:
		return []string{"worker"}, nil
	case TargetGateway:
		if spec.Project.Runtime.Gateway != nil {
			return []string{"provider-gateway"}, nil
		}
		return nil, fmt.Errorf("Provider Gateway Compose service is not configured")
	case TargetCloudflare:
		if spec.Project.Runtime.Cloudflare != nil {
			return []string{"cloudflared"}, nil
		}
		return nil, fmt.Errorf("Cloudflare Tunnel Compose service is not configured")
	case TargetAll:
		if all {
			return desiredApplicationServices(spec), nil
		}
	}
	return nil, fmt.Errorf("Compose service target is invalid")
}

func (manager Manager) Restart(ctx context.Context, spec Spec, target Target, stdout, stderr io.Writer) error {
	manager = manager.configured()
	services, err := targetServices(spec, target, true)
	if err != nil {
		return err
	}
	lock, err := acquireLock(ctx, spec.ProjectDir)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	actionCtx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()
	before, err := manager.attestTargets(actionCtx, spec, services)
	if err != nil {
		return err
	}
	if err := manager.runCompose(actionCtx, spec.ProjectDir, stdout, stderr, append([]string{"up", "--detach", "--force-recreate", "--no-deps", "--wait"}, services...)...); err != nil {
		return fmt.Errorf("force-recreate Dorf Compose services: %w", err)
	}
	after, err := manager.attestTargets(actionCtx, spec, services)
	if err != nil {
		return err
	}
	for index, service := range services {
		if before[index] == after[index] {
			return fmt.Errorf("Dorf Compose %s did not receive a fresh container identity", service)
		}
	}
	return nil
}

func (manager Manager) Logs(ctx context.Context, spec Spec, target Target, lines int, stdout, stderr io.Writer) error {
	manager = manager.configured()
	services, err := targetServices(spec, target, false)
	if err != nil {
		return err
	}
	if lines < 1 || lines > 10_000 {
		return fmt.Errorf("log lines must be between 1 and 10000")
	}
	lock, err := acquireLock(ctx, spec.ProjectDir)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	actionCtx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	ids, err := manager.attestTargets(actionCtx, spec, services)
	if err != nil {
		return err
	}
	return manager.Runner.Run(actionCtx, nil, stdout, stderr, "docker", "container", "logs", "--tail", strconv.Itoa(lines), ids[0])
}

func (manager Manager) attestTargets(ctx context.Context, spec Spec, services []string) ([]string, error) {
	if err := desiredCurrent(spec); err != nil {
		return nil, err
	}
	if err := manager.attestImage(ctx, spec); err != nil {
		return nil, err
	}
	statuses, err := manager.inspectServices(ctx, spec, services)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, service := range services {
		status := statuses[service]
		if !status.Current || status.containerID == "" {
			return nil, fmt.Errorf("Dorf Compose %s container is stale or foreign", service)
		}
		ids = append(ids, status.containerID)
	}
	return ids, nil
}

type ServiceStatus struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Health      string `json:"health,omitempty"`
	Current     bool   `json:"current"`
	Ready       bool   `json:"ready"`
	Detail      string `json:"detail"`
	containerID string
}

type APIStatus struct {
	URL     string `json:"url"`
	Version string `json:"version,omitempty"`
	Ready   bool   `json:"ready"`
	Detail  string `json:"detail"`
}

type Status struct {
	Postgres      ServiceStatus  `json:"postgres"`
	Worker        ServiceStatus  `json:"worker"`
	ControlReader ServiceStatus  `json:"control_reader"`
	ControlAPI    ServiceStatus  `json:"control_api"`
	Gateway       *ServiceStatus `json:"provider_gateway,omitempty"`
	Cloudflare    *ServiceStatus `json:"cloudflared,omitempty"`
	API           APIStatus      `json:"api"`
	Current       bool           `json:"current"`
	Converged     bool           `json:"converged"`
	Ready         bool           `json:"ready"`
}

func (manager Manager) Status(ctx context.Context, spec Spec) (Status, error) {
	manager = manager.configured()
	if err := desiredCurrent(spec); err != nil {
		return Status{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	if err := manager.attestImage(probeCtx, spec); err != nil {
		return Status{}, err
	}
	obsolete, err := manager.obsoleteOptionalContainers(probeCtx, spec)
	if err != nil {
		return Status{}, err
	}
	if len(obsolete) != 0 {
		services := make([]string, 0, len(obsolete))
		for _, container := range obsolete {
			services = append(services, container.Labels["com.docker.compose.service"])
		}
		return Status{}, fmt.Errorf("obsolete Compose service %s remains; reconcile services", strings.Join(services, ", "))
	}
	services := append([]string{"postgres"}, desiredApplicationServices(spec)...)
	statuses, err := manager.inspectServices(probeCtx, spec, services)
	if err != nil {
		return Status{}, err
	}
	status := Status{
		Postgres: statuses["postgres"], Worker: statuses["worker"],
		ControlReader: statuses["control-reader"], ControlAPI: statuses["control-api"], Current: true,
	}
	status.Converged = status.Current && status.Postgres.Current && status.Worker.Current && status.ControlReader.Current && status.ControlAPI.Current &&
		status.Postgres.Ready && status.Worker.Ready && status.ControlReader.Ready && status.ControlAPI.Ready
	if spec.Project.Runtime.Gateway != nil {
		gateway := statuses["provider-gateway"]
		status.Gateway = &gateway
		status.Converged = status.Converged && gateway.Current && gateway.Ready
	}
	if spec.Project.Runtime.Cloudflare != nil {
		cloudflare := statuses["cloudflared"]
		status.Cloudflare = &cloudflare
		status.Converged = status.Converged && cloudflare.Current && cloudflare.Ready
	}
	if status.ControlAPI.Ready {
		apiCtx, cancelAPI := context.WithTimeout(ctx, 10*time.Second)
		status.API = manager.apiStatus(apiCtx, spec.Project.Image.Version)
		cancelAPI()
	} else {
		status.API = APIStatus{URL: ControlURL, Detail: "skipped: control API is not running"}
	}
	status.Ready = status.Converged && status.API.Ready
	return status, nil
}

type composePS struct {
	ID       string `json:"ID"`
	Service  string
	State    string
	Health   string
	ExitCode int
}

func decodeComposePS(raw string) ([]composePS, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var records []composePS
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return nil, fmt.Errorf("decode Docker Compose status: %w", err)
	}
	return records, nil
}

func uniqueRecord(records []composePS, service string) (composePS, bool) {
	var result composePS
	found := false
	for _, record := range records {
		if record.Service != service {
			continue
		}
		if found {
			return composePS{}, false
		}
		result, found = record, true
	}
	return result, found
}

func serviceFromRecords(records []composePS, name string) ServiceStatus {
	status := ServiceStatus{Name: name, Detail: "service is absent"}
	record, found := uniqueRecord(records, name)
	if !found {
		return status
	}
	status.State, status.Health = record.State, record.Health
	healthRequired := name == "postgres" || name == "control-reader"
	status.Ready = status.State == "running" && (!healthRequired || status.Health == "healthy")
	if status.Ready {
		status.Detail = "ready"
	} else if healthRequired && status.State == "running" {
		status.Detail = "health " + status.Health
	} else {
		status.Detail = "state " + status.State
	}
	return status
}

func (manager Manager) attestService(ctx context.Context, spec Spec, name, id string) (bool, string, error) {
	container, err := manager.inspectContainer(ctx, id)
	if err != nil {
		return false, "", err
	}
	hashOutput, err := manager.outputCompose(ctx, spec.ProjectDir, "config", "--hash", name)
	if err != nil {
		return false, "", fmt.Errorf("derive %s Compose config hash: %w", name, err)
	}
	hashFields := strings.Fields(hashOutput)
	if len(hashFields) != 2 || hashFields[0] != name || !exactConfigHash.MatchString(hashFields[1]) {
		return false, "", fmt.Errorf("Docker Compose returned an invalid config hash for %s", name)
	}
	wantImage := spec.Project.Image.ImageID
	if name == "postgres" {
		wantImage = spec.Project.Runtime.Deployment.Database.ImageID
	}
	labels := container.Labels
	current := container.Image == wantImage && labels["com.docker.compose.config-hash"] == hashFields[1] && labels[ownerLabel] == "deployment" && labels["com.docker.compose.project"] == projectName && labels["com.docker.compose.service"] == name && labels["com.docker.compose.container-number"] == "1" && labels["com.docker.compose.oneoff"] == "False" && labels["dev.dorf.project-version"] == strconv.Itoa(composeproject.ProjectVersion) && labels["dev.dorf.release"] == spec.Project.Image.Version
	if !current {
		return false, "container image or ownership labels differ", nil
	}
	return true, "current", nil
}

func (manager Manager) attestRunning(ctx context.Context, spec Spec, services []string) error {
	statuses, err := manager.inspectServices(ctx, spec, services)
	if err != nil {
		return err
	}
	for _, service := range services {
		status := statuses[service]
		if !status.Current || !status.Ready {
			return fmt.Errorf("%s container did not converge: %s", service, status.Detail)
		}
	}
	return nil
}

func (manager Manager) inspectServices(ctx context.Context, spec Spec, services []string) (map[string]ServiceStatus, error) {
	raw, err := manager.outputCompose(ctx, spec.ProjectDir, append([]string{"ps", "--all", "--format", "json"}, services...)...)
	if err != nil {
		return nil, err
	}
	records, err := decodeComposePS(raw)
	if err != nil {
		return nil, err
	}
	statuses := make(map[string]ServiceStatus, len(services))
	for _, service := range services {
		status := serviceFromRecords(records, service)
		record, unique := uniqueRecord(records, service)
		if unique && record.ID != "" {
			status.containerID = record.ID
			status.Current, status.Detail, err = manager.attestService(ctx, spec, service, record.ID)
			if err != nil {
				return nil, err
			}
			if status.Current && status.Ready {
				status.Detail = "ready"
			}
		}
		statuses[service] = status
	}
	return statuses, nil
}

func (manager Manager) apiStatus(ctx context.Context, version string) APIStatus {
	status := APIStatus{URL: ControlURL}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, ControlURL+"/v1", nil)
	response, err := manager.HTTPClient.Do(request)
	var discovery struct{ Product, Version string }
	if err != nil {
		status.Detail = "discovery request failed: " + err.Error()
		return status
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, probeBodyLimit))
	discoveryValid := response.StatusCode == http.StatusOK && decoder.Decode(&discovery) == nil && discovery.Product == "dorf" && discovery.Version == version
	response.Body.Close()
	if !discoveryValid {
		status.Detail = "discovery did not identify the expected Dorf release"
		return status
	}

	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, ControlURL+"/v1/me", nil)
	response, err = manager.HTTPClient.Do(request)
	if err != nil {
		status.Detail = "authentication probe failed: " + err.Error()
		return status
	}
	actualProblem, problemDecoded := decodeProbeProblem(response)
	expectedProblem, found := controlapi.ProblemForCode("unauthenticated")
	if response.StatusCode != http.StatusUnauthorized || !exactHeader(response.Header, "WWW-Authenticate", controlAuthChallenge) ||
		!exactHeader(response.Header, "Content-Type", "application/problem+json") || !problemDecoded || !found || !reflect.DeepEqual(actualProblem, expectedProblem) {
		status.Detail = "authentication probe did not prove the expected unauthenticated boundary"
		return status
	}
	status.Version, status.Ready, status.Detail = discovery.Version, true, "dorf "+discovery.Version
	return status
}

func exactHeader(header http.Header, name, value string) bool {
	values := header.Values(name)
	return len(values) == 1 && values[0] == value
}

func decodeProbeProblem(response *http.Response) (controlapi.Problem, bool) {
	var value struct {
		Type      *string         `json:"type"`
		Title     *string         `json:"title"`
		Status    *int            `json:"status"`
		Code      *string         `json:"code"`
		Retryable *bool           `json:"retryable"`
		Details   *map[string]any `json:"details"`
	}
	if response.Body == nil {
		return controlapi.Problem{}, false
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, probeBodyLimit+1))
	if err != nil || len(contents) > probeBodyLimit {
		return controlapi.Problem{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return controlapi.Problem{}, false
	}
	if value.Type == nil || value.Title == nil || value.Status == nil || value.Code == nil || value.Retryable == nil || value.Details == nil || *value.Details == nil {
		return controlapi.Problem{}, false
	}
	return controlapi.Problem{
		Type: *value.Type, Title: *value.Title, Status: *value.Status, Code: *value.Code,
		Retryable: *value.Retryable, Details: *value.Details,
	}, true
}

func (manager Manager) runCompose(ctx context.Context, directory string, stdout, stderr io.Writer, args ...string) error {
	return manager.Runner.Run(ctx, nil, stdout, stderr, "docker", composeArguments(directory, args...)...)
}

func (manager Manager) outputCompose(ctx context.Context, directory string, args ...string) (string, error) {
	return manager.Runner.Output(ctx, "docker", composeArguments(directory, args...)...)
}

func composeArguments(directory string, args ...string) []string {
	base := []string{"compose", "-p", projectName, "--project-directory", directory, "--file", filepath.Join(directory, composeproject.ComposeFile), "--env-file", filepath.Join(directory, composeproject.EnvironmentFile)}
	return append(base, args...)
}
