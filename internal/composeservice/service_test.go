package composeservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/composeproject"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/dockerexec"
	"github.com/aphronio/dorf/internal/release"
)

const (
	testDorfImageID     = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testPostgresImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestSanitizedCommandEnvironmentForcesLocalDefaultContext(t *testing.T) {
	environment, err := sanitizedCommandEnvironment([]string{
		"PATH=/tools", "HOME=/home/operator", "DOCKER_CONTEXT=remote",
		"DOCKER_CONFIG=/tmp/ambient", "DOCKER_API_VERSION=1.24",
		"DOCKER_HOST=unix:///run/user/1000/docker.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	if !strings.Contains(joined, "PATH=/usr/sbin:/usr/bin:/sbin:/bin") || strings.Contains(joined, "PATH=/tools") {
		t.Fatalf("environment retained ambient executable authority:\n%s", joined)
	}
	for _, want := range []string{"DOCKER_HOST=unix:///run/user/1000/docker.sock"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("environment missing %s:\n%s", want, joined)
		}
	}
	for _, absent := range []string{"DOCKER_CONFIG=", "DOCKER_API_VERSION=", "DOCKER_CONTEXT="} {
		if strings.Contains(joined, absent) {
			t.Fatalf("environment retained %s:\n%s", absent, joined)
		}
	}
	if _, err := sanitizedCommandEnvironment([]string{"DOCKER_HOST=tcp://docker.example:2375"}); err == nil {
		t.Fatal("remote Docker endpoint was accepted")
	}
	for _, host := range []string{"unix:///", " unix:///run/docker.sock", "unix:///run/../docker.sock"} {
		if _, err := sanitizedCommandEnvironment([]string{"DOCKER_HOST=" + host}); err == nil {
			t.Fatalf("unsafe local endpoint %q was accepted", host)
		}
	}
	defaultEnvironment, err := sanitizedCommandEnvironment([]string{"DOCKER_CONTEXT=remote"})
	if err != nil || !strings.Contains(strings.Join(defaultEnvironment, "\n"), "DOCKER_CONTEXT=default") {
		t.Fatalf("default context was not forced: %v %v", defaultEnvironment, err)
	}
}

func TestComposeCommandUsesOneResolvedProtectedDockerExecutable(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	calls := 0
	command, err := composeCommandWithResolver(context.Background(), func() (string, error) {
		calls++
		return dockerexec.LocalPath, nil
	}, "docker", "compose", "version")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || command.Path != dockerexec.LocalPath || !slices.Equal(command.Args, []string{dockerexec.LocalPath, "compose", "version"}) {
		t.Fatalf("resolver calls=%d command=%#v", calls, command)
	}

	refusal := errors.New("unsafe Docker executable")
	calls = 0
	if command, err = composeCommandWithResolver(context.Background(), func() (string, error) {
		calls++
		return "", refusal
	}, "docker", "info"); !errors.Is(err, refusal) || command != nil || calls != 1 {
		t.Fatalf("refused command=%#v error=%v resolver calls=%d", command, err, calls)
	}
}

func TestSpecRejectsMutablePostgresImage(t *testing.T) {
	spec := testSpec(t)
	spec.Project.Runtime.Deployment.Database.ImageID = "postgres:17"
	if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "exact sha256") {
		t.Fatalf("Validate() error=%v", err)
	}
}

func TestProjectCurrentChecksEveryGeneratedArtifact(t *testing.T) {
	spec := testSpec(t)
	for _, directory := range []string{spec.Project.Runtime.ConfigDir, spec.Project.Runtime.DataDir, spec.Project.Runtime.StateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := spec.Project.Materialize(spec.ProjectDir); err != nil {
		t.Fatal(err)
	}
	if current, err := projectCurrent(spec); err != nil || !current {
		t.Fatalf("materialized project current=%t err=%v", current, err)
	}
	if err := os.Remove(filepath.Join(spec.ProjectDir, composeproject.ControlDeploymentFile)); err != nil {
		t.Fatal(err)
	}
	if current, err := projectCurrent(spec); err != nil || current {
		t.Fatalf("project missing generated artifact current=%t err=%v", current, err)
	}
	if err := spec.Project.Materialize(spec.ProjectDir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(spec.ProjectDir, "control-config", "dorf", "integrations", "github", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale provider authority\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if current, err := projectCurrent(spec); err != nil || current {
		t.Fatalf("project with stale projected credential current=%t err=%v", current, err)
	}
}

func TestSameDeploymentComparesIncusAndIgnoresOnlyDatabaseVolumeState(t *testing.T) {
	first := deployment.Config{
		Database: deployment.Database{Password: "secret", VolumeState: deployment.DatabaseVolumePending},
		E2B:      &deployment.E2B{APIKey: "e2b-secret"},
		Incus: &deployment.Incus{
			Endpoint: "https://incus.example:8443", ServerCertificate: "server",
			ClientCertificate: "client", ClientPrivateKey: "private",
		},
	}
	second := first
	second.E2B = &deployment.E2B{APIKey: first.E2B.APIKey}
	clonedIncus := *first.Incus
	second.Incus = &clonedIncus
	second.Database.VolumeState = deployment.DatabaseVolumeInitialized
	if !sameDeployment(first, second) {
		t.Fatal("equivalent deployment values differed")
	}
	second.Incus.ClientPrivateKey = "rotated"
	if sameDeployment(first, second) {
		t.Fatal("Incus credential drift was ignored")
	}
}

func TestApplyAttestsNewImageBeforeStoppingOldRuntime(t *testing.T) {
	spec := testSpec(t)
	if err := deployment.Save(spec.Project.Runtime.DeploymentPath, spec.Project.Runtime.Deployment); err != nil {
		t.Fatal(err)
	}
	runner := &applyRunner{spec: spec}
	manager := Manager{Runner: runner, HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return readyAPIResponse(t, request, "1.2.3"), nil
	})}}
	if err := manager.Apply(context.Background(), spec, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	imageInspect, stop := -1, -1
	for index, call := range runner.calls {
		if strings.Contains(call, "image inspect") && strings.Contains(call, testDorfImageID) && imageInspect < 0 {
			imageInspect = index
		}
		if strings.Contains(call, " stop worker control-reader control-api postgres") {
			stop = index
		}
	}
	if imageInspect < 0 || stop < 0 || imageInspect > stop {
		t.Fatalf("new image was not attested before stop:\n%s", strings.Join(runner.calls, "\n"))
	}
	stored, found, err := deployment.Load(spec.Project.Runtime.DeploymentPath)
	if err != nil || !found || stored.Database.VolumeState != deployment.DatabaseVolumeInitialized {
		t.Fatalf("database receipt=%+v found=%v err=%v", stored.Database, found, err)
	}
	freshProject, err := composeproject.Render(composeproject.Spec{
		Image: spec.Project.Image, UID: spec.Project.Runtime.UID, GID: spec.Project.Runtime.GID,
		ConfigDir: spec.Project.Runtime.ConfigDir, DataDir: spec.Project.Runtime.DataDir, StateDir: spec.Project.Runtime.StateDir,
		Deployment: stored, Gateway: spec.Project.Runtime.Gateway, Cloudflare: spec.Project.Runtime.Cloudflare,
	})
	if err != nil {
		t.Fatal(err)
	}
	spec.Project = freshProject
	runner.spec = spec
	status, err := manager.Status(context.Background(), spec)
	if err != nil || !status.Ready {
		t.Fatalf("post-initialization status=%+v err=%v", status, err)
	}
}

func TestApplyRefusesForeignFixedTargetBeforeDockerMutation(t *testing.T) {
	spec := testSpec(t)
	if err := deployment.Save(spec.Project.Runtime.DeploymentPath, spec.Project.Runtime.Deployment); err != nil {
		t.Fatal(err)
	}
	runner := &foreignTargetRunner{applyRunner: applyRunner{spec: spec}}
	err := (Manager{Runner: runner}).Apply(context.Background(), spec, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("Apply() error=%v", err)
	}
	if len(runner.mutations) != 0 {
		t.Fatalf("foreign project was mutated:\n%s", strings.Join(runner.mutations, "\n"))
	}
	if _, statErr := os.Lstat(spec.ProjectDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Compose project was materialized before ownership preflight: %v", statErr)
	}
}

func TestApplyRemovesOnlyAttestedObsoleteOptionalServices(t *testing.T) {
	spec := testSpec(t)
	if err := deployment.Save(spec.Project.Runtime.DeploymentPath, spec.Project.Runtime.Deployment); err != nil {
		t.Fatal(err)
	}
	runner := &orphanRunner{applyRunner: applyRunner{spec: spec}}
	if err := (Manager{Runner: runner}).Apply(context.Background(), spec, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	remove := "docker container rm --force cloudflare-old-id gateway-old-id"
	removeIndex, stopIndex := slices.Index(runner.calls, remove), -1
	for index, call := range runner.calls {
		if strings.Contains(call, " stop worker control-reader control-api postgres") {
			stopIndex = index
			break
		}
	}
	if removeIndex < 0 || stopIndex < 0 || removeIndex > stopIndex {
		t.Fatalf("obsolete service removal was not explicit and preparation-first:\n%s", strings.Join(runner.calls, "\n"))
	}
}

func TestApplyRefusesForeignObsoleteOptionalServiceBeforeDockerMutation(t *testing.T) {
	spec := testSpec(t)
	if err := deployment.Save(spec.Project.Runtime.DeploymentPath, spec.Project.Runtime.Deployment); err != nil {
		t.Fatal(err)
	}
	runner := &foreignTargetRunner{applyRunner: applyRunner{spec: spec}, service: "provider-gateway"}
	err := (Manager{Runner: runner}).Apply(context.Background(), spec, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("Apply() error=%v", err)
	}
	if len(runner.mutations) != 0 {
		t.Fatalf("foreign optional service was mutated:\n%s", strings.Join(runner.mutations, "\n"))
	}
}

func TestStatusRequiresObsoleteOptionalServicesToBeReconciled(t *testing.T) {
	spec := testSpec(t)
	if err := deployment.Save(spec.Project.Runtime.DeploymentPath, spec.Project.Runtime.Deployment); err != nil {
		t.Fatal(err)
	}
	if err := spec.Project.Materialize(spec.ProjectDir); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{spec.Project.Runtime.ConfigDir, spec.Project.Runtime.DataDir, spec.Project.Runtime.StateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runner := &orphanRunner{applyRunner: applyRunner{spec: spec}}
	status, err := (Manager{Runner: runner}).Status(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "obsolete Compose service") || status.Ready {
		t.Fatalf("Status()=%+v error=%v", status, err)
	}
}

func TestRestartRefusesWrongContainerImage(t *testing.T) {
	spec := testSpec(t)
	if err := deployment.Save(spec.Project.Runtime.DeploymentPath, spec.Project.Runtime.Deployment); err != nil {
		t.Fatal(err)
	}
	if err := spec.Project.Materialize(spec.ProjectDir); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{spec.Project.Runtime.ConfigDir, spec.Project.Runtime.DataDir, spec.Project.Runtime.StateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runner := &targetRunner{spec: spec, containerImage: "sha256:" + strings.Repeat("f", 64)}
	err := (Manager{Runner: runner}).Restart(context.Background(), spec, TargetWorker, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "stale or foreign") {
		t.Fatalf("Restart() error=%v", err)
	}
	if runner.restarted {
		t.Fatal("stale container was restarted")
	}
}

func TestRestartForceRecreatesAttestedTargetAndProvesFreshIdentity(t *testing.T) {
	spec := testSpec(t)
	if err := deployment.Save(spec.Project.Runtime.DeploymentPath, spec.Project.Runtime.Deployment); err != nil {
		t.Fatal(err)
	}
	if err := spec.Project.Materialize(spec.ProjectDir); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{spec.Project.Runtime.ConfigDir, spec.Project.Runtime.DataDir, spec.Project.Runtime.StateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runner := &targetRunner{spec: spec, containerImage: spec.Project.Image.ImageID}
	if err := (Manager{Runner: runner}).Restart(context.Background(), spec, TargetWorker, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !runner.restarted || runner.generation != 1 {
		t.Fatalf("force recreation=%t generation=%d calls=%s", runner.restarted, runner.generation, strings.Join(runner.calls, "\n"))
	}
	want := "compose -p dorf --project-directory " + spec.ProjectDir + " --file " + filepath.Join(spec.ProjectDir, composeproject.ComposeFile) + " --env-file " + filepath.Join(spec.ProjectDir, composeproject.EnvironmentFile) + " up --detach --force-recreate --no-deps --wait worker"
	if !slices.Contains(runner.calls, "docker "+want) {
		t.Fatalf("restart did not use scoped Compose force-recreate:\n%s", strings.Join(runner.calls, "\n"))
	}
}

func TestAPIStatusRequiresExactRelease(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1" {
			return nil, errors.New("unexpected request")
		}
		return jsonResponse(http.StatusOK, "application/json", `{"product":"dorf","version":"9.9.9"}`), nil
	})}
	status := (Manager{HTTPClient: client}).configured().apiStatus(context.Background(), "1.2.3")
	if status.Ready || !strings.Contains(status.Detail, "expected") {
		t.Fatalf("status=%+v", status)
	}
}

func TestAPIStatusProvesUnauthenticatedControlBoundaryWithoutCredentials(t *testing.T) {
	var paths []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if authorization := request.Header.Values("Authorization"); len(authorization) != 0 {
			t.Fatalf("status probe sent Authorization to %s: %q", request.URL.Path, authorization)
		}
		return readyAPIResponse(t, request, "1.2.3"), nil
	})}
	status := (Manager{HTTPClient: client}).configured().apiStatus(context.Background(), "1.2.3")
	if !status.Ready || status.Version != "1.2.3" {
		t.Fatalf("status=%+v", status)
	}
	if got := strings.Join(paths, "\n"); got != "/v1\n/v1/me" {
		t.Fatalf("probe paths:\n%s", got)
	}
}

func TestAPIStatusRejectsIncompleteUnauthenticatedProof(t *testing.T) {
	expected, found := controlapi.ProblemForCode("unauthenticated")
	if !found {
		t.Fatal("unauthenticated Problem is not catalogued")
	}
	problemJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	wrong, found := controlapi.ProblemForCode("enrollment_unavailable")
	if !found {
		t.Fatal("enrollment_unavailable Problem is not catalogued")
	}
	wrongProblemJSON, err := json.Marshal(wrong)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		status      int
		challenge   []string
		contentType string
		body        string
	}{
		{name: "wrong status", status: http.StatusOK, challenge: []string{controlAuthChallenge}, contentType: "application/problem+json", body: string(problemJSON)},
		{name: "missing challenge", status: http.StatusUnauthorized, contentType: "application/problem+json", body: string(problemJSON)},
		{name: "wrong challenge", status: http.StatusUnauthorized, challenge: []string{`Bearer realm="other"`}, contentType: "application/problem+json", body: string(problemJSON)},
		{name: "multiple challenges", status: http.StatusUnauthorized, challenge: []string{controlAuthChallenge, "Basic"}, contentType: "application/problem+json", body: string(problemJSON)},
		{name: "wrong content type", status: http.StatusUnauthorized, challenge: []string{controlAuthChallenge}, contentType: "application/problem+json; charset=utf-8", body: string(problemJSON)},
		{name: "wrong problem", status: http.StatusUnauthorized, challenge: []string{controlAuthChallenge}, contentType: "application/problem+json", body: string(wrongProblemJSON)},
		{name: "malformed problem", status: http.StatusUnauthorized, challenge: []string{controlAuthChallenge}, contentType: "application/problem+json", body: `{}`},
		{name: "incomplete problem", status: http.StatusUnauthorized, challenge: []string{controlAuthChallenge}, contentType: "application/problem+json", body: strings.Replace(string(problemJSON), `"retryable":false,`, "", 1)},
		{name: "trailing problem", status: http.StatusUnauthorized, challenge: []string{controlAuthChallenge}, contentType: "application/problem+json", body: string(problemJSON) + `{}`},
		{name: "oversized problem", status: http.StatusUnauthorized, challenge: []string{controlAuthChallenge}, contentType: "application/problem+json", body: string(problemJSON) + strings.Repeat(" ", probeBodyLimit)},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/v1" {
					return jsonResponse(http.StatusOK, "application/json", `{"product":"dorf","version":"1.2.3"}`), nil
				}
				response := jsonResponse(test.status, test.contentType, test.body)
				response.Header["WWW-Authenticate"] = test.challenge
				return response, nil
			})}
			status := (Manager{HTTPClient: client}).configured().apiStatus(context.Background(), "1.2.3")
			if status.Ready || !strings.Contains(status.Detail, "unauthenticated boundary") {
				t.Fatalf("status=%+v", status)
			}
		})
	}
}

func TestControlReaderStatusRequiresAuthenticatedHealthcheck(t *testing.T) {
	records := []composePS{{ID: "reader-id", Service: "control-reader", State: "running", Health: "starting"}}
	if status := serviceFromRecords(records, "control-reader"); status.Ready || !strings.Contains(status.Detail, "health") {
		t.Fatalf("starting reader status=%+v", status)
	}
	records[0].Health = "healthy"
	if status := serviceFromRecords(records, "control-reader"); !status.Ready {
		t.Fatalf("healthy reader status=%+v", status)
	}
}

func TestCheckDockerProvesDaemonAndComposeWithoutMutation(t *testing.T) {
	runner := &dockerCheckRunner{}
	if err := (Manager{Runner: runner}).CheckDocker(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.calls, "\n"); got != "docker info --format {{.ServerVersion}}\ndocker compose version --short" {
		t.Fatalf("Docker readiness calls:\n%s", got)
	}
}

func TestApplyConvergesOnlyDesiredOptionalServices(t *testing.T) {
	spec := testSpec(t)
	spec.Project.Runtime.Gateway = &composeproject.OptionalService{StatePath: filepath.Join(spec.Project.Runtime.DataDir, "provider-gateway"), Digest: strings.Repeat("a", 64), PublishAddress: "127.0.0.1"}
	spec.Project.Runtime.Cloudflare = &composeproject.OptionalService{StatePath: filepath.Join(spec.Project.Runtime.DataDir, "provider-gateway", "cloudflare"), Digest: strings.Repeat("c", 64)}
	projectSpec := composeproject.Spec{
		Image: spec.Project.Image, UID: spec.Project.Runtime.UID, GID: spec.Project.Runtime.GID,
		ConfigDir: spec.Project.Runtime.ConfigDir, DataDir: spec.Project.Runtime.DataDir, StateDir: spec.Project.Runtime.StateDir,
		Deployment: spec.Project.Runtime.Deployment, Gateway: spec.Project.Runtime.Gateway, Cloudflare: spec.Project.Runtime.Cloudflare,
	}
	project, err := composeproject.Render(projectSpec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Project = project
	if err := deployment.Save(spec.Project.Runtime.DeploymentPath, spec.Project.Runtime.Deployment); err != nil {
		t.Fatal(err)
	}
	runner := &applyRunner{spec: spec}
	if err := (Manager{Runner: runner}).Apply(context.Background(), spec, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(runner.calls, "\n")
	if !strings.Contains(calls, " up --detach --wait provider-gateway cloudflared worker control-reader control-api") {
		t.Fatalf("optional services were not converged with the runtime:\n%s", calls)
	}
}

func testSpec(t *testing.T) Spec {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	stateDir := filepath.Join(root, "state")
	projectDir := filepath.Join(root, "compose")
	project, err := composeproject.Render(composeproject.Spec{
		Image: release.ComposeImage{
			Version: "1.2.3", Reference: "dorf-local:integration",
			ImageID: testDorfImageID, BinarySHA256: strings.Repeat("b", 64),
		},
		UID: os.Geteuid(), GID: os.Getegid(), ConfigDir: configDir, DataDir: dataDir, StateDir: stateDir,
		Deployment: deployment.Config{ControlReaderKey: strings.Repeat("c", 64), Database: deployment.Database{
			Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret",
			Image: "postgres:17.10-bookworm", ImageID: testPostgresImageID, VolumeState: deployment.DatabaseVolumePending,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return Spec{ProjectDir: projectDir, Project: project}
}

type applyRunner struct {
	spec  Spec
	calls []string
}

type foreignTargetRunner struct {
	applyRunner
	mutations []string
	service   string
}

func (runner *foreignTargetRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	runner.mutations = append(runner.mutations, name+" "+strings.Join(args, " "))
	return runner.applyRunner.Run(ctx, stdin, stdout, stderr, name, args...)
}

func (runner *foreignTargetRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	service := runner.service
	if service == "" {
		service = "worker"
	}
	if strings.Contains(call, "container inspect --format {{json .}} "+fixedContainerName(service)) {
		value := map[string]any{
			"Id": "foreign-service-id", "Name": "/" + fixedContainerName(service), "Image": testDorfImageID,
			"Config": map[string]any{"Labels": map[string]string{
				"com.docker.compose.project": projectName, "com.docker.compose.service": service,
				"com.docker.compose.container-number": "1", "com.docker.compose.oneoff": "False",
				"com.docker.compose.config-hash": strings.Repeat("1", 64),
				"dev.dorf.project-version":       strconv.Itoa(composeproject.ProjectVersion), "dev.dorf.release": "1.2.3",
			}},
		}
		contents, _ := json.Marshal(value)
		return string(contents), nil
	}
	return runner.applyRunner.Output(ctx, name, args...)
}

type orphanRunner struct {
	applyRunner
	removed bool
}

func (runner *orphanRunner) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	call := name + " " + strings.Join(args, " ")
	if call == "docker container rm --force cloudflare-old-id gateway-old-id" {
		runner.calls = append(runner.calls, call)
		runner.removed = true
		return nil
	}
	return runner.applyRunner.Run(ctx, stdin, stdout, stderr, name, args...)
}

func (runner *orphanRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	if !runner.removed {
		switch {
		case strings.Contains(call, "container ls") && strings.Contains(call, "label=com.docker.compose.project="+projectName):
			runner.calls = append(runner.calls, call)
			return "cloudflare-old-id\ngateway-old-id\n", nil
		case strings.HasSuffix(call, " cloudflare-old-id"), strings.HasSuffix(call, " "+fixedContainerName("cloudflared")):
			runner.calls = append(runner.calls, call)
			return containerJSON("cloudflare-old-id", "cloudflared", testDorfImageID, true, false), nil
		case strings.HasSuffix(call, " gateway-old-id"), strings.HasSuffix(call, " "+fixedContainerName("provider-gateway")):
			runner.calls = append(runner.calls, call)
			return containerJSON("gateway-old-id", "provider-gateway", testDorfImageID, true, false), nil
		}
	}
	return runner.applyRunner.Output(ctx, name, args...)
}

type dockerCheckRunner struct{ calls []string }

func (runner *dockerCheckRunner) Run(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error {
	return errors.New("Docker readiness must not mutate")
}

func (runner *dockerCheckRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	runner.calls = append(runner.calls, name+" "+strings.Join(args, " "))
	if len(args) > 0 && args[0] == "info" {
		return "28.0.0\n", nil
	}
	return "2.40.0\n", nil
}

func (runner *applyRunner) Run(_ context.Context, _ io.Reader, _, _ io.Writer, name string, args ...string) error {
	runner.calls = append(runner.calls, name+" "+strings.Join(args, " "))
	return nil
}

func (runner *applyRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, call)
	switch {
	case strings.Contains(call, "image inspect --format {{.Id}} "+testPostgresImageID):
		return testPostgresImageID, nil
	case strings.Contains(call, "container ls") && strings.Contains(call, "label=com.docker.compose.project="+projectName):
		return "", nil
	case isFixedTargetInspect(call):
		return "", errors.New("No such container")
	case strings.Contains(call, "container inspect --format {{json .}} dorf-postgres"):
		return "", errors.New("No such container")
	case strings.Contains(call, "volume inspect"):
		return "database", nil
	case strings.Contains(call, "container ls") && strings.Contains(call, "volume="):
		return "", nil
	case strings.Contains(call, "image inspect --format {{json .}} "+testDorfImageID):
		return dockerImageJSON(runner.spec), nil
	case strings.Contains(call, "compose") && strings.Contains(call, " ps "):
		if strings.HasSuffix(call, " migrate") {
			return `[{"ID":"migrate-id","Service":"migrate","State":"exited","ExitCode":0}]`, nil
		}
		return composePSJSON(runner.spec), nil
	case strings.Contains(call, "compose") && strings.Contains(call, " config --hash "):
		service := args[len(args)-1]
		return service + " " + strings.Repeat("1", 64), nil
	case strings.Contains(call, "container inspect --format {{json .}} postgres-id"):
		return containerJSON("postgres-id", "postgres", testPostgresImageID, true, true), nil
	case strings.Contains(call, "container inspect --format {{json .}} worker-id"):
		return containerJSON("worker-id", "worker", testDorfImageID, true, false), nil
	case strings.Contains(call, "container inspect --format {{json .}} reader-id"):
		return containerJSON("reader-id", "control-reader", testDorfImageID, true, false), nil
	case strings.Contains(call, "container inspect --format {{json .}} api-id"):
		return containerJSON("api-id", "control-api", testDorfImageID, true, false), nil
	case strings.Contains(call, "container inspect --format {{json .}} gateway-id"):
		return containerJSON("gateway-id", "provider-gateway", testDorfImageID, true, false), nil
	case strings.Contains(call, "container inspect --format {{json .}} cloudflare-id"):
		return containerJSON("cloudflare-id", "cloudflared", testDorfImageID, true, false), nil
	default:
		return "", errors.New("unexpected command: " + call)
	}
}

type targetRunner struct {
	spec           Spec
	containerImage string
	restarted      bool
	generation     int
	calls          []string
}

func (runner *targetRunner) Run(_ context.Context, _ io.Reader, _, _ io.Writer, name string, args ...string) error {
	call := name + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, call)
	if name == "docker" && strings.Contains(call, " compose ") && strings.Contains(call, " up --detach --force-recreate --no-deps --wait worker") {
		runner.restarted = true
		runner.generation++
	}
	return nil
}

func (runner *targetRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	runner.calls = append(runner.calls, call)
	id := "worker-old-id"
	if runner.generation > 0 {
		id = "worker-new-id"
	}
	switch {
	case strings.Contains(call, "image inspect"):
		return dockerImageJSON(runner.spec), nil
	case strings.Contains(call, "compose") && strings.Contains(call, " ps "):
		return `[{"ID":"` + id + `","Service":"worker","State":"running"}]`, nil
	case strings.Contains(call, "compose") && strings.Contains(call, " config --hash "):
		return "worker " + strings.Repeat("1", 64), nil
	case strings.Contains(call, "container inspect"):
		return containerJSON(id, "worker", runner.containerImage, true, false), nil
	default:
		return "", errors.New("unexpected command: " + call)
	}
}

func dockerImageJSON(spec Spec) string {
	value := map[string]any{
		"Id": spec.Project.Image.ImageID, "Os": "linux", "Architecture": "amd64",
		"Config": map[string]any{"Labels": map[string]string{
			"org.opencontainers.image.version": spec.Project.Image.Version,
			"dev.dorf.binary-sha256":           spec.Project.Image.BinarySHA256,
		}},
	}
	contents, _ := json.Marshal(value)
	return string(contents)
}

func composePSJSON(spec Spec) string {
	records := []map[string]string{{"ID": "postgres-id", "Service": "postgres", "State": "running", "Health": "healthy"}}
	if spec.Project.Runtime.Gateway != nil {
		records = append(records, map[string]string{"ID": "gateway-id", "Service": "provider-gateway", "State": "running"})
	}
	if spec.Project.Runtime.Cloudflare != nil {
		records = append(records, map[string]string{"ID": "cloudflare-id", "Service": "cloudflared", "State": "running"})
	}
	records = append(records,
		map[string]string{"ID": "worker-id", "Service": "worker", "State": "running"},
		map[string]string{"ID": "reader-id", "Service": "control-reader", "State": "running", "Health": "healthy"},
		map[string]string{"ID": "api-id", "Service": "control-api", "State": "running"},
	)
	raw, _ := json.Marshal(records)
	return string(raw)
}

func containerJSON(id, service, image string, running, volume bool) string {
	mounts := []map[string]string{}
	if volume {
		mounts = append(mounts, map[string]string{"Name": databaseVolume, "Destination": "/var/lib/postgresql/data"})
	}
	value := map[string]any{
		"Id": id, "Name": "/" + fixedContainerName(service), "Image": image, "State": map[string]bool{"Running": running}, "Mounts": mounts,
		"Config": map[string]any{"Labels": map[string]string{
			ownerLabel: "deployment", "com.docker.compose.project": projectName,
			"com.docker.compose.service": service, "com.docker.compose.container-number": "1",
			"com.docker.compose.oneoff": "False", "com.docker.compose.config-hash": strings.Repeat("1", 64),
			"dev.dorf.project-version": strconv.Itoa(composeproject.ProjectVersion), "dev.dorf.release": "1.2.3",
		}},
	}
	contents, _ := json.Marshal(value)
	return string(contents)
}

func isFixedTargetInspect(call string) bool {
	for _, service := range fixedComposeServices {
		if strings.HasSuffix(call, " "+fixedContainerName(service)) {
			return true
		}
	}
	return false
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jsonResponse(status int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(bytes.NewBufferString(body))}
}

func readyAPIResponse(t *testing.T, request *http.Request, version string) *http.Response {
	t.Helper()
	if authorization := request.Header.Values("Authorization"); request.Method != http.MethodGet || len(authorization) != 0 {
		t.Fatalf("status probe request=%s %s Authorization=%q", request.Method, request.URL.Path, authorization)
	}
	switch request.URL.Path {
	case "/v1":
		return jsonResponse(http.StatusOK, "application/json", `{"product":"dorf","version":"`+version+`"}`)
	case "/v1/me":
		problem, found := controlapi.ProblemForCode("unauthenticated")
		if !found {
			t.Fatal("unauthenticated Problem is not catalogued")
		}
		contents, err := json.Marshal(problem)
		if err != nil {
			t.Fatal(err)
		}
		response := jsonResponse(http.StatusUnauthorized, "application/problem+json", string(contents))
		response.Header.Set("WWW-Authenticate", controlAuthChallenge)
		return response
	default:
		t.Fatalf("unexpected status probe path %q", request.URL.Path)
		return nil
	}
}
