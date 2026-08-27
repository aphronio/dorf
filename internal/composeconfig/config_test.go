package composeconfig

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/deployment"
	"go.yaml.in/yaml/v4"
)

const testReaderKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRenderProjectsOneProtectedComposeEnvironment(t *testing.T) {
	spec := testSpec(t)
	dataDir := spec.DataDir
	spec.Gateway = &OptionalService{
		StatePath: filepath.Join(dataDir, "provider-gateway"), Digest: strings.Repeat("a", 64), PublishAddress: "10.44.0.1",
	}
	spec.Cloudflare = &OptionalService{
		StatePath: filepath.Join(dataDir, "provider-gateway", "cloudflare"), Digest: strings.Repeat("b", 64),
	}
	config, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if config.Image != spec.Image || config.IncusOverlay {
		t.Fatalf("config image=%+v incus_overlay=%t", config.Image, config.IncusOverlay)
	}
	environment := string(config.environment)
	for _, wanted := range []string{
		"COMPOSE_FILE='" + spec.BaseFile + "'",
		"COMPOSE_PROFILES='gateway,cloudflare'",
		"DORF_RELEASE='1.2.3'",
		"DORF_IMAGE_REF='ghcr.io/aphronio/dorf:1.2.3'",
		"DORF_IMAGE_PULL_POLICY='always'",
		"DORF_LOCAL_INCUS='false'",
		"DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN='http://provider-gateway:8317'",
		"E2B_API_KEY='e2b-secret'",
	} {
		if !strings.Contains(environment, wanted) {
			t.Errorf("environment missing %q:\n%s", wanted, environment)
		}
	}
	token := dotenvValue(t, config.environment, "DORF_CONTROL_READER_TOKEN")
	if len(token) != 64 || token == testReaderKey || strings.Contains(environment, testReaderKey) {
		t.Fatalf("derived reader token=%q leaked raw key=%t", token, strings.Contains(environment, testReaderKey))
	}
}

func TestRenderAcceptsRootContainerIdentity(t *testing.T) {
	spec := testSpec(t)
	spec.UID, spec.GID = 0, 0

	config, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := dotenvValue(t, config.environment, "DORF_UID"); got != "0" {
		t.Fatalf("DORF_UID=%q want=0", got)
	}
	if got := dotenvValue(t, config.environment, "DORF_GID"); got != "0" {
		t.Fatalf("DORF_GID=%q want=0", got)
	}
}

func TestRenderSelectsStaticIncusOverlayForOneRealLocalSocket(t *testing.T) {
	spec := testSpec(t)
	socketPath := filepath.Join(t.TempDir(), "incus.socket")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	spec.Deployment.Incus = &deployment.Incus{Endpoint: "unix://" + socketPath}
	config, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !config.IncusOverlay {
		t.Fatal("local Incus did not select the static overlay")
	}
	wantFiles := spec.BaseFile + ":" + spec.IncusFile
	if got := dotenvValue(t, config.environment, "COMPOSE_FILE"); got != wantFiles {
		t.Fatalf("COMPOSE_FILE=%q want=%q", got, wantFiles)
	}
	if got := dotenvValue(t, config.environment, "DORF_LOCAL_INCUS"); got != "true" {
		t.Fatalf("DORF_LOCAL_INCUS=%q", got)
	}
	if got := dotenvValue(t, config.environment, "DORF_INCUS_SOCKET"); got != socketPath {
		t.Fatalf("DORF_INCUS_SOCKET=%q", got)
	}
	wantDigest, err := spec.Deployment.Incus.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	if got := dotenvValue(t, config.environment, "DORF_INCUS_AUTHORITY_DIGEST"); got != wantDigest {
		t.Fatalf("DORF_INCUS_AUTHORITY_DIGEST=%q want=%q", got, wantDigest)
	}

	missingOverlay := spec
	missingOverlay.IncusFile = ""
	if _, err := Render(missingOverlay); err == nil || !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("missing overlay error=%v", err)
	}
}

func TestRenderGivesInactiveProfilesCanonicalInterpolationInputs(t *testing.T) {
	spec := testSpec(t)
	spec.Deployment.E2B = nil
	config, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"COMPOSE_PROFILES":                      "",
		"DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN": "",
		"DORF_PROVIDER_GATEWAY_HOST_STATE_PATH": filepath.Join(spec.DataDir, "provider-gateway"),
		"DORF_PROVIDER_GATEWAY_DIGEST":          strings.Repeat("0", 64),
		"DORF_PROVIDER_GATEWAY_PUBLISH":         "127.0.0.1",
		"DORF_CLOUDFLARE_HOST_STATE_PATH":       filepath.Join(spec.DataDir, "provider-gateway", "cloudflare"),
		"DORF_CLOUDFLARE_DIGEST":                strings.Repeat("0", 64),
		"DORF_INCUS_AUTHORITY_DIGEST":           strings.Repeat("0", 64),
		"E2B_API_KEY":                           "",
	}
	for key, value := range want {
		if got := dotenvValue(t, config.environment, key); got != value {
			t.Errorf("%s=%q want=%q", key, got, value)
		}
	}
}

func TestMaterializeReportsOnlyGeneratedStateChangesAndLoadImageUsesEnv(t *testing.T) {
	spec := testSpec(t)
	spec.UID, spec.GID = os.Geteuid(), os.Getegid()
	config, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "compose")
	changed, err := config.Materialize(directory)
	if err != nil || !changed {
		t.Fatalf("first materialize changed=%t err=%v", changed, err)
	}
	for _, path := range []string{directory, spec.ConfigDir, spec.DataDir, spec.StateDir} {
		info, err := os.Lstat(path)
		if err != nil || !owned(info, spec.UID, spec.GID, 0o700|os.ModeDir) {
			t.Errorf("protected directory %s mode=%v err=%v", path, infoMode(info), err)
		}
	}
	changed, err = config.Materialize(directory)
	if err != nil || changed {
		t.Fatalf("replayed materialize changed=%t err=%v", changed, err)
	}
	info, err := os.Stat(filepath.Join(directory, EnvironmentFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode=%v err=%v", EnvironmentFile, info.Mode(), err)
	}
	image, err := LoadImage(directory, spec.UID, spec.GID)
	if err != nil || image != spec.Image {
		t.Fatalf("loaded image=%+v err=%v", image, err)
	}

	environmentPath := filepath.Join(directory, EnvironmentFile)
	if err := os.WriteFile(environmentPath, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err = config.Materialize(directory)
	if err != nil || !changed {
		t.Fatalf("repair materialize changed=%t err=%v", changed, err)
	}
	if _, err := LoadImage(directory, spec.UID, spec.GID); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeRejectsUnsafeHostBindSourcesBeforeWritingEnvironment(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, path string)
	}{
		{
			name: "permissive directory",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "target")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := testSpec(t)
			spec.UID, spec.GID = os.Geteuid(), os.Getegid()
			test.prepare(t, spec.DataDir)
			config, err := Render(spec)
			if err != nil {
				t.Fatal(err)
			}
			project := filepath.Join(t.TempDir(), "compose")
			if _, err := config.Materialize(project); err == nil || !strings.Contains(err.Error(), "real operator-owned") {
				t.Fatalf("unsafe bind source error=%v", err)
			}
			if _, err := os.Lstat(filepath.Join(project, EnvironmentFile)); !os.IsNotExist(err) {
				t.Fatalf("environment was written before host bind attestation: %v", err)
			}
		})
	}
}

func TestStaticComposeManifestKeepsThePublicTopologyAndCapabilityBoundary(t *testing.T) {
	document := readYAMLMap(t, filepath.Join("..", "..", SourceBaseComposeFile))
	if document["name"] != "dorf" {
		t.Fatalf("project name=%v", document["name"])
	}
	services := yamlMap(t, document, "services")
	wantServices := []string{"postgres", "migrate", "worker", "control-api", "provider-gateway", "cloudflared"}
	if len(services) != len(wantServices) {
		t.Fatalf("services=%v", mapKeys(services))
	}
	for _, name := range wantServices {
		if _, found := services[name]; !found {
			t.Errorf("missing service %q", name)
		}
	}
	if _, found := services["control-reader"]; found {
		t.Fatal("standalone control-reader survived")
	}
	postgres := serviceMap(t, services, "postgres")
	if postgres["image"] != "postgres:17.10-bookworm" {
		t.Fatalf("postgres image=%v", postgres["image"])
	}
	for _, name := range []string{"migrate", "worker", "control-api", "provider-gateway", "cloudflared"} {
		service := serviceMap(t, services, name)
		if service["image"] != "${DORF_IMAGE_REF}" || service["pull_policy"] != "${DORF_IMAGE_PULL_POLICY}" {
			t.Errorf("%s image=%v pull_policy=%v", name, service["image"], service["pull_policy"])
		}
	}
	if condition(t, serviceMap(t, services, "migrate"), "depends_on", "postgres") != "service_healthy" {
		t.Fatal("migration does not wait for healthy PostgreSQL")
	}
	if got := serviceMap(t, services, "migrate")["networks"]; !jsonEqual(got, []any{"database", "worker-egress"}) {
		t.Fatalf("migration networks=%v, want database plus bounded egress", got)
	}
	if got := postgres["networks"]; !jsonEqual(got, []any{"database", "worker-egress"}) {
		t.Fatalf("PostgreSQL networks=%v, want database plus runtime egress", got)
	}
	controlAPI := serviceMap(t, services, "control-api")
	if got := controlAPI["networks"]; !jsonEqual(got, []any{"database", "reader", "worker-egress", "ingress"}) {
		t.Fatalf("control API networks=%v, want database, reader, runtime egress, and public ingress", got)
	}
	if got := serviceMap(t, services, "provider-gateway")["networks"]; !jsonEqual(got, []any{"application", "ingress"}) {
		t.Fatalf("provider Gateway networks=%v, want application and public ingress only", got)
	}
	if got := serviceMap(t, services, "cloudflared")["networks"]; !jsonEqual(got, []any{"ingress"}) {
		t.Fatalf("cloudflared networks=%v, want public ingress only", got)
	}
	if got := controlAPI["ports"]; !jsonEqual(got, []any{"127.0.0.1:8745:8745"}) {
		t.Fatalf("control API ports=%v, want loopback-only host mapping", got)
	}
	if condition(t, serviceMap(t, services, "worker"), "depends_on", "migrate") != "service_completed_successfully" {
		t.Fatal("worker does not wait for the one-shot migration")
	}
	if condition(t, serviceMap(t, services, "control-api"), "depends_on", "worker") != "service_healthy" {
		t.Fatal("API does not wait for the worker-hosted reader")
	}
	assertHealthCommand(t, serviceMap(t, services, "worker"), "_container-control-reader-health")
	assertHealthCommand(t, serviceMap(t, services, "control-api"), "_container-control-api-health")

	workerEnvironment := yamlMap(t, serviceMap(t, services, "worker"), "environment")
	apiEnvironment := yamlMap(t, serviceMap(t, services, "control-api"), "environment")
	for _, key := range []string{"DORF_CONTROL_READER_TOKEN", "DORF_INCUS_AUTHORITY_DIGEST", "E2B_API_KEY", "DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN"} {
		if _, found := workerEnvironment[key]; !found {
			t.Errorf("worker missing %s", key)
		}
	}
	for _, forbidden := range []string{"E2B_API_KEY", "DORF_INCUS_SOCKET", "DORF_CONFIG_DIR", "DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN"} {
		if _, found := apiEnvironment[forbidden]; found {
			t.Errorf("control API received %s", forbidden)
		}
	}
	apiEncoded, err := json.Marshal(serviceMap(t, services, "control-api")["volumes"])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"control-config", "DORF_CONTROL_CONFIG_DIR", "/var/lib/dorf/.config"} {
		if strings.Contains(string(apiEncoded), forbidden) {
			t.Errorf("control API retained generated config custody %q", forbidden)
		}
	}
	if ports, found := serviceMap(t, services, "worker")["ports"]; found || ports != nil {
		t.Fatalf("worker published its private reader port: %v", ports)
	}
	for name, want := range map[string][]any{
		"provider-gateway": {"gateway", "cloudflare"},
		"cloudflared":      {"cloudflare"},
	} {
		if got := serviceMap(t, services, name)["profiles"]; !jsonEqual(got, want) {
			t.Errorf("%s profiles=%v want=%v", name, got, want)
		}
	}
	assertBindSourcesArePrecreated(t, document)
	assertPreparedRuntimeKeepsHostAbsolutePath(t, services, "provider-gateway", "DORF_PROVIDER_GATEWAY_STATE", "DORF_PROVIDER_GATEWAY_HOST_STATE_PATH")
	assertPreparedRuntimeKeepsHostAbsolutePath(t, services, "cloudflared", "DORF_CLOUDFLARE_STATE", "DORF_CLOUDFLARE_HOST_STATE_PATH")
	volumes := yamlMap(t, document, "volumes")
	if len(volumes) != 1 {
		t.Fatalf("volumes=%v", volumes)
	}
	if _, found := volumes["postgres-data"]; !found {
		t.Fatal("missing ordinary Compose-owned PostgreSQL volume")
	}
	contents, err := os.ReadFile(filepath.Join("..", "..", SourceBaseComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"network_mode: host", "/var/run/docker.sock", "external: true", "DORF_INCUS_SOCKET", "DORF_POSTGRES_IMAGE"} {
		if strings.Contains(string(contents), forbidden) {
			t.Errorf("base manifest contains forbidden authority %q", forbidden)
		}
	}
}

func TestStaticIncusOverlayAddsOnlyWorkerSocketCustody(t *testing.T) {
	document := readYAMLMap(t, filepath.Join("..", "..", SourceIncusComposeFile))
	services := yamlMap(t, document, "services")
	if len(services) != 1 {
		t.Fatalf("overlay services=%v", mapKeys(services))
	}
	worker := serviceMap(t, services, "worker")
	if len(worker) != 2 || worker["group_add"] == nil || worker["volumes"] == nil {
		t.Fatalf("overlay worker=%v", worker)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{"DORF_INCUS_SOCKET", "DORF_INCUS_SOCKET_GID"} {
		if !strings.Contains(string(encoded), wanted) {
			t.Errorf("overlay missing %s", wanted)
		}
	}
	assertBindSourcesArePrecreated(t, document)
}

func TestImageAndManifestPathsRejectAmbiguousInputs(t *testing.T) {
	for _, image := range []Image{
		{Version: "v1.2.3", Reference: "ghcr.io/aphronio/dorf:1.2.3", Pull: true},
		{Version: "1.2.3", Reference: "sha256:" + strings.Repeat("a", 64)},
		{Version: "1.2.3", Reference: "dorf image:latest"},
	} {
		if err := image.Validate(); err == nil {
			t.Errorf("Validate() accepted %+v", image)
		}
	}
	spec := testSpec(t)
	spec.BaseFile = "/opt/dorf:alternate/dorf-compose.yaml"
	if _, err := Render(spec); err == nil || !strings.Contains(err.Error(), "colon") {
		t.Fatalf("ambiguous COMPOSE_FILE error=%v", err)
	}
}

func testSpec(t *testing.T) Spec {
	t.Helper()
	root := t.TempDir()
	return Spec{
		Image:     Image{Version: "1.2.3", Reference: "ghcr.io/aphronio/dorf:1.2.3", Pull: true},
		UID:       1000,
		GID:       1000,
		ConfigDir: filepath.Join(root, "config"),
		DataDir:   filepath.Join(root, "data"),
		StateDir:  filepath.Join(root, "state"),
		BaseFile:  "/opt/dorf/" + InstalledBaseFile,
		IncusFile: "/opt/dorf/" + InstalledIncusFile,
		Deployment: deployment.Config{
			ControlReaderKey: testReaderKey,
			Database: deployment.Database{
				Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "database-secret",
			},
			E2B: &deployment.E2B{APIKey: "e2b-secret"},
		},
	}
}

func dotenvValue(t *testing.T, environment []byte, key string) string {
	t.Helper()
	prefix := key + "='"
	for _, line := range strings.Split(string(environment), "\n") {
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, "'") {
			return strings.TrimSuffix(strings.TrimPrefix(line, prefix), "'")
		}
	}
	t.Fatalf("environment is missing %s:\n%s", key, environment)
	return ""
}

func readYAMLMap(t *testing.T, path string) map[string]any {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return document
}

func yamlMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s=%T %v, want map", key, parent[key], parent[key])
	}
	return value
}

func serviceMap(t *testing.T, services map[string]any, name string) map[string]any {
	t.Helper()
	return yamlMap(t, services, name)
}

func condition(t *testing.T, service map[string]any, section, dependency string) string {
	t.Helper()
	dependencies := yamlMap(t, service, section)
	entry := yamlMap(t, dependencies, dependency)
	value, _ := entry["condition"].(string)
	return value
}

func assertHealthCommand(t *testing.T, service map[string]any, command string) {
	t.Helper()
	health := yamlMap(t, service, "healthcheck")
	test, ok := health["test"].([]any)
	if !ok {
		t.Fatalf("health test=%T %v", health["test"], health["test"])
	}
	found := false
	for _, argument := range test {
		found = found || argument == command
	}
	if !found {
		t.Errorf("health test=%v missing %s", test, command)
	}
}

func assertBindSourcesArePrecreated(t *testing.T, document map[string]any) {
	t.Helper()
	for serviceName, value := range yamlMap(t, document, "services") {
		service, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("service %s=%T, want map", serviceName, value)
		}
		volumes, _ := service["volumes"].([]any)
		for index, value := range volumes {
			volume, ok := value.(map[string]any)
			if !ok || volume["type"] != "bind" {
				continue
			}
			bind := yamlMap(t, volume, "bind")
			if create, ok := bind["create_host_path"].(bool); !ok || create {
				t.Errorf("%s bind %d create_host_path=%v, want false", serviceName, index, bind["create_host_path"])
			}
		}
	}
}

func assertPreparedRuntimeKeepsHostAbsolutePath(t *testing.T, services map[string]any, serviceName, stateKey, pathKey string) {
	t.Helper()
	service := serviceMap(t, services, serviceName)
	want := "${" + pathKey + "}"
	if got := yamlMap(t, service, "environment")[stateKey]; got != want {
		t.Errorf("%s %s=%v want=%q", serviceName, stateKey, got, want)
	}
	volumes, ok := service["volumes"].([]any)
	if !ok || len(volumes) != 1 {
		t.Fatalf("%s volumes=%T %v, want one bind", serviceName, service["volumes"], service["volumes"])
	}
	volume, ok := volumes[0].(map[string]any)
	if !ok || volume["type"] != "bind" {
		t.Fatalf("%s volume=%T %v, want bind", serviceName, volumes[0], volumes[0])
	}
	if source, target := volume["source"], volume["target"]; source != want || target != source {
		t.Errorf("%s bind source=%v target=%v want=%q", serviceName, source, target, want)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode()
}

func mapKeys(values map[string]any) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	return result
}

func jsonEqual(first, second any) bool {
	left, leftErr := json.Marshal(first)
	right, rightErr := json.Marshal(second)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func TestLoadImageRejectsSymlinkedEnvironment(t *testing.T) {
	spec := testSpec(t)
	spec.UID, spec.GID = os.Geteuid(), os.Getegid()
	config, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "compose")
	if _, err := config.Materialize(directory); err != nil {
		t.Fatal(err)
	}
	environmentPath := filepath.Join(directory, EnvironmentFile)
	if err := os.Remove(environmentPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.env")
	if err := os.WriteFile(outside, config.environment, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, environmentPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImage(directory, spec.UID, spec.GID); err == nil {
		t.Fatal("LoadImage accepted a symlinked environment")
	}
	if contents, err := os.ReadFile(outside); err != nil || !strings.Contains(string(contents), "DORF_IMAGE_REF") {
		t.Fatalf("outside environment changed: %q err=%v", contents, err)
	}
}
