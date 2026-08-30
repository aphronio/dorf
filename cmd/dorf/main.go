package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	cloudflareapp "github.com/aphronio/dorf/internal/cloudflare"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/doctor"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/hostsetup"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/proofbarrier"
	releaseapp "github.com/aphronio/dorf/internal/release"
	"github.com/aphronio/dorf/internal/version"
	"github.com/charmbracelet/x/term"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "dorf:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usage(stderr)
	}
	if args[0] == "version" {
		fmt.Fprintf(stdout, "dorf %s\n", version.Version)
		return nil
	}
	if args[0] == "update" {
		if len(args) != 1 {
			return fmt.Errorf("update does not accept arguments")
		}
		result, err := releaseapp.UpdateApplication(ctx, stdout, stderr)
		if err != nil {
			return err
		}
		if result.Updated {
			fmt.Fprintf(stdout, "Dorf update complete: %s -> %s\n", result.From, result.Latest)
			fmt.Fprintln(stdout, "On a deployment host, run dorf setup to apply and verify the updated deployment.")
		} else if result.From == result.Latest {
			fmt.Fprintf(stdout, "Dorf is already up to date: %s\n", result.From)
		} else {
			fmt.Fprintf(stdout, "No update available: running %s; latest published release is %s\n", result.From, result.Latest)
		}
		return nil
	}
	if handled, err := containerForegroundCommand(ctx, args, stdout, stderr); handled || err != nil {
		return err
	}
	if handled, err := remoteCommand(ctx, args, stdout, stderr); handled || err != nil {
		return err
	}
	switch args[0] {
	case "setup", "integration", "client", "migrate", "doctor", "provider", "profile", "release-manifest", "serve", "worker":
	default:
		return usage(stderr)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if args[0] == "setup" {
		return setupCommand(ctx, cfg, args[1:], stdout, stderr)
	}
	if args[0] == "integration" {
		return integrationCommand(ctx, cfg, args[1:], stdout, stderr)
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return fmt.Errorf("PostgreSQL is not configured; run dorf setup")
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer db.Close()
	store := postgres.Store{DB: db}
	switch args[0] {
	case "client":
		return clientCommand(ctx, store, args[1:], stdout, stderr)
	case "migrate":
		return migrate(ctx, store, args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(ctx, db, cfg, args[1:], stdout, stderr)
	case "provider":
		return providerCommand(ctx, store, cfg, args[1:], stdout, stderr)
	case "profile":
		return profileCommand(ctx, store, cfg, args[1:], stdout, stderr)
	case "release-manifest":
		return releaseManifest(args[1:], stdout, stderr)
	}
	client, err := absurdClient(db)
	if err != nil {
		return err
	}
	defer client.Close()
	switch args[0] {
	case "serve":
		return serveCommand(ctx, store, client, cfg, args[1:], stdout, stderr)
	case "worker":
		if err := registerWorkerTasks(store, client, cfg); err != nil {
			return err
		}
		return worker(ctx, store, client, cfg, args[1:], stdout, stderr)
	default:
		return usage(stderr)
	}
}

func configuredProviderGateway(cfg config.Config) gateway.Gateway {
	return gateway.Gateway{StatePath: cfg.GatewayStatePath, InternalDialOrigin: cfg.GatewayInternalOrigin}
}

func directAdmissionKey(value string, source io.Reader) (string, bool, error) {
	return operationKey("direct", value, source)
}

func operationKey(kind, value string, source io.Reader) (string, bool, error) {
	if value = strings.TrimSpace(value); value != "" {
		return value, false, nil
	}
	if source == nil {
		return "", false, fmt.Errorf("generate %s request key: randomness is not configured", kind)
	}
	random := make([]byte, 16)
	if _, err := io.ReadFull(source, random); err != nil {
		return "", false, fmt.Errorf("generate %s request key: %w", kind, err)
	}
	return kind + "-" + hex.EncodeToString(random), true, nil
}

func registerWorkerTasks(store postgres.Store, client *absurd.Client, cfg config.Config) error {
	barrier, err := proofbarrier.FromEnv()
	if err != nil {
		return err
	}
	runtimes := profileRuntimeResolver{cfg: cfg, store: store, client: client, barrier: barrier}
	core := coreApplication(store, client)
	core.SandboxRuntimes = runtimes
	core.CleanupRuntimes = runtimes
	core.RegisterCleanup()
	direct.Register(core, store, runtimes)
	coding.Register(core, store, runtimes)
	investigation.Register(core, store, runtimes)
	return nil
}

func coreApplication(store postgres.Store, client *absurd.Client) core.Application {
	return core.Application{Store: store, Tasks: client, AgentMessages: composedMessageAdmissions{store: store}}
}

// composedMessageAdmissions is closed-world deployment composition, not a
// Core registry. Each known workflow or client supplies its execution envelope.
type composedMessageAdmissions struct{ store postgres.Store }

func (a composedMessageAdmissions) AdmitAgentMessage(ctx context.Context, input core.MessageAdmission) (core.MessageAdmissionResult, error) {
	job, err := a.store.Job(ctx, input.JobID)
	if err != nil {
		return core.MessageAdmissionResult{}, err
	}
	var admitted core.MessageAdmissionResult
	switch {
	case job.Workflow == "" && job.WorkflowRevision == "":
		admitted, err = a.store.AdmitDirectMessage(ctx, input)
	case job.Workflow == coding.Workflow && job.WorkflowRevision == coding.WorkflowRevision:
		admitted, err = a.store.AdmitCodingMessage(ctx, input)
	case job.Workflow == investigation.Workflow && job.WorkflowRevision == investigation.WorkflowRevision:
		admitted, err = a.store.AdmitInvestigationMessage(ctx, input)
	default:
		return core.MessageAdmissionResult{}, fmt.Errorf("Job contract %s revision %s does not accept Messages in this deployment", job.Workflow, job.WorkflowRevision)
	}
	if err != nil {
		return admitted, err
	}
	return admitted, nil
}

func absurdClient(db *sql.DB) (*absurd.Client, error) {
	return absurd.New(absurd.Options{
		DB:                 db,
		QueueName:          config.QueueName,
		DefaultMaxAttempts: 5,
		Logger:             absurdruntime.WorkerLogger(os.Stderr),
	})
}

func migrate(ctx context.Context, store postgres.Store, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("migrate", flag.ContinueOnError)
	set.SetOutput(stderr)
	absurdSchema := set.String("absurd-schema", "", "path to the exact upstream Absurd 0.5.0 absurd.sql (required only for first initialization)")
	if err := set.Parse(args); err != nil {
		return err
	}
	ready, readyErr := store.AbsurdReady(ctx)
	if readyErr != nil {
		return readyErr
	}
	var contents []byte
	var err error
	if ready {
		contents = nil
	} else if *absurdSchema != "" {
		contents, err = os.ReadFile(*absurdSchema)
		if err != nil {
			return fmt.Errorf("read Absurd schema: %w", err)
		}
	} else {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, postgres.AbsurdSchemaURL, nil)
		if requestErr != nil {
			return requestErr
		}
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			return fmt.Errorf("download pinned Absurd 0.5.0 schema: %w (or pass --absurd-schema)", requestErr)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("download pinned Absurd 0.5.0 schema: HTTP %d (or pass --absurd-schema)", response.StatusCode)
		}
		contents, err = io.ReadAll(io.LimitReader(response.Body, 4<<20))
		if err != nil {
			return fmt.Errorf("download pinned Absurd 0.5.0 schema: %w", err)
		}
	}
	if !ready {
		if err := store.BootstrapAbsurd(ctx, contents); err != nil {
			return err
		}
	}
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "PostgreSQL ready: Dorf schema and Absurd 0.5.0 queue dorf_jobs")
	return nil
}

func providerCommand(ctx context.Context, store postgres.Store, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("provider requires: connect chatgpt, connect openai, or status")
	}
	if args[0] == "status" {
		return providerStatusCommand(ctx, store, cfg, args[1:], stdout, stderr)
	}
	if args[0] != "connect" || len(args) < 2 || (args[1] != "chatgpt" && args[1] != "openai") {
		return fmt.Errorf("the supported provider commands are: provider connect chatgpt, provider connect openai, and provider status")
	}
	authMode := args[1]
	set := flag.NewFlagSet("provider connect "+authMode, flag.ContinueOnError)
	set.SetOutput(stderr)
	defaultName := "personal-chatgpt"
	if authMode == "openai" {
		defaultName = "openai-api"
	}
	name := set.String("name", defaultName, "stable AI connection name")
	bind := set.String("bind", "", "exact broker bind IP; a non-loopback address requires its matching Incus --profile")
	profileName := set.String("profile", "", "Incus profile used to resolve its private bridge")
	apiKeyFile := set.String("api-key-file", "", "OpenAI API key file; use - to read standard input")
	if err := set.Parse(args[2:]); err != nil {
		return err
	}
	if authMode == "chatgpt" && strings.TrimSpace(*apiKeyFile) != "" {
		return fmt.Errorf("provider connect chatgpt does not accept --api-key-file")
	}
	if authMode == "openai" && strings.TrimSpace(*apiKeyFile) == "" {
		return fmt.Errorf("provider connect openai requires --api-key-file PATH or -")
	}
	if err := requireSupportedCloudflareState(cfg.GatewayStatePath); err != nil {
		return err
	}
	g, resolvedBind, err := providerGatewayForBind(ctx, store, cfg, *bind, *profileName)
	if err != nil {
		return err
	}
	provider := "ChatGPT subscription"
	if authMode == "openai" {
		key, err := readSecretFile(*apiKeyFile, os.Stdin)
		if err != nil {
			return fmt.Errorf("read OpenAI API key: %w", err)
		}
		if err := g.ConnectOpenAIAPIKey(ctx, *name, resolvedBind, key); err != nil {
			return err
		}
		provider = "OpenAI API key"
	} else {
		if err := g.ConnectChatGPT(ctx, *name, resolvedBind, func(url, code string) {
			fmt.Fprintf(stdout, "Open %s and enter %s\n", url, code)
		}); err != nil {
			return err
		}
	}
	if err := makeProviderConnectionReady(ctx, *name, func(ctx context.Context) error {
		return refreshExistingDeploymentConfig(ctx)
	}, g.FinalizeConnection, g.SetDefaultConnection); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "AI connection ready: %s (%s; default; broker %s on %s)\n", *name, provider, gateway.BackendVersion, resolvedBind)
	return nil
}

func makeProviderConnectionReady(
	ctx context.Context,
	name string,
	prepare func(context.Context) error,
	finalize func(context.Context, string) error,
	setDefault func(string) error,
) error {
	if err := prepare(ctx); err != nil {
		return fmt.Errorf("prepare deployment configuration for AI connection %q: %w", name, err)
	}
	if err := finalize(ctx, name); err != nil {
		return fmt.Errorf("verify AI connection %q through the deployment Gateway: %w", name, err)
	}
	if err := setDefault(name); err != nil {
		return fmt.Errorf("select default AI connection %q: %w", name, err)
	}
	return nil
}

func providerGatewayForBind(ctx context.Context, store postgres.Store, cfg config.Config, bind, profileName string) (gateway.Gateway, string, error) {
	bind, profileName = strings.TrimSpace(bind), strings.TrimSpace(profileName)
	parsedBind := net.ParseIP(bind)
	if bind != "" && parsedBind != nil && parsedBind.To4() != nil && !parsedBind.IsLoopback() && profileName == "" {
		return gateway.Gateway{}, "", fmt.Errorf("a non-loopback provider bind requires its matching Incus --profile")
	}
	profiles, err := store.SandboxProfiles(ctx)
	if err != nil {
		return gateway.Gateway{}, "", err
	}
	profile, err := providerGatewayProfile(profiles, profileName)
	if err != nil {
		return gateway.Gateway{}, "", err
	}
	g := configuredProviderGateway(cfg)
	preparedAddress, prepared, err := g.PreparedComposePublishAddress()
	if err != nil {
		return gateway.Gateway{}, "", err
	}
	resolved, err := selectProviderGatewayBind(cfg, profiles, profile, bind, preparedAddress, prepared)
	if err != nil {
		return gateway.Gateway{}, "", err
	}
	return g, resolved, nil
}

func providerGatewayProfile(profiles []core.SandboxProfile, name string) (*core.SandboxProfile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	for i := range profiles {
		if profiles[i].Name == name {
			return &profiles[i], nil
		}
	}
	return nil, postgres.ErrProfileNotFound
}

func selectProviderGatewayBind(cfg config.Config, profiles []core.SandboxProfile, selected *core.SandboxProfile, requested, preparedAddress string, prepared bool) (string, error) {
	requested, preparedAddress = strings.TrimSpace(requested), strings.TrimSpace(preparedAddress)
	if selected != nil && selected.Provider == core.SandboxProviderIncus {
		if err := validateIncusProfileEndpointAuthority(cfg.Incus, *selected); err != nil {
			return "", err
		}
	}
	network, err := gatewayIncusNetwork(profiles, nil)
	if err != nil {
		return "", err
	}
	profileAddress, directRequired, err := guidedIncusBridgeAuthority(profiles, nil, network)
	if err != nil {
		return "", err
	}
	if directRequired {
		for _, profile := range profiles {
			if _, direct := guidedIncusProfilePublishAddress(profile); direct && profile.IncusNetwork == network {
				if err := validateIncusProfileEndpointAuthority(cfg.Incus, profile); err != nil {
					return "", err
				}
			}
		}
	}
	if requested != "" {
		parsed := net.ParseIP(requested)
		if parsed == nil || parsed.To4() == nil || parsed.IsUnspecified() || (!parsed.IsLoopback() && !privateOrSharedIPv4(parsed)) {
			return "", fmt.Errorf("provider bind must be one loopback or private IPv4 address")
		}
		requested = parsed.To4().String()
		if !parsed.IsLoopback() {
			if selected == nil {
				return "", fmt.Errorf("a non-loopback provider bind requires its matching Incus --profile")
			}
			if selected.Provider != core.SandboxProviderIncus {
				return "", fmt.Errorf("Sandbox profile %q is not an Incus profile and cannot authorize a non-loopback provider bind", selected.Name)
			}
			selectedAddress, direct := guidedIncusProfilePublishAddress(*selected)
			if !direct || requested != selectedAddress {
				return "", fmt.Errorf("provider bind %s does not match Sandbox profile %q exact Gateway URL", requested, selected.Name)
			}
		}
	}
	return selectGatewayPublishAddress(requested, preparedAddress, prepared, profileAddress, directRequired)
}

func selectGatewayPublishAddress(requested, preparedAddress string, prepared bool, profileAddress string, directRequired bool) (string, error) {
	requested, preparedAddress, profileAddress = strings.TrimSpace(requested), strings.TrimSpace(preparedAddress), strings.TrimSpace(profileAddress)
	if requested != "" {
		if directRequired && requested != profileAddress {
			return "", fmt.Errorf("provider bind %s conflicts with Sandbox Profile Gateway publication %s; update and re-verify those profiles explicitly first", requested, profileAddress)
		}
		return requested, nil
	}
	if directRequired {
		if prepared && preparedAddress != profileAddress {
			return "", fmt.Errorf("prepared Compose Gateway publication %s conflicts with Sandbox Profile authority %s; update and re-verify the affected profile explicitly", preparedAddress, profileAddress)
		}
		return profileAddress, nil
	}
	if prepared {
		return preparedAddress, nil
	}
	return "127.0.0.1", nil
}

func readSecretFile(path string, stdin io.Reader) (string, error) {
	var source io.Reader
	var closeSource func() error
	if strings.TrimSpace(path) == "-" {
		if stdin == nil {
			return "", fmt.Errorf("standard input is unavailable")
		}
		source = stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		source, closeSource = file, file.Close
	}
	if closeSource != nil {
		defer closeSource()
	}
	raw, err := io.ReadAll(io.LimitReader(source, (16<<10)+1))
	if err != nil {
		return "", err
	}
	if len(raw) > 16<<10 {
		return "", fmt.Errorf("secret exceeds 16 KiB")
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return "", fmt.Errorf("secret is empty")
	}
	return secret, nil
}

type providerGatewayCheckView struct {
	Status string `json:"status"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail"`
}

type providerGatewayStatusView struct {
	Profile           string                   `json:"profile"`
	SandboxProvider   core.SandboxProvider     `json:"sandbox_provider"`
	ProfileVerified   bool                     `json:"profile_verified"`
	ProfileVerifiedAt *time.Time               `json:"profile_verified_at,omitempty"`
	Connection        string                   `json:"connection"`
	Lifecycle         string                   `json:"lifecycle"`
	Authority         providerGatewayCheckView `json:"authority"`
	SandboxPath       providerGatewayCheckView `json:"sandbox_path"`
	Ready             bool                     `json:"ready"`
	Impact            string                   `json:"impact"`
	Next              string                   `json:"next,omitempty"`
}

func providerStatusCommand(ctx context.Context, store postgres.Store, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("provider status", flag.ContinueOnError)
	set.SetOutput(stderr)
	connection := set.String("ai-connection", "", "named AI connection (default: deployment default)")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	jsonOutput := set.Bool("json", false, "emit machine-readable status")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("provider status received unexpected arguments")
	}
	profile, err := sandboxProfileByNameOrDefault(ctx, store, *profileName)
	if err != nil {
		return err
	}
	g := configuredProviderGateway(cfg)
	selectedConnection, err := selectedAIConnection(g, *connection)
	if err != nil {
		return err
	}
	authorityErr := g.Check(ctx, selectedConnection)
	var sandboxPathErr error
	switch profile.Provider {
	case core.SandboxProviderE2B:
		remote, gatewayErr := remoteGatewayForProviderStatus(cfg, profile)
		if gatewayErr != nil {
			sandboxPathErr = gatewayErr
		} else {
			sandboxPathErr = remote.CheckRemote(ctx, profile.E2BGatewayURL)
		}
	case core.SandboxProviderIncus:
		sandboxPathErr = validateIncusComposePublication(g, profile)
		if sandboxPathErr == nil {
			remote, gatewayErr := remoteGatewayForProviderStatus(cfg, profile)
			if gatewayErr != nil {
				sandboxPathErr = gatewayErr
			} else {
				sandboxPathErr = remote.CheckRemote(ctx, profile.IncusGatewayURL)
			}
		}
	}
	view := newProviderGatewayStatusView(profile, selectedConnection, authorityErr, sandboxPathErr)
	if *jsonOutput {
		if err := writeJSON(stdout, view); err != nil {
			return err
		}
	} else {
		renderProviderGatewayStatus(stdout, view)
	}
	if !view.Ready {
		return fmt.Errorf("provider gateway status is not ready")
	}
	return nil
}

func selectedAIConnection(g gateway.Gateway, explicit string) (string, error) {
	if name := strings.TrimSpace(explicit); name != "" {
		return name, nil
	}
	return g.DefaultConnection()
}

func remoteGatewayForProviderStatus(cfg config.Config, profile core.SandboxProfile) (gateway.Gateway, error) {
	g := configuredProviderGateway(cfg)
	gatewayURL := ""
	switch profile.Provider {
	case core.SandboxProviderE2B:
		gatewayURL = profile.E2BGatewayURL
	case core.SandboxProviderIncus:
		gatewayURL = profile.IncusGatewayURL
	default:
		return g, nil
	}
	tunnel := cloudflareapp.Tunnel{StatePath: filepath.Join(cfg.GatewayStatePath, "cloudflare")}
	state, found, err := tunnel.Current()
	if err != nil {
		return g, fmt.Errorf("inspect Dorf-owned Cloudflare Tunnel: %w", err)
	}
	if !found {
		return g, nil
	}
	ownedURL, err := cloudflareapp.GatewayURL(state.ModelHostname)
	if err != nil {
		return g, err
	}
	if ownedURL == gatewayURL {
		client := freshDNSHTTPClient()
		probeURL, err := state.ProbeURL(state.ModelHostname)
		if err != nil {
			return g, err
		}
		g.Client = client
		g.DeploymentProbeURL = probeURL
	}
	return g, nil
}

func validateIncusComposePublication(g gateway.Gateway, profile core.SandboxProfile) error {
	profileAddress, direct := guidedIncusProfilePublishAddress(profile)
	if !direct {
		return nil
	}
	published, found, err := g.PreparedComposePublishAddress()
	if err != nil {
		return fmt.Errorf("inspect Compose Gateway publication: %w", err)
	}
	if !found {
		return fmt.Errorf("Compose Gateway publication is not prepared for Sandbox Profile %q", profile.Name)
	}
	if published != profileAddress {
		return fmt.Errorf("Compose publishes %s but Sandbox Profile %q owns %s", published, profile.Name, profileAddress)
	}
	return nil
}

func newProviderGatewayStatusView(profile core.SandboxProfile, connection string, authorityErr, sandboxPathErr error) providerGatewayStatusView {
	check := func(target string, err error) providerGatewayCheckView {
		if err != nil {
			return providerGatewayCheckView{Status: "failed", Target: target, Detail: err.Error()}
		}
		return providerGatewayCheckView{Status: "ready", Target: target, Detail: "ready"}
	}
	view := providerGatewayStatusView{
		Profile: profile.Name, SandboxProvider: profile.Provider, ProfileVerified: profile.BaseVerified(),
		Connection: connection, Lifecycle: "Compose-owned Provider Gateway service",
		Authority: check("private broker and named AI connection", authorityErr),
	}
	if profile.BaseVerified() {
		verifiedAt := profile.Verification.ProbeCompletedAt
		view.ProfileVerifiedAt = &verifiedAt
	}
	if profile.Provider == core.SandboxProviderE2B || profile.Provider == core.SandboxProviderIncus {
		gatewayURL := profile.E2BGatewayURL
		if profile.Provider == core.SandboxProviderIncus {
			gatewayURL = profile.IncusGatewayURL
		}
		view.SandboxPath = check(gatewayURL, sandboxPathErr)
		if sandboxPathErr == nil {
			view.SandboxPath.Detail = "reachable; anonymous access rejected"
		}
	} else {
		view.SandboxPath = providerGatewayCheckView{
			Status: "failed", Detail: "unsupported Sandbox provider",
		}
	}
	view.Ready = view.ProfileVerified && view.Authority.Status == "ready" &&
		view.SandboxPath.Status == "ready"
	switch {
	case !view.ProfileVerified:
		detail, next := sandboxProfileNotReady(profile)
		view.Impact = "new Jobs cannot use this Sandbox profile; " + detail
		view.Next = next
	case view.Authority.Status != "ready":
		view.Impact = "new AgentRuns cannot obtain authenticated inference routes"
		view.Next = "restore the named AI connection and private broker, then rerun provider status"
	case profile.Provider == core.SandboxProviderE2B && view.SandboxPath.Status != "ready":
		view.Impact = "remote Sandboxes using this profile cannot reach inference"
		view.Next = "restore the configured HTTPS route, or update and reverify the profile"
	case profile.Provider == core.SandboxProviderIncus && view.SandboxPath.Status != "ready":
		view.Impact = "Incus Sandboxes using this profile cannot reach inference"
		view.Next = "restore the exact Compose publication and Gateway route, or update and reverify the profile"
	default:
		view.Impact = "none"
	}
	return view
}

func renderProviderGatewayStatus(output io.Writer, view providerGatewayStatusView) {
	verification := "not verified"
	if view.ProfileVerifiedAt != nil {
		verification = "verified previously · " + view.ProfileVerifiedAt.Local().Format(time.RFC3339)
	}
	fmt.Fprintln(output, "Provider Gateway")
	fmt.Fprintf(output, "  Profile       %s · %s\n", view.Profile, strings.ToUpper(string(view.SandboxProvider)))
	fmt.Fprintf(output, "  Verification  %s\n", verification)
	fmt.Fprintf(output, "  Connection    %s\n", view.Connection)
	fmt.Fprintf(output, "  Lifecycle     %s\n", view.Lifecycle)
	fmt.Fprintf(output, "  Authority     %s\n", view.Authority.Status)
	if view.Authority.Status != "ready" {
		fmt.Fprintf(output, "                %s\n", view.Authority.Detail)
	}
	fmt.Fprintf(output, "  Sandbox path  %s · %s\n", view.SandboxPath.Status, view.SandboxPath.Target)
	if view.SandboxPath.Status == "failed" {
		fmt.Fprintf(output, "                %s\n", view.SandboxPath.Detail)
	}
	fmt.Fprintf(output, "  Impact        %s\n", view.Impact)
	if view.Next != "" {
		fmt.Fprintf(output, "  Next          %s\n", view.Next)
	}
}

func releaseManifest(args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("release-manifest", flag.ContinueOnError)
	set.SetOutput(stderr)
	archive := set.String("archive", "", "Incus export archive")
	metadata := set.String("image-metadata", "", "candidate image metadata")
	tag := set.String("release-tag", "", "release tag")
	source := set.String("source-commit", "", "exact source commit")
	validated := set.String("validated-at", "", "UTC validation time")
	output := set.String("output", "", "manifest output path")
	if err := set.Parse(args); err != nil {
		return err
	}
	if err := releaseapp.CreateManifest(*archive, *metadata, *tag, *source, *validated, *output); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Release manifest ready: %s\n", *output)
	return nil
}

type setupOptions struct {
	Yes                       bool
	Connection                string
	ProfileName               string
	LocalImage                string
	IncusManifest             string
	IncusArchive              string
	IncusTrustOfferFile       string
	SandboxProviders          sandboxProviderFlags
	Harness                   string
	ConnectionMode            setupConnectionMode
	OpenAIKeyFile             string
	E2BKeyFile                string
	E2BTemplate               string
	GatewayURL                string
	CloudflareDomain          string
	CloudflareControlHostname string
	CloudflareModelHostname   string
	ReplaceCloudflareDNS      bool
	AllowInternet             bool
}

type sandboxProviderFlags []core.SandboxProvider

func (values *sandboxProviderFlags) String() string {
	names := make([]string, 0, len(*values))
	for _, value := range *values {
		names = append(names, string(value))
	}
	return strings.Join(names, ",")
}

func (values *sandboxProviderFlags) Set(raw string) error {
	provider := core.SandboxProvider(strings.ToLower(strings.TrimSpace(raw)))
	if provider != core.SandboxProviderIncus && provider != core.SandboxProviderE2B {
		return fmt.Errorf("Sandbox provider must be incus or e2b")
	}
	for _, existing := range *values {
		if existing == provider {
			return nil
		}
	}
	*values = append(*values, provider)
	return nil
}

func parseSetupOptions(args []string, stderr io.Writer) (setupOptions, error) {
	set := flag.NewFlagSet("setup", flag.ContinueOnError)
	set.SetOutput(stderr)
	yes := set.Bool("yes", false, "approve guided setup choices")
	connection := set.String("ai-connection", "", "named AI connection (default: deployment default)")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	localImage := set.String("local-image", "", "trust one already-loaded exact contributor/integration image reference")
	incusManifest := set.String("incus-manifest", "", "verified local Incus image manifest")
	incusArchive := set.String("incus-archive", "", "matching local Incus VM archive")
	incusTrustOfferFile := set.String("incus-trust-offer-file", "", "protected Incus trust offer file; use - for standard input")
	providers := sandboxProviderFlags{}
	set.Var(&providers, "sandbox-provider", "configure incus or e2b; repeat to select both")
	harness := set.String("harness", "", "Harness for guided profiles: codex or pi")
	connectionMode := set.String("connection-auth", "", "create an AI connection with chatgpt or openai")
	openAIKeyFile := set.String("openai-api-key-file", "", "OpenAI API key file for guided setup")
	e2bKeyFile := set.String("e2b-api-key-file", "", "E2B API key file for guided setup")
	e2bTemplate := set.String("e2b-template", "", "exact E2B template build reference")
	gatewayURL := set.String("gateway-url", "", "existing stable HTTPS /v1 Provider Gateway URL")
	cloudflareDomain := set.String("cloudflare-domain", "", "Dorf domain using Cloudflare DNS")
	cloudflareControlHostname := set.String("cloudflare-control-hostname", "", "exact Control API hostname under the Dorf domain")
	cloudflareModelHostname := set.String("cloudflare-model-hostname", "", "exact model Gateway hostname under the Dorf domain")
	replaceCloudflareDNS := set.Bool("replace-cloudflare-dns", false, "replace unrelated DNS routes for the selected public endpoints")
	allowInternet := set.Bool("allow-internet", false, "allow general internet egress from the guided E2B profile")
	if err := set.Parse(args); err != nil {
		return setupOptions{}, err
	}
	if set.NArg() != 0 {
		return setupOptions{}, fmt.Errorf("setup does not accept positional arguments")
	}
	options := setupOptions{
		Yes: *yes, Connection: strings.TrimSpace(*connection),
		ProfileName: strings.TrimSpace(*profileName), LocalImage: strings.TrimSpace(*localImage),
		IncusManifest: strings.TrimSpace(*incusManifest), IncusArchive: strings.TrimSpace(*incusArchive),
		IncusTrustOfferFile: strings.TrimSpace(*incusTrustOfferFile),
		SandboxProviders:    providers,
		Harness:             strings.TrimSpace(*harness), ConnectionMode: setupConnectionMode(strings.TrimSpace(*connectionMode)),
		OpenAIKeyFile: strings.TrimSpace(*openAIKeyFile), E2BKeyFile: strings.TrimSpace(*e2bKeyFile),
		E2BTemplate: strings.TrimSpace(*e2bTemplate), GatewayURL: strings.TrimSpace(*gatewayURL),
		CloudflareDomain:          strings.TrimSpace(*cloudflareDomain),
		CloudflareControlHostname: strings.TrimSpace(*cloudflareControlHostname),
		CloudflareModelHostname:   strings.TrimSpace(*cloudflareModelHostname),
		ReplaceCloudflareDNS:      *replaceCloudflareDNS,
		AllowInternet:             *allowInternet,
	}
	if err := validateSetupOptions(options); err != nil {
		return setupOptions{}, err
	}
	return options, nil
}

func validateSetupOptions(options setupOptions) error {
	cloudflareSelected := options.CloudflareDomain != "" || options.CloudflareControlHostname != "" || options.CloudflareModelHostname != "" || options.ReplaceCloudflareDNS
	if len(options.SandboxProviders) == 0 && (options.Connection != "" || options.ProfileName != "" || options.Harness != "" || options.ConnectionMode != "" || options.OpenAIKeyFile != "" || options.E2BKeyFile != "" || options.E2BTemplate != "" || options.GatewayURL != "" || cloudflareSelected || options.AllowInternet || options.IncusManifest != "" || options.IncusArchive != "" || options.IncusTrustOfferFile != "") {
		return fmt.Errorf("agent setup flags require at least one --sandbox-provider")
	}
	if options.ProfileName != "" && len(options.SandboxProviders) != 1 {
		return fmt.Errorf("--profile requires exactly one --sandbox-provider")
	}
	if options.GatewayURL != "" && cloudflareSelected {
		return fmt.Errorf("setup accepts either --gateway-url or Cloudflare domain options, not both")
	}
	if options.CloudflareDomain == "" && (options.CloudflareControlHostname != "" || options.CloudflareModelHostname != "" || options.ReplaceCloudflareDNS) {
		return fmt.Errorf("Cloudflare endpoint overrides and --replace-cloudflare-dns require --cloudflare-domain")
	}
	if options.Connection != "" && options.ConnectionMode != "" {
		return fmt.Errorf("setup accepts either an existing --ai-connection or --connection-auth, not both")
	}
	if options.ConnectionMode != "" && options.ConnectionMode != setupConnectionChatGPT && options.ConnectionMode != setupConnectionOpenAI {
		return fmt.Errorf("--connection-auth must be chatgpt or openai")
	}
	if options.OpenAIKeyFile != "" && options.ConnectionMode != setupConnectionOpenAI {
		return fmt.Errorf("--openai-api-key-file requires --connection-auth openai")
	}
	if options.Harness != "" && options.Harness != "codex" && options.Harness != "pi" {
		return fmt.Errorf("--harness must be codex or pi")
	}
	if (options.IncusManifest == "") != (options.IncusArchive == "") {
		return fmt.Errorf("setup requires both --incus-manifest and --incus-archive")
	}
	if err := validateSetupIncusOptions(options); err != nil {
		return err
	}
	if len(options.SandboxProviders) > 0 && !containsSandboxProvider(options.SandboxProviders, core.SandboxProviderE2B) &&
		(options.E2BKeyFile != "" || options.E2BTemplate != "" || options.AllowInternet) {
		return fmt.Errorf("E2B setup flags require --sandbox-provider e2b")
	}
	return nil
}

func validateSetupIncusOptions(options setupOptions) error {
	if containsSandboxProvider(options.SandboxProviders, core.SandboxProviderIncus) {
		return nil
	}
	if options.IncusManifest != "" {
		return fmt.Errorf("Incus image transport flags require --sandbox-provider incus")
	}
	if options.IncusTrustOfferFile != "" {
		return fmt.Errorf("--incus-trust-offer-file requires --sandbox-provider incus")
	}
	return nil
}

func setupCommand(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	options, err := parseSetupOptions(args, stderr)
	if err != nil {
		return err
	}
	if cfg.DatabaseExternal {
		return fmt.Errorf("managed dorf setup does not accept DORF_DATABASE_URL; unset it or supervise the development deployment explicitly")
	}
	presenter := newSetupPresenter(stdout)
	presenter.Welcome()
	if err := requireSupportedCloudflareState(cfg.GatewayStatePath); err != nil {
		return err
	}
	if err := checkDockerRuntime(ctx); err != nil {
		return setupBootstrapHandoff(bootstrapDocker, err, stdout)
	}
	presenter.Ready("Host runtime", "Docker Engine with Compose")

	database, err := hostsetup.InitializeDatabase(cfg.DeploymentPath)
	if err != nil {
		return err
	}
	cfg.DatabaseURL, err = database.URL()
	if err != nil {
		return err
	}
	err = presenter.Run(ctx, "Checking image, applying migrations, and waiting for healthy services", func(ctx context.Context) error {
		_, err := prepareSetupDeployment(ctx, options.LocalImage, false)
		return err
	})
	if err != nil {
		return err
	}
	presenter.Ready("Deployment", "Docker Compose services healthy")
	var db *sql.DB
	var store postgres.Store
	err = presenter.Run(ctx, "Preparing durable state", func(ctx context.Context) error {
		var err error
		db, err = sql.Open("pgx", cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("open PostgreSQL: %w", err)
		}
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("connect to Compose PostgreSQL on host loopback: %w", err)
		}
		store = postgres.Store{DB: db}
		return nil
	})
	if err != nil {
		if db != nil {
			db.Close()
		}
		return err
	}
	defer db.Close()
	presenter.Ready("Durable state", "PostgreSQL")
	paths, err := config.CurrentOperatorPaths()
	if err != nil {
		return err
	}
	var hostIdentity controlapi.Identity
	err = presenter.Run(ctx, "Preparing Job control", func(ctx context.Context) error {
		var err error
		hostIdentity, err = ensureHostControlClient(ctx, store, paths.StateDir)
		return err
	})
	if err != nil {
		return err
	}
	presenter.Ready("Job control", "Authenticated deployment-host Client "+hostIdentity.Client.ID)
	fmt.Fprintln(stdout)

	resolvedOptions, err := resolveSetupSandboxOptions(ctx, store, cfg, options, presenter)
	if err != nil {
		if errors.Is(err, errSetupCancelled) {
			presenter.Note("Sandbox setup", "Skipped; no Sandbox provider was prepared")
			options.SandboxProviders = nil
		} else {
			return err
		}
	} else {
		options = resolvedOptions
	}
	providers := []core.SandboxProvider(options.SandboxProviders)
	if containsSandboxProvider(providers, core.SandboxProviderIncus) {
		if err := prepareSetupIncusAuthority(ctx, &cfg, options, providers, paths.ComposeDir, presenter); err != nil {
			return err
		}
	}
	var prepared *guidedSetupPrepared
	if len(providers) == 0 {
		presenter.Note("Agent Sandboxes", "Skipped for now. Run dorf setup again when you’re ready")
	} else {
		value, err := prepareGuidedSetup(ctx, store, &cfg, options, providers, presenter, stdout, stderr)
		if err != nil {
			var readiness incusSetupReadinessError
			if errors.As(err, &readiness) {
				return setupIncusReadinessHandoff(cfg.Incus, err, stdout)
			}
			return err
		}
		prepared = &value
		if value.PrivateIPv4 != "" {
			presenter.Ready("Local Sandbox", "Incus using QEMU/KVM")
		}
	}
	if prepared != nil {
		if err := makeProviderConnectionReady(ctx, prepared.Connection, func(ctx context.Context) error {
			return presenter.Run(ctx, "Applying agent configuration and waiting for healthy services", func(ctx context.Context) error {
				_, err := prepareSetupDeployment(ctx, options.LocalImage, true)
				return err
			})
		}, prepared.Gateway.FinalizeConnection, prepared.Gateway.SetDefaultConnection); err != nil {
			return err
		}
		if err := completeGuidedSetup(ctx, store, cfg, options, *prepared, presenter); err != nil {
			return err
		}
	}
	presenter.Section("Ready")
	if len(providers) == 0 {
		presenter.Ready("Dorf", "Control plane ready. Configure a Sandbox profile before admitting Jobs")
	} else {
		presenter.Ready("Dorf", "Control plane and durable Job worker ready")
	}
	if prepared != nil && prepared.ControlURL != "" {
		presenter.Ready("Connect", "dorf connect "+prepared.ControlURL)
	}
	return nil
}

func prepareSetupIncusAuthority(ctx context.Context, cfg *config.Config, options setupOptions, providers []core.SandboxProvider, composeDir string, presenter setupPresenter) error {
	pendingPath := filepath.Join(composeDir, "incus-enrollment.json")
	kvmAvailable := setupKVMDevicePresent()
	trustOffer := ""
	if setupNeedsInteractiveIncusTrustOffer(cfg, options, providers, kvmAvailable, presenter.interactive) {
		if err := presenter.RunForm(ctx, presenter.SecretGroup(
			"Connect your Incus host",
			"Paste the one-time trust offer from the Incus workstation. Dorf retains the resulting client identity; the offer is one-use.",
			&trustOffer,
		)); err != nil {
			return fmt.Errorf("read Incus trust offer: %w", err)
		}
	}
	if err := establishSetupIncusAuthority(ctx, cfg, options, providers, pendingPath, kvmAvailable, trustOffer, os.Stdin, incus.EnsureEnrollment); err != nil {
		return err
	}
	summary, err := setupIncusAuthoritySummary(*cfg.Incus)
	if err != nil {
		return err
	}
	presenter.Ready("Incus authority", summary)
	return nil
}

func setupNeedsInteractiveIncusTrustOffer(cfg *config.Config, options setupOptions, providers []core.SandboxProvider, kvmAvailable, interactive bool) bool {
	return containsSandboxProvider(providers, core.SandboxProviderIncus) && cfg.Incus == nil && options.IncusTrustOfferFile == "" &&
		!kvmAvailable && interactive && !options.Yes
}

var errSetupCancelled = errors.New("setup cancelled")

func resolveSetupSandboxOptions(ctx context.Context, store postgres.Store, cfg config.Config, options setupOptions, presenter setupPresenter) (setupOptions, error) {
	if len(options.SandboxProviders) > 0 {
		return options, nil
	}
	profiles, err := store.SandboxProfiles(ctx)
	if err != nil {
		return setupOptions{}, err
	}
	resolved, settled := deriveSetupSandboxOptions(options, retainedDefaultSetupProfile(profiles), presenter.interactive)
	if settled {
		return resolved, nil
	}
	selected := []core.SandboxProvider{}
	kvmAvailable := setupKVMDevicePresent()
	remoteIncus := cfg.Incus != nil && strings.HasPrefix(cfg.Incus.Endpoint, "https://")
	if err := presenter.RunForm(ctx, presenter.ProviderGroup(&selected, kvmAvailable, remoteIncus)); err != nil {
		return setupOptions{}, fmt.Errorf("select Sandbox providers: %w", err)
	}
	resolved.SandboxProviders = selected
	return resolved, nil
}

func setupKVMDevicePresent() bool {
	device, err := os.Stat("/dev/kvm")
	return err == nil && device.Mode()&os.ModeCharDevice != 0
}

func deriveSetupSandboxOptions(options setupOptions, retainedDefault *core.SandboxProfile, interactive bool) (setupOptions, bool) {
	if len(options.SandboxProviders) > 0 {
		return options, true
	}
	if retainedDefault != nil {
		options.SandboxProviders = sandboxProviderFlags{retainedDefault.Provider}
		options.ProfileName = retainedDefault.Name
		options.Harness = retainedDefault.Harness
		return options, true
	}
	if options.Yes || !interactive {
		return options, true
	}
	return options, false
}

func retainedDefaultSetupProfile(profiles []core.SandboxProfile) *core.SandboxProfile {
	for index := range profiles {
		if profiles[index].Default {
			return &profiles[index]
		}
	}
	return nil
}

func establishSetupIncusAuthority(
	ctx context.Context,
	cfg *config.Config,
	options setupOptions,
	providers []core.SandboxProvider,
	pendingPath string,
	localKVMAvailable bool,
	promptedTrustOffer string,
	stdin io.Reader,
	enroll func(context.Context, incus.EnrollmentRequest) (deployment.Incus, error),
) error {
	if !containsSandboxProvider(providers, core.SandboxProviderIncus) {
		return nil
	}
	if cfg.Incus != nil {
		return reconcileAcceptedSetupIncusAuthority(ctx, cfg, pendingPath, enroll)
	}

	var authority deployment.Incus
	if options.IncusTrustOfferFile == "" && localKVMAvailable {
		authority = deployment.Incus{Endpoint: "unix://" + incus.DefaultUnixSocket}
		cfg.Incus = &authority
		return nil
	}
	trustOffer, err := selectedSetupIncusTrustOffer(options.IncusTrustOfferFile, promptedTrustOffer, stdin)
	if err != nil {
		return err
	}
	authority, err = enroll(ctx, incus.EnrollmentRequest{
		DeploymentPath: cfg.DeploymentPath,
		PendingPath:    pendingPath,
		TrustToken:     trustOffer,
	})
	if err != nil {
		return err
	}
	return requirePersistedSetupIncusAuthority(cfg, authority)
}

func selectedSetupIncusTrustOffer(path, prompted string, stdin io.Reader) (string, error) {
	if path != "" {
		return readSetupIncusTrustOffer(path, stdin)
	}
	if strings.TrimSpace(prompted) == "" {
		return "", fmt.Errorf("automated remote Incus setup requires --incus-trust-offer-file")
	}
	return normalizeSetupIncusTrustOffer(prompted)
}

func reconcileAcceptedSetupIncusAuthority(
	ctx context.Context,
	cfg *config.Config,
	pendingPath string,
	enroll func(context.Context, incus.EnrollmentRequest) (deployment.Incus, error),
) error {
	accepted := *cfg.Incus
	reconciled, err := enroll(ctx, incus.EnrollmentRequest{
		DeploymentPath: cfg.DeploymentPath,
		PendingPath:    pendingPath,
	})
	if err != nil {
		return err
	}
	if reconciled != accepted {
		return fmt.Errorf("Incus enrollment reconciliation returned a different retained authority")
	}
	return requirePersistedSetupIncusAuthority(cfg, accepted)
}

func readSetupIncusTrustOffer(path string, stdin io.Reader) (string, error) {
	if path != "-" {
		return incus.ReadTrustTokenFile(path)
	}
	trustOffer, err := readSecretFile(path, stdin)
	if err != nil {
		return "", fmt.Errorf("read Incus trust offer from standard input: %w", err)
	}
	return normalizeSetupIncusTrustOffer(trustOffer)
}

func normalizeSetupIncusTrustOffer(raw string) (string, error) {
	if len(raw) > 16<<10 {
		return "", fmt.Errorf("Incus trust offer exceeds 16 KiB")
	}
	trustOffer := strings.TrimSpace(raw)
	if trustOffer == "" || strings.ContainsRune(trustOffer, '\x00') {
		return "", fmt.Errorf("Incus trust offer is empty or invalid")
	}
	return trustOffer, nil
}

func requirePersistedSetupIncusAuthority(cfg *config.Config, authority deployment.Incus) error {
	cfg.Incus = &authority
	stored, found, err := deployment.Load(cfg.DeploymentPath)
	if err != nil {
		return err
	}
	if !found || stored.Incus == nil || *stored.Incus != authority {
		return fmt.Errorf("Incus authority was not committed to the Dorf Deployment")
	}
	accepted := *stored.Incus
	cfg.Incus = &accepted
	return nil
}

func setupIncusAuthoritySummary(authority deployment.Incus) (string, error) {
	authorityHash, err := authority.AuthorityHash()
	if err != nil {
		return "", err
	}
	summary := "Endpoint " + authority.Endpoint + "; server SHA-256 " + authorityHash
	fingerprint, err := authority.ClientCertificateFingerprint()
	if err != nil {
		return "", err
	}
	if fingerprint != "" {
		summary += "; client SHA-256 " + fingerprint
	}
	return summary, nil
}

func containsSandboxProvider(values []core.SandboxProvider, wanted core.SandboxProvider) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func isTerminal(file *os.File) bool {
	return file != nil && term.IsTerminal(file.Fd())
}

func runDoctor(ctx context.Context, db *sql.DB, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("doctor", flag.ContinueOnError)
	set.SetOutput(stderr)
	connection := set.String("ai-connection", "", "named AI connection (default: deployment default)")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	if err := set.Parse(args); err != nil {
		return err
	}
	profile, err := sandboxProfileByNameOrDefault(ctx, postgres.Store{DB: db}, *profileName)
	if err != nil {
		return err
	}
	selectedConnection, err := selectedAIConnection(configuredProviderGateway(cfg), *connection)
	if err != nil {
		return err
	}
	checks := doctor.Run(ctx, db, cfg, profile, selectedConnection)
	checks = appendProfileVerificationCheck(checks, profile)
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(checks); err != nil {
		return err
	}
	if !doctor.Ready(checks) {
		return fmt.Errorf("readiness failed; repair the failed checks above")
	}
	return nil
}

func worker(ctx context.Context, store postgres.Store, client *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("worker", flag.ContinueOnError)
	set.SetOutput(stderr)
	once := set.Bool("once", false, "claim at most one batch and return")
	concurrency := set.Int("concurrency", 1, "maximum concurrent durable tasks")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *concurrency < 1 {
		return fmt.Errorf("worker concurrency must be positive")
	}
	if err := coreApplication(store, client).RecoverCleanupRequests(ctx); err != nil {
		return err
	}
	claimTimeout := cfg.TurnTimeout + 5*time.Minute
	if *once {
		err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: workerID(), ClaimTimeout: claimTimeout, BatchSize: *concurrency})
		if err == nil {
			fmt.Fprintln(stdout, "Absurd delivery reconciled")
		}
		return err
	}
	readerService := controlReaderService(store, client, cfg)
	reader, err := newWorkerControlReader(strings.TrimSpace(os.Getenv("DORF_CONTROL_READER_TOKEN")), readerService)
	if err != nil {
		return err
	}
	workerCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWorkerProcesses(workerCtx, reader,
		func(runCtx context.Context) error {
			return client.RunWorker(runCtx, absurd.WorkerOptions{WorkerID: workerID(), ClaimTimeout: claimTimeout, BatchSize: *concurrency, Concurrency: *concurrency})
		},
		func(runCtx context.Context) error {
			return coreApplication(store, client).ReconcileCleanupRequests(runCtx, time.Second)
		},
		func() { fmt.Fprintln(stdout, "Dorf durable worker started") },
	)
}

func readInput(path, command, noun string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s requires a file with complete %s", command, noun)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s file must be a regular file", noun)
	}
	if info.Size() > 1<<20 {
		return "", fmt.Errorf("%s file exceeds 1 MiB", noun)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := string(contents)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("complete %s cannot be empty", noun)
	}
	return value, nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
func workerID() string {
	hostname, _ := os.Hostname()
	return fmt.Sprintf("%s:%d", hostname, os.Getpid())
}
func openClosed(open bool) string {
	if open {
		return "open"
	}
	return "closed"
}

type taskResultView struct {
	TaskID    string                 `json:"task_id,omitempty"`
	State     absurd.TaskResultState `json:"state,omitempty"`
	Result    json.RawMessage        `json:"result,omitempty"`
	LastError string                 `json:"last_error,omitempty"`
}

func fetchTaskResult(ctx context.Context, client *absurd.Client, taskID string) (taskResultView, error) {
	if taskID == "" {
		return taskResultView{}, nil
	}
	snapshot, err := client.FetchTaskResult(ctx, client.QueueName(), taskID)
	if err != nil {
		return taskResultView{}, err
	}
	if snapshot == nil {
		return taskResultView{TaskID: taskID, State: "missing"}, nil
	}
	return projectTaskResult(taskID, snapshot), nil
}

func projectTaskResult(taskID string, snapshot *absurd.TaskResultSnapshot) taskResultView {
	if snapshot == nil {
		return taskResultView{TaskID: taskID, State: "missing"}
	}
	return taskResultView{
		TaskID:    taskID,
		State:     snapshot.State,
		Result:    snapshot.Result,
		LastError: boundedTaskError(snapshot.Failure),
	}
}

func boundedTaskError(raw json.RawMessage) string {
	var failure struct {
		Message string `json:"message"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &failure) != nil {
		return ""
	}
	message := strings.Join(strings.Fields(failure.Message), " ")
	const maxRunes = 320
	runes := []rune(message)
	if len(runes) > maxRunes {
		message = string(runes[:maxRunes-1]) + "…"
	}
	return message
}

func usage(output io.Writer) error {
	fmt.Fprintln(output, "usage: dorf <version|update|setup|connect|auth|client|serve|integration|migrate|doctor|provider|profile|run|job|workflow|worker|sandbox> [options]")
	return fmt.Errorf("unknown or missing command")
}
