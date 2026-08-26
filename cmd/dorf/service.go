package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	cloudflareapp "github.com/aphronio/dorf/internal/cloudflare"
	"github.com/aphronio/dorf/internal/composeproject"
	"github.com/aphronio/dorf/internal/composeservice"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
	releaseapp "github.com/aphronio/dorf/internal/release"
	"github.com/aphronio/dorf/internal/version"
	bootstraphelper "github.com/aphronio/dorf/scripts/bootstrap"
)

const (
	defaultComposeServiceLogLines = 200
	composeServiceReadyTimeout    = 30 * time.Second
	composeServiceReadyPoll       = 250 * time.Millisecond
)

var errComposeServiceReconcileCancelled = errors.New("Compose service reconciliation cancelled")

// composeForegroundCommand is intentionally hidden from the human CLI. The
// generated Compose project is its only caller and supplies one exact mounted
// state path; these commands never open PostgreSQL or supervise themselves.
func composeForegroundCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) == 0 || (args[0] != "_compose-provider-gateway" && args[0] != "_compose-cloudflared" && args[0] != "_compose-control-reader-health") {
		return false, nil
	}
	if len(args) != 1 {
		return true, fmt.Errorf("%s does not accept arguments", args[0])
	}
	serviceCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "_compose-control-reader-health":
		client, err := controlreader.NewClient("http://127.0.0.1:8756", strings.TrimSpace(os.Getenv("DORF_CONTROL_READER_TOKEN")), nil)
		if err != nil {
			return true, err
		}
		healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return true, client.Health(healthCtx)
	case "_compose-provider-gateway":
		state := strings.TrimSpace(os.Getenv("DORF_PROVIDER_GATEWAY_STATE"))
		return true, (gateway.Gateway{StatePath: state}).RunForeground(serviceCtx, stdout, stderr)
	default:
		state := strings.TrimSpace(os.Getenv("DORF_CLOUDFLARE_STATE"))
		return true, (cloudflareapp.Tunnel{StatePath: state}).RunForeground(serviceCtx, stdout, stderr)
	}
}

type composeServiceReconcileOptions struct {
	Yes        bool
	Existing   bool
	LocalImage string
}

// composeServiceRootCommand keeps deployment operations available when the
// database, API, or worker is unavailable.
func composeServiceRootCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "service" {
		return false, nil
	}
	return true, serviceCommand(ctx, args[1:], stdout, stderr)
}

func serviceCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		writeComposeServiceUsage(stderr)
		return fmt.Errorf("service requires: reconcile, status, restart, or logs")
	}
	if args[0] == "-h" || args[0] == "--help" {
		writeComposeServiceUsage(stderr)
		return flag.ErrHelp
	}
	if serviceSubcommandHelp(args, stderr) {
		return flag.ErrHelp
	}

	manager := currentComposeServiceManager()
	switch args[0] {
	case "reconcile":
		options, err := parseComposeServiceReconcileOptions(args[1:], stderr)
		if err != nil {
			return err
		}
		if options.Existing {
			projectDir, err := currentComposeProjectDir()
			if err != nil {
				return err
			}
			installed, err := existingComposeDeployment(projectDir)
			if err != nil {
				return err
			}
			if !installed {
				fmt.Fprintln(stdout, "Dorf Compose deployment is not installed; nothing to reconcile")
				return nil
			}
		}
		if err := requireOrdinaryComposeOperator(); err != nil {
			return err
		}
		source, err := currentComposeDeploymentBaseSource()
		if err != nil {
			return err
		}
		return reconcileComposeServices(ctx, manager, source, options, stdout, stderr)
	case "status", "restart", "logs":
		spec, err := currentComposeServiceSpec()
		if err != nil {
			return err
		}
		switch args[0] {
		case "status":
			return composeServiceStatusCommand(ctx, manager, spec, args[1:], stdout, stderr)
		case "restart":
			return composeServiceRestartCommand(ctx, manager, spec, args[1:], stdout, stderr)
		default:
			return composeServiceLogsCommand(ctx, manager, spec, args[1:], stdout, stderr)
		}
	default:
		return fmt.Errorf("service requires: reconcile, status, restart, or logs")
	}
}

func serviceSubcommandHelp(args []string, output io.Writer) bool {
	if len(args) < 2 || !containsServiceHelpFlag(args[1:]) {
		return false
	}
	switch args[0] {
	case "reconcile":
		fmt.Fprintln(output, "usage: dorf service reconcile [--yes] [--existing] [--local-image REF]")
	case "status":
		fmt.Fprintln(output, "usage: dorf service status [--output human|json]")
	case "restart":
		fmt.Fprintln(output, "usage: dorf service restart <api|worker|gateway|cloudflare|all>")
	case "logs":
		fmt.Fprintln(output, "usage: dorf service logs <api|worker|gateway|cloudflare> [--lines N]")
	default:
		return false
	}
	return true
}

func containsServiceHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func currentComposeServiceManager() composeservice.Manager {
	return composeservice.Manager{}
}

type composeDeploymentSource struct {
	Version    string
	BinaryPath string
	ProjectDir string
	ImageCache string
	Project    composeproject.Spec
}

// currentComposeDeploymentSource derives every non-image deployment fact from the
// running binary and protected deployment record. Image authority is resolved
// separately so reconcile can keep acquisition behind its single approval.
func currentComposeDeploymentSource() (composeDeploymentSource, error) {
	source, err := currentComposeDeploymentBaseSource()
	if err != nil {
		return composeDeploymentSource{}, err
	}
	return composeDeploymentSourceWithOptionalState(source)
}

func currentComposeDeploymentBaseSource() (composeDeploymentSource, error) {
	paths, err := config.CurrentOperatorPaths()
	if err != nil {
		return composeDeploymentSource{}, err
	}
	uid, gid := os.Geteuid(), os.Getegid()
	binary, err := currentDorfBinary()
	if err != nil {
		return composeDeploymentSource{}, err
	}
	stored, found, err := deployment.Load(filepath.Join(paths.ConfigDir, "deployment.json"))
	if err != nil {
		return composeDeploymentSource{}, err
	}
	if !found {
		return composeDeploymentSource{}, fmt.Errorf("Dorf deployment is not configured; run dorf setup")
	}
	return composeDeploymentSource{
		Version: version.Version, BinaryPath: binary,
		ProjectDir: paths.ComposeDir,
		ImageCache: filepath.Join(paths.ComposeDir, "image-cache"),
		Project: composeproject.Spec{
			UID: uid, GID: gid, ConfigDir: paths.ConfigDir, DataDir: paths.DataDir, StateDir: paths.StateDir, Deployment: stored,
		},
	}, nil
}

func composeDeploymentSourceWithOptionalState(source composeDeploymentSource) (composeDeploymentSource, error) {
	gatewayStatePath := filepath.Join(source.Project.DataDir, "provider-gateway")
	preparedGateway, gatewayDesired, err := (gateway.Gateway{StatePath: gatewayStatePath}).ComposeState()
	if err != nil {
		return composeDeploymentSource{}, err
	}
	if gatewayDesired {
		source.Project.Gateway = &composeproject.OptionalService{StatePath: preparedGateway.StatePath, Digest: preparedGateway.Digest, PublishAddress: preparedGateway.PublishAddress}
		preparedCloudflare, cloudflareDesired, err := (cloudflareapp.Tunnel{StatePath: filepath.Join(gatewayStatePath, "cloudflare")}).ComposeState()
		if err != nil {
			return composeDeploymentSource{}, err
		}
		if cloudflareDesired {
			source.Project.Cloudflare = &composeproject.OptionalService{StatePath: preparedCloudflare.StatePath, Digest: preparedCloudflare.Digest}
		}
	}
	return source, nil
}

// prepareComposeOptionalRuntime is called only from an already authorized
// setup/reconcile mutation. It upgrades pinned sibling executables without
// performing provider login, Cloudflare browser/DNS work, or profile proof.
func prepareComposeOptionalRuntime(ctx context.Context, source composeDeploymentSource) (composeDeploymentSource, error) {
	gatewayStatePath := filepath.Join(source.Project.DataDir, "provider-gateway")
	g := gateway.Gateway{StatePath: gatewayStatePath}
	address, found, err := g.PreparedComposePublishAddress()
	if err != nil {
		return composeDeploymentSource{}, err
	}
	if found {
		if err := g.PrepareContainer(ctx, address); err != nil {
			return composeDeploymentSource{}, fmt.Errorf("prepare current Provider Gateway runtime: %w", err)
		}
	}
	tunnel := cloudflareapp.Tunnel{StatePath: filepath.Join(gatewayStatePath, "cloudflare")}
	if _, err := tunnel.PrepareRuntimeBinary(ctx); err != nil {
		return composeDeploymentSource{}, fmt.Errorf("prepare current cloudflared runtime: %w", err)
	}
	return composeDeploymentSourceWithOptionalState(source)
}

func (source composeDeploymentSource) render(image releaseapp.ComposeImage) (composeservice.Spec, error) {
	renderSpec := source.Project
	renderSpec.Image = image
	project, err := composeproject.Render(renderSpec)
	if err != nil {
		return composeservice.Spec{}, err
	}
	spec := composeservice.Spec{
		ProjectDir: source.ProjectDir,
		Project:    project,
	}
	return spec, nil
}

// currentComposeServiceSpec is intentionally offline. It reconstructs the
// exact desired project from the protected receipt and current binary; only
// reconcile may acquire or load another image.
func currentComposeServiceSpec() (composeservice.Spec, error) {
	source, err := currentComposeDeploymentSource()
	if err != nil {
		return composeservice.Spec{}, err
	}
	image, err := composeproject.LoadImageAuthority(source.ProjectDir, source.Version, source.BinaryPath, source.Project.UID, source.Project.GID)
	if err != nil {
		return composeservice.Spec{}, err
	}
	return source.render(image)
}

func currentComposeProjectDir() (string, error) {
	paths, err := config.CurrentOperatorPaths()
	if err != nil {
		return "", err
	}
	return paths.ComposeDir, nil
}

func requireOrdinaryComposeOperator() error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("Dorf Compose reconciliation must run as the ordinary operator, not root")
	}
	return nil
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

type composeServiceStatuser interface {
	Status(context.Context, composeservice.Spec) (composeservice.Status, error)
}

func composeServiceStatusCommand(ctx context.Context, manager composeServiceStatuser, spec composeservice.Spec, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("service status", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("service status does not accept positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	status, err := manager.Status(ctx, spec)
	if err != nil {
		return err
	}
	if *output == "json" {
		if err := writeJSON(stdout, status); err != nil {
			return err
		}
	} else {
		renderComposeDeploymentStatus(stdout, status)
	}
	if !status.Ready {
		return fmt.Errorf("Dorf Compose services are not ready")
	}
	return nil
}

type composeServiceRestarter interface {
	Restart(context.Context, composeservice.Spec, composeservice.Target, io.Writer, io.Writer) error
}

func composeServiceRestartCommand(ctx context.Context, manager composeServiceRestarter, spec composeservice.Spec, args []string, stdout, stderr io.Writer) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "usage: dorf service restart <api|worker|gateway|cloudflare|all>")
		return flag.ErrHelp
	}
	if len(args) != 1 {
		return fmt.Errorf("service restart requires api, worker, gateway, cloudflare, or all")
	}
	target, err := composeServiceTarget(args[0], true)
	if err != nil {
		return err
	}
	if err := manager.Restart(ctx, spec, target, stdout, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restarted Dorf Compose target: %s\n", target)
	return nil
}

type composeServiceLogger interface {
	Logs(context.Context, composeservice.Spec, composeservice.Target, int, io.Writer, io.Writer) error
}

func composeServiceLogsCommand(ctx context.Context, manager composeServiceLogger, spec composeservice.Spec, args []string, stdout, stderr io.Writer) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "usage: dorf service logs <api|worker|gateway|cloudflare> [--lines N]")
		return flag.ErrHelp
	}
	if len(args) == 0 {
		return fmt.Errorf("service logs requires api, worker, gateway, or cloudflare")
	}
	target, err := composeServiceTarget(args[0], false)
	if err != nil {
		return err
	}
	set := flag.NewFlagSet("service logs "+args[0], flag.ContinueOnError)
	set.SetOutput(stderr)
	lines := set.Int("lines", defaultComposeServiceLogLines, "maximum log lines (1-10000)")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("service logs does not accept positional arguments after the target")
	}
	if *lines < 1 || *lines > 10_000 {
		return fmt.Errorf("service log lines must be between 1 and 10000")
	}
	return manager.Logs(ctx, spec, target, *lines, stdout, stderr)
}

func composeServiceTarget(raw string, allowAll bool) (composeservice.Target, error) {
	switch raw {
	case string(composeservice.TargetAPI):
		return composeservice.TargetAPI, nil
	case string(composeservice.TargetWorker):
		return composeservice.TargetWorker, nil
	case string(composeservice.TargetGateway):
		return composeservice.TargetGateway, nil
	case string(composeservice.TargetCloudflare):
		return composeservice.TargetCloudflare, nil
	case string(composeservice.TargetAll):
		if allowAll {
			return composeservice.TargetAll, nil
		}
	}
	expected := "api, worker, gateway, or cloudflare"
	if allowAll {
		expected += ", or all"
	}
	return "", fmt.Errorf("service target must be %s", expected)
}

func parseComposeServiceReconcileOptions(args []string, stderr io.Writer) (composeServiceReconcileOptions, error) {
	set := flag.NewFlagSet("service reconcile", flag.ContinueOnError)
	set.SetOutput(stderr)
	yes := set.Bool("yes", false, "approve the Compose deployment plan shown")
	existing := set.Bool("existing", false, "reconcile only when a Compose deployment already exists")
	localImage := set.String("local-image", "", "trust one already-loaded exact contributor/integration image reference")
	if err := set.Parse(args); err != nil {
		return composeServiceReconcileOptions{}, err
	}
	if set.NArg() != 0 {
		return composeServiceReconcileOptions{}, fmt.Errorf("service reconcile does not accept positional arguments")
	}
	return composeServiceReconcileOptions{Yes: *yes, Existing: *existing, LocalImage: strings.TrimSpace(*localImage)}, nil
}

// existingComposeDeployment is the updater's compatibility gate.
// Only a genuinely absent canonical directory makes --existing a no-op. Any
// real directory, including a partial project, is installed state for Apply to
// repair. A link or non-directory cannot establish custody.
func existingComposeDeployment(projectDir string) (bool, error) {
	info, err := os.Lstat(projectDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Dorf Compose project %s: %w", projectDir, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("Dorf Compose project %s must be one real directory", projectDir)
	}
	return true, nil
}

type composeServiceReconciler interface {
	composeServiceStatuser
	Apply(context.Context, composeservice.Spec, io.Writer, io.Writer) error
}

type composeServiceReconcilePlan struct {
	Summaries []string
	Resolve   func(context.Context) (composeservice.Spec, error)
}

func reconcileComposeServices(ctx context.Context, manager composeServiceReconciler, source composeDeploymentSource, options composeServiceReconcileOptions, stdout, stderr io.Writer) error {
	plan := composeServiceReconcilePlan{Summaries: composeServiceApplySummaries(source, options)}
	plan.Resolve = func(ctx context.Context) (composeservice.Spec, error) {
		prepared, err := prepareComposeOptionalRuntime(ctx, source)
		if err != nil {
			return composeservice.Spec{}, err
		}
		var image releaseapp.ComposeImage
		if options.LocalImage != "" {
			image, err = releaseapp.AttestLocalComposeImage(ctx, prepared.Version, prepared.BinaryPath, options.LocalImage)
		} else {
			image, err = releaseapp.AcquireComposeImage(ctx, prepared.Version, prepared.BinaryPath, prepared.ImageCache)
		}
		if err != nil {
			return composeservice.Spec{}, err
		}
		return prepared.render(image)
	}
	return reconcileComposeServicesWith(ctx, manager, plan, options, stdout, stderr)
}

func reconcileComposeServicesWith(ctx context.Context, manager composeServiceReconciler, plan composeServiceReconcilePlan, options composeServiceReconcileOptions, stdout, stderr io.Writer) error {
	if plan.Resolve == nil {
		return fmt.Errorf("Compose reconciliation image plan is incomplete")
	}
	if err := approveComposeServiceChanges(ctx, plan.Summaries, options.Yes, stdout); err != nil {
		return err
	}
	spec, err := plan.Resolve(ctx)
	if err != nil {
		return err
	}
	return applyComposeServiceSpec(ctx, manager, spec, stdout, stderr)
}

func applyComposeServiceSpec(ctx context.Context, manager composeServiceReconciler, spec composeservice.Spec, stdout, stderr io.Writer) error {
	if err := manager.Apply(ctx, spec, stdout, stderr); err != nil {
		return err
	}
	status, err := waitForComposeServicesReady(ctx, manager, spec, composeServiceReadyTimeout, composeServiceReadyPoll)
	if err != nil {
		return err
	}
	renderComposeDeploymentStatus(stdout, status)
	if !status.Ready {
		return fmt.Errorf("Dorf Compose services are not ready: %s", composeServiceFailureSummary(status))
	}
	return nil
}

func composeServiceApplySummaries(source composeDeploymentSource, options composeServiceReconcileOptions) []string {
	image := "Acquire, verify, and load official Dorf release image · ghcr.io/aphronio/dorf:" + source.Version
	if options.LocalImage != "" {
		image = "Trust and attest already-loaded contributor/integration image · " + options.LocalImage
	}
	return []string{
		image,
		"Render protected Compose project from the exact image ID · " + source.ProjectDir,
		"Prepare and attest exact PostgreSQL image · " + source.Project.Deployment.Database.ImageID,
		"Attest the durable PostgreSQL volume and run the database migration",
		"Converge PostgreSQL, worker, control API, and prepared optional Compose services",
		"If present, remove only an attested stopped legacy Dorf PostgreSQL container after volume handoff",
	}
}

func reconcileSetupBaseServices(ctx context.Context, manager composeServiceReconciler, options setupOptions, stdout, stderr io.Writer) (releaseapp.ComposeImage, error) {
	source, err := currentComposeDeploymentBaseSource()
	if err != nil {
		return releaseapp.ComposeImage{}, err
	}
	image, resumed, err := setupComposeImageResume(ctx, source, options.LocalImage, releaseapp.AttestInstalledComposeImage)
	if err != nil {
		return releaseapp.ComposeImage{}, err
	}
	if resumed {
		fmt.Fprintf(stdout, "Reusing installed exact Compose image authority · %s\n", image.Reference)
		prepared, err := prepareComposeOptionalRuntime(ctx, source)
		if err != nil {
			return releaseapp.ComposeImage{}, err
		}
		spec, err := prepared.render(image)
		if err != nil {
			return releaseapp.ComposeImage{}, err
		}
		if err := applyComposeServiceSpec(ctx, manager, spec, stdout, stderr); err != nil {
			return releaseapp.ComposeImage{}, err
		}
		return image, nil
	}
	resumeAuthority := image
	image = releaseapp.ComposeImage{}
	serviceOptions := composeServiceReconcileOptions{Yes: options.Yes, LocalImage: options.LocalImage}
	plan := composeServiceReconcilePlan{Summaries: composeServiceApplySummaries(source, serviceOptions)}
	plan.Resolve = func(ctx context.Context) (composeservice.Spec, error) {
		prepared, prepareErr := prepareComposeOptionalRuntime(ctx, source)
		if prepareErr != nil {
			return composeservice.Spec{}, prepareErr
		}
		var resolveErr error
		if serviceOptions.LocalImage != "" {
			image, resolveErr = releaseapp.AttestLocalComposeImage(ctx, prepared.Version, prepared.BinaryPath, serviceOptions.LocalImage)
		} else {
			image, resolveErr = releaseapp.AcquireComposeImage(ctx, prepared.Version, prepared.BinaryPath, prepared.ImageCache)
		}
		if resolveErr != nil {
			return composeservice.Spec{}, resolveErr
		}
		if err := requireMatchingReacquiredComposeImage(resumeAuthority, image); err != nil {
			return composeservice.Spec{}, err
		}
		return prepared.render(image)
	}
	if err := reconcileComposeServicesWith(ctx, manager, plan, serviceOptions, stdout, stderr); err != nil {
		return releaseapp.ComposeImage{}, err
	}
	return image, nil
}

func setupComposeImageResume(ctx context.Context, source composeDeploymentSource, requestedLocalImage string, attest func(context.Context, string, releaseapp.ComposeImage) (bool, error)) (releaseapp.ComposeImage, bool, error) {
	path := filepath.Join(source.ProjectDir, composeproject.ImageFile)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return releaseapp.ComposeImage{}, false, nil
	} else if err != nil {
		return releaseapp.ComposeImage{}, false, fmt.Errorf("inspect installed Compose image authority: %w", err)
	}
	image, err := composeproject.LoadImageAuthority(source.ProjectDir, source.Version, source.BinaryPath, source.Project.UID, source.Project.GID)
	if err != nil {
		return releaseapp.ComposeImage{}, false, err
	}
	requestedLocalImage = strings.TrimSpace(requestedLocalImage)
	if requestedLocalImage != "" && image.Reference != requestedLocalImage {
		return releaseapp.ComposeImage{}, false, nil
	}
	if attest == nil {
		return releaseapp.ComposeImage{}, false, fmt.Errorf("installed Compose image attestation is not configured")
	}
	found, err := attest(ctx, source.BinaryPath, image)
	if err != nil {
		return releaseapp.ComposeImage{}, false, fmt.Errorf("attest installed Compose image authority: %w", err)
	}
	if !found {
		if image.ReleaseTag == "" {
			return releaseapp.ComposeImage{}, false, fmt.Errorf("installed local Compose image %s is no longer loaded; reload that exact image and rerun dorf setup --local-image %s", image.Reference, image.Reference)
		}
		return image, false, nil
	}
	return image, true, nil
}

func requireMatchingReacquiredComposeImage(previous, current releaseapp.ComposeImage) error {
	if previous.Reference != "" && previous != current {
		return fmt.Errorf("reacquired official Compose image does not match the protected installed image authority")
	}
	return nil
}

// reconcileSetupFinalServices is the one real setup lifecycle boundary. It
// re-renders optional prepared services around the already acquired exact
// image, without another approval, download, or image-attestation choice.
func reconcileSetupFinalServices(ctx context.Context, manager composeServiceReconciler, image releaseapp.ComposeImage, stdout, stderr io.Writer) error {
	source, err := currentComposeDeploymentSource()
	if err != nil {
		return err
	}
	return reconcileSetupFinalServicesWith(ctx, manager, source, image, stdout, stderr)
}

func reconcileSetupFinalServicesWith(ctx context.Context, manager composeServiceReconciler, source composeDeploymentSource, image releaseapp.ComposeImage, stdout, stderr io.Writer) error {
	spec, err := source.render(image)
	if err != nil {
		return err
	}
	return applyComposeServiceSpec(ctx, manager, spec, stdout, stderr)
}

// reconcileExistingComposeServices converges prepared optional services using
// the installed project's protected exact image authority. It never acquires
// an image or opens a second approval boundary.
func reconcileExistingComposeServices(ctx context.Context, manager composeServiceReconciler, stdout, stderr io.Writer) error {
	source, err := currentComposeDeploymentBaseSource()
	if err != nil {
		return err
	}
	source, err = prepareComposeOptionalRuntime(ctx, source)
	if err != nil {
		return err
	}
	image, err := composeproject.LoadImageAuthority(source.ProjectDir, source.Version, source.BinaryPath, source.Project.UID, source.Project.GID)
	if err != nil {
		return err
	}
	spec, err := source.render(image)
	if err != nil {
		return err
	}
	return applyComposeServiceSpec(ctx, manager, spec, stdout, stderr)
}

type setupBootstrapKind = bootstraphelper.Name

const (
	bootstrapDocker setupBootstrapKind = bootstraphelper.Docker
	bootstrapIncus  setupBootstrapKind = bootstraphelper.Incus
)

func setupBootstrapHandoff(kind setupBootstrapKind, cause error, output io.Writer) error {
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
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
	arguments := []string{"sudo", "--", artifact.Path, "--user", account.Username}
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

func waitForComposeServicesReady(ctx context.Context, manager composeServiceStatuser, spec composeservice.Spec, timeout, interval time.Duration) (composeservice.Status, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last composeservice.Status
	for {
		status, err := manager.Status(waitCtx, spec)
		if err != nil {
			return composeservice.Status{}, err
		}
		last = status
		if status.Ready {
			return status, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			return last, fmt.Errorf("Dorf Compose services did not become ready within %s: %s", timeout, composeServiceFailureSummary(last))
		case <-timer.C:
		}
	}
}

func approveComposeServiceChanges(ctx context.Context, summaries []string, yes bool, output io.Writer) error {
	description := composeServiceChangeDescription(summaries)
	fmt.Fprintln(output, "Dorf Compose changes:")
	fmt.Fprintln(output, description)
	if yes {
		return nil
	}
	presenter := newSetupPresenter(output)
	if !presenter.interactive {
		return fmt.Errorf("Compose deployment changes require approval; rerun dorf service reconcile --yes")
	}
	approved := false
	if err := presenter.RunForm(ctx, presenter.ConfirmGroup("Apply these Compose deployment changes?", description, &approved)); err != nil {
		if errors.Is(err, errSetupCancelled) {
			return errComposeServiceReconcileCancelled
		}
		return fmt.Errorf("confirm Compose deployment changes: %w", err)
	}
	if !approved {
		return errComposeServiceReconcileCancelled
	}
	return nil
}

func composeServiceChangeDescription(summaries []string) string {
	lines := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		lines = append(lines, "  • "+summary)
	}
	return strings.Join(lines, "\n")
}

func renderComposeDeploymentStatus(output io.Writer, status composeservice.Status) {
	fmt.Fprintf(output, "Dorf Compose services: %s · %s · %s\n", serviceState(status.Ready, "ready", "not ready"), serviceState(status.Converged, "converged", "not converged"), serviceState(status.Current, "current", "drifted"))
	renderComposeServiceStatus(output, "PostgreSQL", status.Postgres)
	renderComposeServiceStatus(output, "Worker", status.Worker)
	renderComposeServiceStatus(output, "Control reader", status.ControlReader)
	renderComposeServiceStatus(output, "Control API", status.ControlAPI)
	if status.Gateway != nil {
		renderComposeServiceStatus(output, "Provider Gateway", *status.Gateway)
	}
	if status.Cloudflare != nil {
		renderComposeServiceStatus(output, "Cloudflare Tunnel", *status.Cloudflare)
	}
	fmt.Fprintf(output, "  Discovery: %s · %s\n", serviceState(status.API.Ready, "ready", "not ready"), status.API.Detail)
}

func renderComposeServiceStatus(output io.Writer, label string, status composeservice.ServiceStatus) {
	parts := []string{serviceState(status.Ready, "ready", "not ready"), empty(status.State)}
	if status.Health != "" {
		parts = append(parts, status.Health)
	}
	parts = append(parts, serviceState(status.Current, "current", "drifted"))
	if !status.Ready && strings.TrimSpace(status.Detail) != "" {
		parts = append(parts, status.Detail)
	}
	fmt.Fprintf(output, "  %s: %s\n", label, strings.Join(parts, " · "))
}

func serviceState(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}

func composeServiceFailureSummary(status composeservice.Status) string {
	detail := fmt.Sprintf("PostgreSQL: %s; worker: %s; control reader: %s; control API: %s; discovery: %s",
		status.Postgres.Detail, status.Worker.Detail, status.ControlReader.Detail, status.ControlAPI.Detail,
		status.API.Detail)
	if status.Gateway != nil {
		detail += "; Provider Gateway: " + status.Gateway.Detail
	}
	if status.Cloudflare != nil {
		detail += "; Cloudflare Tunnel: " + status.Cloudflare.Detail
	}
	return detail
}

func writeComposeServiceUsage(output io.Writer) {
	fmt.Fprintln(output, "usage: dorf service <reconcile|status|restart|logs> [options]")
}
