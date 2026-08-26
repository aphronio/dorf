package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	cloudflareapp "github.com/aphronio/dorf/internal/cloudflare"
	"github.com/aphronio/dorf/internal/composeconfig"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/dockerexec"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/version"
	bootstraphelper "github.com/aphronio/dorf/scripts/bootstrap"
)

const (
	controlDiscoveryURL     = "http://127.0.0.1:8745/v1"
	deploymentGuideURL      = "https://github.com/aphronio/dorf/blob/main/docs/getting-started.md"
	deploymentProbeLimit    = 64 << 10
	deploymentProbeTimeout  = 5 * time.Second
	dockerEngineProbeLimit  = 16 << 10
	dockerEngineProbePeriod = 30 * time.Second
)

var errDeploymentSetupHandoff = errors.New("Dorf deployment configuration requires an operator handoff")

// deploymentSetupHandoffError marks normal resumable setup. Dorf has committed
// its configuration authority, but an operator still needs to apply the
// static deployment manifests or make their running version current.
type deploymentSetupHandoffError struct {
	ProjectDir string
	Changed    bool
	Detail     string
}

func (err deploymentSetupHandoffError) Error() string {
	return fmt.Sprintf("Dorf deployment configuration at %s awaits operator deployment; see %s", err.ProjectDir, deploymentGuideURL)
}

func (err deploymentSetupHandoffError) Is(target error) bool {
	return target == errDeploymentSetupHandoff
}

// containerForegroundCommand is intentionally hidden from the human CLI. The
// static manifests are its only caller and supply exact mounted state paths.
func containerForegroundCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) == 0 || (args[0] != "_container-provider-gateway" && args[0] != "_container-cloudflared" && args[0] != "_container-control-reader-health" && args[0] != "_container-control-api-health") {
		return false, nil
	}
	if len(args) != 1 {
		return true, fmt.Errorf("%s does not accept arguments", args[0])
	}
	serviceCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "_container-control-api-health":
		ready, detail := controlAPIReady(ctx, version.Version, localControlHTTPClient())
		if !ready {
			return true, fmt.Errorf("control API discovery is not ready: %s", detail)
		}
		return true, nil
	case "_container-control-reader-health":
		client, err := controlreader.NewClient("http://127.0.0.1:8756", strings.TrimSpace(os.Getenv("DORF_CONTROL_READER_TOKEN")), nil)
		if err != nil {
			return true, err
		}
		healthCtx, cancel := context.WithTimeout(ctx, deploymentProbeTimeout)
		defer cancel()
		return true, client.Health(healthCtx)
	case "_container-provider-gateway":
		state := strings.TrimSpace(os.Getenv("DORF_PROVIDER_GATEWAY_STATE"))
		return true, (gateway.Gateway{StatePath: state}).RunForeground(serviceCtx, stdout, stderr)
	default:
		state := strings.TrimSpace(os.Getenv("DORF_CLOUDFLARE_STATE"))
		return true, (cloudflareapp.Tunnel{StatePath: state}).RunForeground(serviceCtx, stdout, stderr)
	}
}

// checkDockerEngine is the only Docker process Dorf invokes. It proves the
// protected local daemon for the administrator-helper UX; deployment
// lifecycle remains entirely outside the Go program.
func checkDockerEngine(ctx context.Context) error {
	return checkDockerEngineWith(ctx, dockerexec.Resolve, dockerInfoOutput)
}

func checkDockerEngineWith(
	ctx context.Context,
	resolve func() (string, error),
	output func(context.Context, string) (string, error),
) error {
	executable, err := resolve()
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, dockerEngineProbePeriod)
	defer cancel()
	server, err := output(probeCtx, executable)
	if err != nil {
		return fmt.Errorf("Docker Engine is unavailable: %w", err)
	}
	if strings.TrimSpace(server) == "" {
		return fmt.Errorf("Docker Engine readiness returned no server version")
	}
	return nil
}

func dockerInfoOutput(ctx context.Context, executable string) (string, error) {
	environment, err := dockerCommandEnvironment(os.Environ())
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, executable, "info", "--format", "{{.ServerVersion}}")
	command.Env = environment
	output, err := command.CombinedOutput()
	if len(output) > dockerEngineProbeLimit {
		output = output[:dockerEngineProbeLimit]
	}
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, detail)
	}
	return string(output), nil
}

func dockerCommandEnvironment(environment []string) ([]string, error) {
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
			return nil, fmt.Errorf("Dorf requires a local absolute unix:// Docker endpoint")
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

type deploymentConfigurationSource struct {
	Paths      config.Paths
	Deployment deployment.Config
	BaseFile   string
	IncusFile  string
	UID        int
	GID        int
	Gateway    *composeconfig.OptionalService
	Cloudflare *composeconfig.OptionalService
}

// prepareSetupDeployment writes only the protected inputs consumed by the
// installed static manifests. prepareOptional refreshes already-authorized
// Gateway and Tunnel runtime state before projecting it.
func prepareSetupDeployment(ctx context.Context, localImage string, prepareOptional bool, output io.Writer) (composeconfig.Image, error) {
	source, err := currentDeploymentConfigurationSource(ctx, prepareOptional)
	if err != nil {
		return composeconfig.Image{}, err
	}
	image, err := selectDeploymentImage(source.Paths.ComposeDir, source.UID, source.GID, version.Version, localImage)
	if err != nil {
		return composeconfig.Image{}, err
	}
	return materializeDeploymentConfiguration(ctx, source, image, localControlHTTPClient(), output)
}

// refreshExistingDeploymentConfig keeps an installed same-version image choice
// while projecting newly prepared optional state. It never controls the
// deployment lifecycle.
func refreshExistingDeploymentConfig(ctx context.Context, output io.Writer) error {
	_, err := prepareSetupDeployment(ctx, "", true, output)
	return err
}

func currentDeploymentConfigurationSource(ctx context.Context, prepareOptional bool) (deploymentConfigurationSource, error) {
	paths, err := config.CurrentOperatorPaths()
	if err != nil {
		return deploymentConfigurationSource{}, err
	}
	stored, found, err := deployment.Load(filepath.Join(paths.ConfigDir, "deployment.json"))
	if err != nil {
		return deploymentConfigurationSource{}, err
	}
	if !found {
		return deploymentConfigurationSource{}, fmt.Errorf("Dorf deployment is not configured; run dorf setup")
	}
	binary, err := currentDorfBinary()
	if err != nil {
		return deploymentConfigurationSource{}, err
	}
	baseFile, incusFile, err := resolveDeploymentManifests(binary)
	if err != nil {
		return deploymentConfigurationSource{}, err
	}
	source := deploymentConfigurationSource{
		Paths: paths, Deployment: stored, BaseFile: baseFile, IncusFile: incusFile,
		UID: os.Geteuid(), GID: os.Getegid(),
	}
	if prepareOptional {
		if err := prepareOptionalDeploymentRuntime(ctx, paths.DataDir); err != nil {
			return deploymentConfigurationSource{}, err
		}
	}
	return deploymentConfigurationSourceWithOptionalState(source)
}

func deploymentConfigurationSourceWithOptionalState(source deploymentConfigurationSource) (deploymentConfigurationSource, error) {
	gatewayStatePath := filepath.Join(source.Paths.DataDir, "provider-gateway")
	preparedGateway, gatewayDesired, err := (gateway.Gateway{StatePath: gatewayStatePath}).ComposeState()
	if err != nil {
		return deploymentConfigurationSource{}, err
	}
	if !gatewayDesired {
		return source, nil
	}
	source.Gateway = &composeconfig.OptionalService{
		StatePath: preparedGateway.StatePath, Digest: preparedGateway.Digest, PublishAddress: preparedGateway.PublishAddress,
	}
	preparedCloudflare, cloudflareDesired, err := (cloudflareapp.Tunnel{StatePath: filepath.Join(gatewayStatePath, "cloudflare")}).ComposeState()
	if err != nil {
		return deploymentConfigurationSource{}, err
	}
	if cloudflareDesired {
		source.Cloudflare = &composeconfig.OptionalService{StatePath: preparedCloudflare.StatePath, Digest: preparedCloudflare.Digest}
	}
	return source, nil
}

func prepareOptionalDeploymentRuntime(ctx context.Context, dataDir string) error {
	gatewayStatePath := filepath.Join(dataDir, "provider-gateway")
	g := gateway.Gateway{StatePath: gatewayStatePath}
	address, found, err := g.PreparedComposePublishAddress()
	if err != nil {
		return err
	}
	if found {
		if err := g.PrepareContainer(ctx, address); err != nil {
			return fmt.Errorf("prepare current Provider Gateway runtime: %w", err)
		}
	}
	tunnel := cloudflareapp.Tunnel{StatePath: filepath.Join(gatewayStatePath, "cloudflare")}
	if _, err := tunnel.PrepareRuntimeBinary(ctx); err != nil {
		return fmt.Errorf("prepare current cloudflared runtime: %w", err)
	}
	return nil
}

func materializeDeploymentConfiguration(
	ctx context.Context,
	source deploymentConfigurationSource,
	image composeconfig.Image,
	client *http.Client,
	output io.Writer,
) (composeconfig.Image, error) {
	rendered, err := composeconfig.Render(composeconfig.Spec{
		Image: image, UID: source.UID, GID: source.GID,
		ConfigDir: source.Paths.ConfigDir, DataDir: source.Paths.DataDir, StateDir: source.Paths.StateDir,
		BaseFile: source.BaseFile, IncusFile: source.IncusFile, Deployment: source.Deployment,
		Gateway: source.Gateway, Cloudflare: source.Cloudflare,
	})
	if err != nil {
		return composeconfig.Image{}, err
	}
	changed, err := rendered.Materialize(source.Paths.ComposeDir)
	if err != nil {
		return composeconfig.Image{}, err
	}
	if changed {
		return image, deploymentHandoff(source.Paths.ComposeDir, true, "configuration changed", output)
	}
	ready, detail := controlAPIReady(ctx, image.Version, client)
	if !ready {
		return image, deploymentHandoff(source.Paths.ComposeDir, false, detail, output)
	}
	return image, nil
}

func deploymentHandoff(projectDir string, changed bool, detail string, output io.Writer) error {
	fmt.Fprintf(output, "\nDorf deployment configuration is ready: %s\n", projectDir)
	fmt.Fprintf(output, "Continue with the deployment guide: %s\n", deploymentGuideURL)
	return deploymentSetupHandoffError{ProjectDir: projectDir, Changed: changed, Detail: detail}
}

func selectDeploymentImage(projectDir string, uid, gid int, releaseVersion, localImage string) (composeconfig.Image, error) {
	localImage = strings.TrimSpace(localImage)
	if localImage != "" {
		image := composeconfig.Image{Version: releaseVersion, Reference: localImage}
		return image, image.Validate()
	}
	environmentPath := filepath.Join(projectDir, composeconfig.EnvironmentFile)
	if _, err := os.Lstat(environmentPath); err == nil {
		installed, err := composeconfig.LoadImage(projectDir, uid, gid)
		if err != nil {
			return composeconfig.Image{}, err
		}
		if installed.Version == releaseVersion {
			return installed, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return composeconfig.Image{}, fmt.Errorf("inspect installed deployment image choice: %w", err)
	}
	image := composeconfig.Image{
		Version: releaseVersion, Reference: "ghcr.io/aphronio/dorf:" + releaseVersion, Pull: true,
	}
	return image, image.Validate()
}

func currentDorfBinary() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running Dorf executable: %w", err)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", fmt.Errorf("resolve running Dorf executable: %w", err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return "", fmt.Errorf("resolve running Dorf executable: %w", err)
	}
	return filepath.Clean(binary), nil
}

func resolveDeploymentManifests(binary string) (string, string, error) {
	directory := filepath.Dir(binary)
	installedBase := filepath.Join(directory, composeconfig.InstalledBaseFile)
	installedIncus := filepath.Join(directory, composeconfig.InstalledIncusFile)
	basePresent, baseErr := regularDeploymentManifest(installedBase)
	incusPresent, incusErr := regularDeploymentManifest(installedIncus)
	if baseErr != nil {
		return "", "", baseErr
	}
	if incusErr != nil {
		return "", "", incusErr
	}
	if basePresent && incusPresent {
		return installedBase, installedIncus, nil
	}
	if basePresent != incusPresent {
		return "", "", fmt.Errorf("installed Dorf deployment manifests beside %s are incomplete", binary)
	}
	if filepath.Base(binary) == "dorf" && filepath.Base(directory) == "bin" && filepath.Base(filepath.Dir(directory)) == ".dorf" {
		repository := filepath.Clean(filepath.Join(directory, "..", ".."))
		developmentBase := filepath.Join(repository, filepath.FromSlash(composeconfig.SourceBaseComposeFile))
		developmentIncus := filepath.Join(repository, filepath.FromSlash(composeconfig.SourceIncusComposeFile))
		basePresent, baseErr = regularDeploymentManifest(developmentBase)
		incusPresent, incusErr = regularDeploymentManifest(developmentIncus)
		if baseErr != nil {
			return "", "", baseErr
		}
		if incusErr != nil {
			return "", "", incusErr
		}
		if basePresent && incusPresent {
			return developmentBase, developmentIncus, nil
		}
	}
	return "", "", fmt.Errorf("Dorf deployment manifests are missing beside %s", binary)
}

func regularDeploymentManifest(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Dorf deployment manifest %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("Dorf deployment manifest %s must be one regular file", path)
	}
	return true, nil
}

func localControlHTTPClient() *http.Client {
	return &http.Client{
		Timeout: deploymentProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func controlAPIReady(ctx context.Context, releaseVersion string, client *http.Client) (bool, string) {
	if client == nil {
		return false, "control API discovery is unavailable"
	}
	probeCtx, cancel := context.WithTimeout(ctx, deploymentProbeTimeout)
	defer cancel()
	request, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, controlDiscoveryURL, nil)
	response, err := client.Do(request)
	if err != nil {
		return false, "control API discovery is unavailable"
	}
	if response.Body == nil {
		return false, "control API discovery is unavailable"
	}
	defer response.Body.Close()
	var discovery struct {
		Product string `json:"product"`
		Version string `json:"version"`
	}
	contents, readErr := io.ReadAll(io.LimitReader(response.Body, deploymentProbeLimit+1))
	if response.StatusCode != http.StatusOK || readErr != nil || len(contents) > deploymentProbeLimit || json.Unmarshal(contents, &discovery) != nil || discovery.Product != "dorf" || discovery.Version != releaseVersion {
		return false, "control API discovery does not match Dorf " + releaseVersion
	}
	return true, "ready"
}

type setupBootstrapKind = bootstraphelper.Name

const (
	bootstrapDocker setupBootstrapKind = bootstraphelper.Docker
	bootstrapIncus  setupBootstrapKind = bootstraphelper.Incus
)

func setupBootstrapHandoff(kind setupBootstrapKind, cause error, output io.Writer) error {
	euid := os.Geteuid()
	account, err := user.LookupId(strconv.Itoa(euid))
	if err != nil {
		return fmt.Errorf("resolve current Dorf operator for %s handoff: %w", kind, err)
	}
	paths, err := config.CurrentOperatorPaths()
	if err != nil {
		return err
	}
	if err := ensureSetupDataRoot(paths.DataDir); err != nil {
		return err
	}
	artifact, err := bootstraphelper.Materialize(paths.DataDir, version.Version, kind)
	if err != nil {
		return fmt.Errorf("materialize exact %s bootstrap helper: %w", kind, err)
	}
	arguments := []string{artifact.Path, "--user", account.Username}
	if euid != 0 {
		arguments = append([]string{"sudo", "--"}, arguments...)
	}
	manual := []string{}
	warning := ""
	switch kind {
	case bootstraphelper.Docker:
		arguments = append(arguments, "--acknowledge-docker-root-authority", "--acknowledge-firewall-impact")
		warning = "Docker daemon access is root-equivalent and Docker changes host firewall and forwarding behavior."
		manual = []string{"https://docs.docker.com/engine/install/ubuntu/", "https://docs.docker.com/compose/install/linux/"}
	case bootstraphelper.Incus:
		arguments = append(arguments, "--acknowledge-incus-root-authority", "--acknowledge-kvm-device-access", "--initialize-pristine")
		warning = "incus-admin is root-equivalent and the kvm group grants virtualization-device access."
		manual = []string{"https://linuxcontainers.org/incus/docs/main/installing/"}
	default:
		return fmt.Errorf("unsupported setup bootstrap handoff %q", kind)
	}
	fmt.Fprintf(output, "\n%s needs administrator preparation; Dorf did not run sudo or the helper.\n", strings.ToUpper(string(kind[:1]))+string(kind[1:]))
	fmt.Fprintln(output, warning)
	fmt.Fprintln(output, "Inspect the materialized helper, then run this exact command explicitly:")
	fmt.Fprintln(output, "  "+shellJoin(arguments))
	fmt.Fprintln(output, "Equivalent upstream instructions:")
	for _, link := range manual {
		fmt.Fprintln(output, "  "+link)
	}
	return fmt.Errorf("%s is not ready; complete the administrator handoff and rerun dorf setup: %w", kind, cause)
}

func setupIncusReadinessHandoff(authority *deployment.Incus, cause error, output io.Writer) error {
	if authority != nil && authority.Endpoint == "unix://"+incus.DefaultUnixSocket {
		return setupBootstrapHandoff(bootstrapIncus, cause, output)
	}
	if authority != nil && strings.HasPrefix(authority.Endpoint, "https://") {
		return fmt.Errorf("remote Incus endpoint %s is not supported until its live proof gate passes; select another Sandbox provider: %w", authority.Endpoint, cause)
	}
	endpoint := "unconfigured"
	if authority != nil {
		endpoint = authority.Endpoint
	}
	return fmt.Errorf("configured Incus endpoint %s is not ready: %w; repair that endpoint or select another Sandbox provider", endpoint, cause)
}

func ensureSetupDataRoot(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create protected Dorf data directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	owner, owned := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !owned || int(owner.Uid) != os.Geteuid() || int(owner.Gid) != os.Getegid() {
		return fmt.Errorf("Dorf data directory %s must be one real operator-owned directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("protect Dorf data directory: %w", err)
	}
	return nil
}

func shellJoin(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = "'" + strings.ReplaceAll(argument, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}
