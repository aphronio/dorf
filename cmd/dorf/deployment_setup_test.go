package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/composeconfig"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/deployment"
)

func TestSelectDeploymentImagePreservesSameVersionChoice(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := "DORF_RELEASE='1.2.3'\nDORF_IMAGE_REF='dorf-local:proof'\nDORF_IMAGE_PULL_POLICY='never'\n"
	if err := os.WriteFile(filepath.Join(projectDir, composeconfig.EnvironmentFile), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	preserved, err := selectDeploymentImage(projectDir, os.Geteuid(), os.Getegid(), "1.2.3", "")
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Reference != "dorf-local:proof" || preserved.Pull {
		t.Fatalf("preserved image = %+v", preserved)
	}

	upgraded, err := selectDeploymentImage(projectDir, os.Geteuid(), os.Getegid(), "1.2.4", "")
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Reference != "ghcr.io/aphronio/dorf:1.2.4" || !upgraded.Pull {
		t.Fatalf("upgraded image = %+v", upgraded)
	}

	explicit, err := selectDeploymentImage(projectDir, os.Geteuid(), os.Getegid(), "1.2.4", " dorf-dev:test ")
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Reference != "dorf-dev:test" || explicit.Pull {
		t.Fatalf("explicit image = %+v", explicit)
	}
}

func TestMaterializeDeploymentConfigurationAppliesAndReplaysReadyProject(t *testing.T) {
	source := testDeploymentConfigurationSource(t)
	image := composeconfig.Image{Version: "1.2.3", Reference: "ghcr.io/aphronio/dorf:1.2.3", Pull: true}
	running := false
	client := &http.Client{Transport: deploymentRoundTripFunc(func(*http.Request) (*http.Response, error) {
		if !running {
			return nil, errors.New("connection refused")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"product":"dorf","version":"1.2.3"}`)),
			Header:     make(http.Header),
		}, nil
	})}
	applied := 0
	apply := func(_ context.Context, projectDir string) error {
		applied++
		if projectDir != source.Paths.ComposeDir {
			t.Fatalf("applied project = %q, want %q", projectDir, source.Paths.ComposeDir)
		}
		running = true
		return nil
	}

	if got, err := materializeDeploymentConfiguration(context.Background(), source, image, client, apply); err != nil || got != image || applied != 1 {
		t.Fatalf("initial materialization image=%+v error=%v applies=%d", got, err, applied)
	}
	running = false
	if got, err := materializeDeploymentConfiguration(context.Background(), source, image, client, apply); err != nil || got != image || applied != 2 {
		t.Fatalf("replayed materialization image=%+v error=%v applies=%d", got, err, applied)
	}
}

func TestMaterializeDeploymentConfigurationReturnsComposeApplyFailureWithoutReadinessProbe(t *testing.T) {
	source := testDeploymentConfigurationSource(t)
	image := composeconfig.Image{Version: "1.2.3", Reference: "ghcr.io/aphronio/dorf:1.2.3", Pull: true}
	requests := 0
	client := &http.Client{Transport: deploymentRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected readiness request")
	})}

	_, err := materializeDeploymentConfiguration(context.Background(), source, image, client, func(context.Context, string) error {
		return errors.New("worker exited before becoming healthy")
	})
	if err == nil || !strings.Contains(err.Error(), "apply Dorf Compose project") ||
		!strings.Contains(err.Error(), "worker exited before becoming healthy") || requests != 0 {
		t.Fatalf("Compose apply error=%v readiness requests=%d", err, requests)
	}
}

func TestMaterializeDeploymentConfigurationFailsWhenAppliedProjectIsNotReady(t *testing.T) {
	source := testDeploymentConfigurationSource(t)
	image := composeconfig.Image{Version: "1.2.3", Reference: "ghcr.io/aphronio/dorf:1.2.3", Pull: true}
	notReady := &http.Client{Transport: deploymentRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	applied := 0

	_, err := materializeDeploymentConfiguration(context.Background(), source, image, notReady, func(context.Context, string) error {
		applied++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") || applied != 1 {
		t.Fatalf("not-ready apply error=%v applies=%d", err, applied)
	}
}

func TestDockerComposeUpUsesGeneratedProjectAndWaitsForHealth(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "project")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(root, "invocation")
	executable := filepath.Join(root, "docker")
	script := "#!/bin/sh\n{ printf 'dir=%s\\n' \"$PWD\"; printf 'arg=%s\\n' \"$@\"; } >\"$CAPTURE\"\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := dockerComposeUpOutput(context.Background(), executable, projectDir, []string{"CAPTURE=" + capture})
	if err != nil || output != "" {
		t.Fatalf("compose output=%q error=%v", output, err)
	}
	contents, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := "dir=" + projectDir + "\narg=compose\narg=--progress\narg=quiet\narg=up\narg=--detach\narg=--wait\narg=--remove-orphans\n"
	if string(contents) != want {
		t.Fatalf("Compose invocation:\n%s\nwant:\n%s", contents, want)
	}
}

func TestResolveDeploymentManifestsUsesInstalledPairOrExactDevelopmentFallback(t *testing.T) {
	installedRoot := t.TempDir()
	installedBinary := filepath.Join(installedRoot, "dorf")
	writeTestFile(t, filepath.Join(installedRoot, composeconfig.InstalledBaseFile))
	writeTestFile(t, filepath.Join(installedRoot, composeconfig.InstalledIncusFile))
	base, incus, err := resolveDeploymentManifests(installedBinary)
	if err != nil || base != filepath.Join(installedRoot, composeconfig.InstalledBaseFile) || incus != filepath.Join(installedRoot, composeconfig.InstalledIncusFile) {
		t.Fatalf("installed manifests = %q, %q, %v", base, incus, err)
	}

	developmentRoot := t.TempDir()
	developmentBinary := filepath.Join(developmentRoot, ".dorf", "bin", "dorf")
	if err := os.MkdirAll(filepath.Dir(developmentBinary), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(developmentRoot, filepath.FromSlash(composeconfig.SourceBaseComposeFile)))
	writeTestFile(t, filepath.Join(developmentRoot, filepath.FromSlash(composeconfig.SourceIncusComposeFile)))
	base, incus, err = resolveDeploymentManifests(developmentBinary)
	if err != nil || base != filepath.Join(developmentRoot, filepath.FromSlash(composeconfig.SourceBaseComposeFile)) || incus != filepath.Join(developmentRoot, filepath.FromSlash(composeconfig.SourceIncusComposeFile)) {
		t.Fatalf("development manifests = %q, %q, %v", base, incus, err)
	}

	if err := os.Remove(incus); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveDeploymentManifests(developmentBinary); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("incomplete development manifests error = %v", err)
	}
}

func TestContainerForegroundDispatchIsHiddenAndStrict(t *testing.T) {
	if handled, err := containerForegroundCommand(context.Background(), []string{"service"}, io.Discard, io.Discard); handled || err != nil {
		t.Fatalf("public-looking command handled=%t error=%v", handled, err)
	}
	if handled, err := containerForegroundCommand(context.Background(), []string{"_container-control-api-health", "extra"}, io.Discard, io.Discard); !handled || err == nil || !strings.Contains(err.Error(), "does not accept") {
		t.Fatalf("hidden command handled=%t error=%v", handled, err)
	}
}

func TestPublicServiceCommandHasNoEarlyDispatch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	err := run(context.Background(), []string{"service", "status"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "PostgreSQL is not configured") {
		t.Fatalf("public service command error = %v", err)
	}
}

func TestCheckDockerRuntimeRequiresEngineAndComposePlugin(t *testing.T) {
	var calls [][]string
	err := checkDockerRuntimeWith(context.Background(), func() (string, error) {
		return "/usr/bin/docker", nil
	}, func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "info" {
			return "28.0.0\n", nil
		}
		return "v2.39.2\n", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/usr/bin/docker", "info", "--format", "{{.ServerVersion}}"},
		{"/usr/bin/docker", "compose", "version", "--short"},
	}
	if !slices.EqualFunc(calls, want, slices.Equal) {
		t.Fatalf("Docker readiness calls = %#v, want %#v", calls, want)
	}
}

func TestCheckDockerRuntimeReportsMissingComposePlugin(t *testing.T) {
	missing := errors.New("compose is not a docker command")
	err := checkDockerRuntimeWith(context.Background(), func() (string, error) {
		return "/usr/bin/docker", nil
	}, func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "info" {
			return "28.0.0\n", nil
		}
		return "", missing
	})
	if !errors.Is(err, missing) || !strings.Contains(err.Error(), "Compose plugin is unavailable") {
		t.Fatalf("missing Compose error = %v", err)
	}
}

func TestShellJoinQuotesAdministratorHandoffArguments(t *testing.T) {
	got := shellJoin([]string{"sudo", "/tmp/helper with spaces", "operator's-name"})
	want := `'sudo' '/tmp/helper with spaces' 'operator'"'"'s-name'`
	if got != want {
		t.Fatalf("shellJoin() = %q, want %q", got, want)
	}
}

func testDeploymentConfigurationSource(t *testing.T) deploymentConfigurationSource {
	t.Helper()
	root := t.TempDir()
	paths := config.Paths{
		ConfigDir: filepath.Join(root, "config"), DataDir: filepath.Join(root, "data"),
		StateDir: filepath.Join(root, "state"), ComposeDir: filepath.Join(root, "project"),
	}
	for _, path := range []string{paths.ConfigDir, paths.DataDir, paths.StateDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	baseFile := filepath.Join(root, "dorf-compose.yaml")
	incusFile := filepath.Join(root, "dorf-compose-incus.yaml")
	writeTestFile(t, baseFile)
	writeTestFile(t, incusFile)
	return deploymentConfigurationSource{
		Paths: paths, BaseFile: baseFile, IncusFile: incusFile, UID: os.Geteuid(), GID: os.Getegid(),
		Deployment: deployment.Config{
			ControlReaderKey: strings.Repeat("a", 64),
			Database: deployment.Database{
				Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret",
			},
		},
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type deploymentRoundTripFunc func(*http.Request) (*http.Response, error)

func (function deploymentRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
