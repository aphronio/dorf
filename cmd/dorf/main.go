package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/blob"
	cloudflareapp "github.com/aphronio/dorf/internal/cloudflare"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/doctor"
	"github.com/aphronio/dorf/internal/gateway"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/hostsetup"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/investigation"
	outcomeapp "github.com/aphronio/dorf/internal/outcome"
	"github.com/aphronio/dorf/internal/postgres"
	profileapp "github.com/aphronio/dorf/internal/profile"
	"github.com/aphronio/dorf/internal/proofbarrier"
	releaseapp "github.com/aphronio/dorf/internal/release"
	"github.com/aphronio/dorf/internal/version"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
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
		} else if result.From == result.Latest {
			fmt.Fprintf(stdout, "Dorf is already up to date: %s\n", result.From)
		} else {
			fmt.Fprintf(stdout, "No update available: running %s; latest published release is %s\n", result.From, result.Latest)
		}
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if args[0] == "setup" {
		return setupCommand(ctx, cfg, args[1:], stdout, stderr)
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
	case "retry":
		client, err := absurdClient(db)
		if err != nil {
			return err
		}
		defer client.Close()
		return retry(ctx, store, client, args[1:], stdout, stderr)
	case "artifact":
		return artifactCommand(ctx, store, blob.Store{Root: cfg.BlobRoot}, args[1:], stdout, stderr)
	case "inspect":
		client, err := absurdClient(db)
		if err != nil {
			return err
		}
		defer client.Close()
		return inspect(ctx, store, client, blob.Store{Root: cfg.BlobRoot}, args[1:], stdout, stderr)
	}
	client, err := application(db, cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	switch args[0] {
	case "admit":
		return admit(ctx, store, client, cfg, args[1:], stdout, stderr)
	case "workflow":
		return workflowCommand(ctx, store, client, cfg, args[1:], stdout, stderr)
	case "message":
		return message(ctx, store, client, args[1:], stdout, stderr)
	case "worker":
		return worker(ctx, store, client, cfg, args[1:], stdout, stderr)
	case "evidence":
		return evidenceCommand(ctx, store, blob.Store{Root: cfg.BlobRoot}, args[1:], stdout, stderr)
	case "cleanup":
		return cleanup(ctx, store, client, args[1:], stdout, stderr)
	case "abandon":
		githubClient := githubapi.Client{APIURL: cfg.GitHubAPIURL, Metadata: cfg.GitHubMetadata, PrivateKey: cfg.GitHubPrivateKey}
		return abandon(ctx, store, client, githubClient, args[1:], stdout)
	default:
		return usage(stderr)
	}
}

func application(db *sql.DB, cfg config.Config) (*absurd.Client, error) {
	client, err := absurdClient(db)
	if err != nil {
		return nil, err
	}
	store := postgres.Store{DB: db}
	barrier, err := proofbarrier.FromEnv()
	if err != nil {
		client.Close()
		return nil, err
	}
	runtimes := profileRuntimeResolver{cfg: cfg, store: store, client: client, barrier: barrier}
	core := coreApplication(store, client)
	core.SandboxRuntimes = runtimes
	core.CleanupRuntimes = runtimes
	core.RegisterCleanup()
	coding.Register(core, store, runtimes)
	investigation.Register(core, store, runtimes)
	return client, nil
}

func coreApplication(store postgres.Store, client *absurd.Client) core.Application {
	return core.Application{Store: store, Tasks: client, AgentMessages: workflowMessageAdmissions{store: store}}
}

// workflowMessageAdmissions is closed-world deployment composition, not a
// Core workflow registry. Each known module retains its policy transaction.
type workflowMessageAdmissions struct{ store postgres.Store }

func (a workflowMessageAdmissions) AdmitAgentMessage(ctx context.Context, input core.MessageAdmission) (core.Message, bool, error) {
	job, err := a.store.Job(ctx, input.JobID)
	if err != nil {
		return core.Message{}, false, err
	}
	switch {
	case job.Workflow == coding.Workflow && job.WorkflowRevision == coding.WorkflowRevision:
		return a.store.AdmitCodingMessage(ctx, input)
	case job.Workflow == investigation.Workflow && job.WorkflowRevision == investigation.WorkflowRevision:
		return a.store.AdmitInvestigationMessage(ctx, input)
	default:
		return core.Message{}, false, fmt.Errorf("workflow %s revision %s does not accept Messages in this deployment", job.Workflow, job.WorkflowRevision)
	}
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
	g, resolvedBind, err := providerGatewayForBind(ctx, store, cfg, *bind, *profileName)
	if err != nil {
		return err
	}
	if authMode == "openai" {
		key, err := readSecretFile(*apiKeyFile, os.Stdin)
		if err != nil {
			return fmt.Errorf("read OpenAI API key: %w", err)
		}
		if err := g.ConnectOpenAIAPIKey(ctx, *name, resolvedBind, key); err != nil {
			return err
		}
		if err := g.SetDefaultConnection(*name); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "AI connection ready: %s (OpenAI API key; default; broker %s on %s)\n", *name, gateway.BackendVersion, resolvedBind)
		return nil
	}
	if err := g.ConnectChatGPT(ctx, *name, resolvedBind, func(url, code string) {
		fmt.Fprintf(stdout, "Open %s and enter %s\n", url, code)
	}); err != nil {
		return err
	}
	if err := g.SetDefaultConnection(*name); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "AI connection ready: %s (ChatGPT subscription; default; broker %s on %s)\n", *name, gateway.BackendVersion, resolvedBind)
	return nil
}

func providerGatewayForBind(ctx context.Context, store postgres.Store, cfg config.Config, bind, profileName string) (gateway.Gateway, string, error) {
	privateBridge := ""
	if strings.TrimSpace(bind) == "" || strings.TrimSpace(profileName) != "" {
		profile, err := sandboxProfileByNameOrDefault(ctx, store, profileName)
		if err != nil {
			return gateway.Gateway{}, "", err
		}
		if profile.Provider == core.SandboxProviderIncus {
			privateBridge = profile.IncusNetwork
		} else if strings.TrimSpace(bind) == "" {
			profiles, listErr := store.SandboxProfiles(ctx)
			if listErr != nil {
				return gateway.Gateway{}, "", listErr
			}
			network, networkErr := gatewayIncusNetwork(profiles, nil)
			if networkErr != nil {
				return gateway.Gateway{}, "", networkErr
			}
			if network == "" {
				bind = "127.0.0.1"
			} else {
				privateBridge = network
				result, runErr := (incus.CommandRunner{}).Run(ctx, "incus", nil, "network", "get", network, "ipv4.address")
				if runErr != nil || result.ExitCode != 0 {
					return gateway.Gateway{}, "", fmt.Errorf("resolve private Incus bridge address; initialize %s or pass --bind", network)
				}
				bind = strings.Split(strings.TrimSpace(result.Stdout), "/")[0]
			}
		}
		if strings.TrimSpace(bind) == "" {
			result, err := (incus.CommandRunner{}).Run(ctx, "incus", nil, "network", "get", profile.IncusNetwork, "ipv4.address")
			if err != nil || result.ExitCode != 0 {
				return gateway.Gateway{}, "", fmt.Errorf("resolve private Incus bridge address; initialize %s or pass --bind", profile.IncusNetwork)
			}
			bind = strings.Split(strings.TrimSpace(result.Stdout), "/")[0]
		}
	}
	if strings.TrimSpace(bind) == "" {
		bind = "127.0.0.1"
	}
	return gateway.Gateway{StatePath: cfg.GatewayStatePath, PrivateBridge: privateBridge}, bind, nil
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
	g := gateway.Gateway{StatePath: cfg.GatewayStatePath}
	selectedConnection, err := selectedAIConnection(g, *connection)
	if err != nil {
		return err
	}
	authorityErr := g.Check(ctx, selectedConnection)
	var sandboxPathErr error
	if profile.Provider == core.SandboxProviderE2B {
		remote, gatewayErr := remoteGatewayForProviderStatus(cfg, profile)
		if gatewayErr != nil {
			sandboxPathErr = gatewayErr
		} else {
			sandboxPathErr = remote.CheckRemote(ctx, profile.E2BGatewayURL)
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
	g := gateway.Gateway{StatePath: cfg.GatewayStatePath}
	if profile.Provider != core.SandboxProviderE2B {
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
	ownedURL, err := cloudflareapp.GatewayURL(state.Hostname)
	if err != nil {
		return g, err
	}
	if ownedURL == profile.E2BGatewayURL {
		client := freshDNSHTTPClient()
		probeURL, err := state.ProbeURL()
		if err != nil {
			return g, err
		}
		g.Client = client
		g.DeploymentProbeURL = probeURL
	}
	return g, nil
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
		Connection: connection, Lifecycle: "persistent host process started by provider connect",
		Authority: check("private broker and named AI connection", authorityErr),
	}
	if profile.BaseVerified() {
		verifiedAt := profile.Verification.ProbeCompletedAt
		view.ProfileVerifiedAt = &verifiedAt
	}
	if profile.Provider == core.SandboxProviderE2B {
		view.SandboxPath = check(profile.E2BGatewayURL, sandboxPathErr)
		if sandboxPathErr == nil {
			view.SandboxPath.Detail = "reachable; anonymous access rejected"
		}
	} else {
		view.SandboxPath = providerGatewayCheckView{
			Status: "historical", Target: "private Incus network " + profile.IncusNetwork,
			Detail: "covered by profile verification; no Sandbox was created for this status check",
		}
	}
	view.Ready = view.ProfileVerified && view.Authority.Status == "ready" &&
		(profile.Provider != core.SandboxProviderE2B || view.SandboxPath.Status == "ready")
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
	Yes              bool
	Connection       string
	ProfileName      string
	AbsurdSchema     string
	SandboxProviders sandboxProviderFlags
	Harness          string
	ConnectionMode   setupConnectionMode
	OpenAIKeyFile    string
	E2BKeyFile       string
	E2BTemplate      string
	GatewayURL       string
	CloudflareHost   string
	AllowInternet    bool
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
	yes := set.Bool("yes", false, "approve every host change shown by setup")
	connection := set.String("ai-connection", "", "named AI connection (default: deployment default)")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	absurdSchema := set.String("absurd-schema", "", "optional local copy of the pinned Absurd schema")
	providers := sandboxProviderFlags{}
	set.Var(&providers, "sandbox-provider", "prepare host requirements for incus or e2b; repeat to select both")
	harness := set.String("harness", "", "Harness for guided profiles: codex or pi")
	connectionMode := set.String("connection-auth", "", "create an AI connection with chatgpt or openai")
	openAIKeyFile := set.String("openai-api-key-file", "", "OpenAI API key file for guided setup")
	e2bKeyFile := set.String("e2b-api-key-file", "", "E2B API key file for guided setup")
	e2bTemplate := set.String("e2b-template", "", "exact E2B template build reference")
	gatewayURL := set.String("gateway-url", "", "existing stable HTTPS /v1 Provider Gateway URL")
	cloudflareHost := set.String("cloudflare-hostname", "", "hostname for guided Cloudflare Tunnel setup")
	allowInternet := set.Bool("allow-internet", false, "allow general internet egress from the guided E2B profile")
	if err := set.Parse(args); err != nil {
		return setupOptions{}, err
	}
	options := setupOptions{
		Yes: *yes, Connection: strings.TrimSpace(*connection),
		ProfileName: strings.TrimSpace(*profileName), AbsurdSchema: strings.TrimSpace(*absurdSchema),
		SandboxProviders: providers,
		Harness:          strings.TrimSpace(*harness), ConnectionMode: setupConnectionMode(strings.TrimSpace(*connectionMode)),
		OpenAIKeyFile: strings.TrimSpace(*openAIKeyFile), E2BKeyFile: strings.TrimSpace(*e2bKeyFile),
		E2BTemplate: strings.TrimSpace(*e2bTemplate), GatewayURL: strings.TrimSpace(*gatewayURL),
		CloudflareHost: strings.TrimSpace(*cloudflareHost), AllowInternet: *allowInternet,
	}
	if err := validateSetupOptions(options); err != nil {
		return setupOptions{}, err
	}
	return options, nil
}

func validateSetupOptions(options setupOptions) error {
	if len(options.SandboxProviders) == 0 && (options.Connection != "" || options.ProfileName != "" || options.Harness != "" || options.ConnectionMode != "" || options.OpenAIKeyFile != "" || options.E2BKeyFile != "" || options.E2BTemplate != "" || options.GatewayURL != "" || options.CloudflareHost != "" || options.AllowInternet) {
		return fmt.Errorf("agent setup flags require at least one --sandbox-provider")
	}
	if options.ProfileName != "" && len(options.SandboxProviders) != 1 {
		return fmt.Errorf("--profile requires exactly one --sandbox-provider")
	}
	if options.GatewayURL != "" && options.CloudflareHost != "" {
		return fmt.Errorf("setup accepts either --gateway-url or --cloudflare-hostname, not both")
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
	if len(options.SandboxProviders) > 0 && !containsSandboxProvider(options.SandboxProviders, core.SandboxProviderE2B) &&
		(options.E2BKeyFile != "" || options.E2BTemplate != "" || options.GatewayURL != "" || options.CloudflareHost != "" || options.AllowInternet) {
		return fmt.Errorf("E2B setup flags require --sandbox-provider e2b")
	}
	return nil
}

func setupCommand(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	options, err := parseSetupOptions(args, stderr)
	if err != nil {
		return err
	}
	presenter := newSetupPresenter(stdout)
	presenter.Welcome()
	plan, err := hostsetup.ObserveHost(ctx, !cfg.DatabaseExternal, false)
	if err != nil {
		return err
	}
	if err := applySetupHostPlan(ctx, plan, options.Yes, stdout, stderr); err != nil {
		return err
	}
	runtimes := plan.RuntimeNames()
	if len(runtimes) == 0 {
		runtimes = []string{"No local runtime required"}
	}
	presenter.Ready("Host runtime", strings.Join(runtimes, " · "))

	migrateArgs := []string{}
	if options.AbsurdSchema != "" {
		migrateArgs = append(migrateArgs, "--absurd-schema", options.AbsurdSchema)
	}
	var db *sql.DB
	var store postgres.Store
	err = presenter.Run(ctx, "Preparing durable state", func(ctx context.Context) error {
		if !cfg.DatabaseExternal {
			database, err := hostsetup.EnsureDatabase(ctx, cfg.DeploymentPath)
			if err != nil {
				return err
			}
			cfg.DatabaseURL, err = database.URL()
			if err != nil {
				return err
			}
		}
		var err error
		db, err = sql.Open("pgx", cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("open PostgreSQL: %w", err)
		}
		store = postgres.Store{DB: db}
		return migrate(ctx, store, migrateArgs, io.Discard, stderr)
	})
	if err != nil {
		if db != nil {
			db.Close()
		}
		return err
	}
	defer db.Close()
	databaseDetail := "PostgreSQL"
	if cfg.DatabaseExternal {
		databaseDetail = "External PostgreSQL"
	}
	presenter.Ready("Durable state", databaseDetail)
	fmt.Fprintln(stdout)

	providers, err := selectSetupSandboxProviders(ctx, cfg, options, presenter)
	if err != nil {
		if errors.Is(err, errSetupCancelled) {
			presenter.Note("Setup paused", "Foundation is ready; no Sandbox provider was prepared")
			return nil
		}
		return err
	}
	if containsSandboxProvider(providers, core.SandboxProviderIncus) {
		providerPlan, err := hostsetup.ObserveHost(ctx, false, true)
		if err != nil {
			if errors.Is(err, hostsetup.ErrKVMUnavailable) {
				return errors.New("Local Sandboxes unavailable\n\nThis machine does not provide KVM hardware virtualization.\nChoose Cloud · E2B, or enable KVM and run setup again.")
			}
			return err
		}
		if err := applySetupHostPlan(ctx, providerPlan, options.Yes, stdout, stderr); err != nil {
			return err
		}
		presenter.Ready("Local Sandbox", "Incus · QEMU · KVM")
	}
	if len(providers) == 0 {
		presenter.Note("Agent Sandboxes", "Skipped for now · run dorf setup again when you’re ready")
		return nil
	}
	return completeGuidedSetup(ctx, store, &cfg, options, providers, presenter, stdout, stderr)
}

var errSetupCancelled = errors.New("setup cancelled")

func selectSetupSandboxProviders(ctx context.Context, cfg config.Config, options setupOptions, presenter setupPresenter) ([]core.SandboxProvider, error) {
	kvmAvailable := hostsetup.KVMDevicePresent()
	selected, settled := deriveSetupSandboxProviders(cfg, options, presenter.interactive, kvmAvailable)
	if settled {
		return selected, nil
	}
	if err := presenter.RunForm(ctx, presenter.ProviderGroup(&selected, kvmAvailable)); err != nil {
		return nil, fmt.Errorf("select Sandbox providers: %w", err)
	}
	return selected, nil
}

func deriveSetupSandboxProviders(cfg config.Config, options setupOptions, interactive, kvmAvailable bool) ([]core.SandboxProvider, bool) {
	selected := append([]core.SandboxProvider{}, options.SandboxProviders...)
	if len(selected) > 0 || options.Yes || !interactive {
		return selected, true
	}
	if !kvmAvailable && strings.TrimSpace(cfg.E2BAPIKey) != "" {
		return []core.SandboxProvider{core.SandboxProviderE2B}, true
	}
	return selected, false
}

func containsSandboxProvider(values []core.SandboxProvider, wanted core.SandboxProvider) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func applySetupHostPlan(ctx context.Context, plan hostsetup.HostPlan, yes bool, stdout, stderr io.Writer) error {
	if !plan.Empty() {
		if err := approveHostPlan(ctx, plan, yes, stdout); err != nil {
			return err
		}
	}
	return hostsetup.ApplyHost(ctx, plan, stdout, stderr)
}

func approveHostPlan(ctx context.Context, plan hostsetup.HostPlan, yes bool, output io.Writer) error {
	if yes || plan.Empty() {
		return nil
	}
	presenter := newSetupPresenter(output)
	if !presenter.interactive {
		fmt.Fprintln(output, "Host changes required:")
		fmt.Fprintln(output, plan.Description())
		return fmt.Errorf("host changes require approval; rerun dorf setup --yes")
	}
	approved := false
	if err := presenter.RunForm(ctx, presenter.ConfirmGroup("Apply these host changes?", plan.Description(), &approved)); err != nil {
		return fmt.Errorf("confirm host changes: %w", err)
	}
	if !approved {
		return errSetupCancelled
	}
	return nil
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func runDoctor(ctx context.Context, db *sql.DB, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("doctor", flag.ContinueOnError)
	set.SetOutput(stderr)
	connection := set.String("ai-connection", "", "named AI connection (default: deployment default)")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	cloneURL := set.String("repo", "", "managed GitHub clone URL")
	githubRepository := set.String("github-repo", "", "canonical lower-case owner/repository")
	githubInstallation := set.String("github-installation", "", "GitHub App installation identity")
	base := set.String("base", "", "explicit GitHub base branch")
	if err := set.Parse(args); err != nil {
		return err
	}
	profile, err := sandboxProfileByNameOrDefault(ctx, postgres.Store{DB: db}, *profileName)
	if err != nil {
		return err
	}
	selectedConnection, err := selectedAIConnection(gateway.Gateway{StatePath: cfg.GatewayStatePath}, *connection)
	if err != nil {
		return err
	}
	checks := doctor.Run(ctx, db, cfg, profile, selectedConnection)
	checks = appendProfileVerificationCheck(checks, profile)
	githubValues := []string{*cloneURL, *githubRepository, *githubInstallation, *base}
	wantsGitHub := false
	completeGitHub := true
	for _, value := range githubValues {
		wantsGitHub = wantsGitHub || strings.TrimSpace(value) != ""
		completeGitHub = completeGitHub && strings.TrimSpace(value) != ""
	}
	if wantsGitHub {
		check := doctor.Check{Name: "github-repository-authority", Status: "failed"}
		if !completeGitHub {
			check.Detail = "--repo, --github-repo, --github-installation, and --base are required together"
		} else if validateErr := githubapi.ValidateAuthority(*cloneURL, *githubRepository, *githubInstallation, *base, "dorf/readiness-probe"); validateErr != nil {
			check.Detail = validateErr.Error()
		} else {
			client := githubapi.Client{APIURL: cfg.GitHubAPIURL, Metadata: cfg.GitHubMetadata, PrivateKey: cfg.GitHubPrivateKey}
			_, exists, authorityErr := client.RemoteHead(ctx, githubapi.Authority{Repository: *githubRepository, InstallationID: *githubInstallation}, *base)
			if authorityErr != nil {
				check.Detail = authorityErr.Error() + "; install the Dorf GitHub App for this repository with contents and pull-request authority"
			} else if !exists {
				check.Detail = "base branch was not found through the configured GitHub App"
			} else {
				check.Status, check.Detail = "ready", "ready"
			}
		}
		checks = append(checks, check)
	}
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

func admit(ctx context.Context, store postgres.Store, client *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("admit", flag.ContinueOnError)
	set.SetOutput(stderr)
	key := set.String("key", "", "stable caller admission identity")
	goalFile := set.String("goal-file", "", "path containing the complete goal")
	repository := set.String("repo", "", "clone URL")
	revision := set.String("revision", "", "exact commit or admitted starting Revision")
	branch := set.String("branch", "", "Job branch (default dorf/<Job ID>)")
	githubRepository := set.String("github-repo", "", "canonical lower-case GitHub owner/repository")
	githubInstallation := set.String("github-installation", "", "GitHub App installation identity")
	base := set.String("base", "", "explicit immutable GitHub base branch")
	connection := set.String("ai-connection", "", "named AI connection (default: deployment default)")
	model := set.String("model", "", "Codex model")
	effort := set.String("reasoning", "high", "Codex reasoning effort")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	if err := set.Parse(args); err != nil {
		return err
	}
	goal, err := readInput(*goalFile, "admit", "goal")
	if err != nil {
		return err
	}
	if strings.TrimSpace(*branch) == "" && strings.TrimSpace(*key) != "" {
		*branch = "dorf/" + core.JobID(strings.TrimSpace(*key))
	}
	providers := gateway.Gateway{StatePath: cfg.GatewayStatePath}
	profile, err := selectedSandboxProfile(ctx, store, *profileName)
	if err != nil {
		return err
	}
	if err := requireRemoteGitAccess(profile); err != nil {
		return err
	}
	selectedConnection, err := selectedAIConnection(providers, *connection)
	if err != nil {
		return err
	}
	job, created, err := coding.Admit(ctx, store, coreApplication(store, client), providers, profileapp.Runtime{SandboxProfile: profile.Name}, coding.Admission{
		JobAdmission: core.JobAdmission{AdmissionKey: *key, Goal: goal, SandboxProfile: profile.Name, ProviderConnection: selectedConnection, Model: *model, ReasoningEffort: *effort},
		Repository:   *repository, Revision: *revision, Branch: *branch, GitHubRepository: *githubRepository, GitHubInstallation: *githubInstallation, BaseBranch: *base,
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": job.ID, "workflow": job.Workflow, "workflow_revision": job.WorkflowRevision, "created": created, "task_id": job.CurrentTaskID, "scheduled": true})
}

func workflowCommand(ctx context.Context, store postgres.Store, client *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("workflow requires: run codebase-investigation")
	}
	if len(args) < 2 || args[0] != "run" {
		return fmt.Errorf("workflow requires: run codebase-investigation")
	}
	if args[1] != string(investigation.Workflow) {
		return fmt.Errorf("unsupported workflow %q", args[1])
	}
	set := flag.NewFlagSet("workflow run codebase-investigation", flag.ContinueOnError)
	set.SetOutput(stderr)
	key := set.String("key", "", "stable caller admission identity")
	briefFile := set.String("brief-file", "", "path containing the complete investigation brief")
	repositoryURL := set.String("repo", "", "clone URL")
	localRepository := set.String("local-repo", "", "local Git repository containing the committed Revision")
	revision := set.String("revision", "", "exact repository Revision (default HEAD with --local-repo)")
	connection := set.String("ai-connection", "", "named AI connection (default: deployment default)")
	model := set.String("model", "", "Harness model")
	effort := set.String("reasoning", "high", "Harness reasoning effort")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	if err := set.Parse(args[2:]); err != nil {
		return err
	}
	brief, err := readInput(*briefFile, "workflow run codebase-investigation", "brief")
	if err != nil {
		return err
	}
	profile, err := selectedSandboxProfile(ctx, store, *profileName)
	if err != nil {
		return err
	}
	providers := gateway.Gateway{StatePath: cfg.GatewayStatePath}
	selectedConnection, err := selectedAIConnection(providers, *connection)
	if err != nil {
		return err
	}
	source, workingTreeChangesExcluded, err := prepareInvestigationSource(ctx, blob.Store{Root: cfg.BlobRoot}, *repositoryURL, *localRepository, *revision)
	if err != nil {
		return err
	}
	if source.Kind == investigation.SourceRemote {
		if err := requireRemoteGitAccess(profile); err != nil {
			return err
		}
	}
	input := investigation.Admission{
		JobAdmission: core.JobAdmission{AdmissionKey: *key, Goal: brief, SandboxProfile: profile.Name, ProviderConnection: selectedConnection, Model: *model, ReasoningEffort: *effort},
		Source:       source,
	}
	job, created, err := investigation.Admit(ctx, store, coreApplication(store, client), providers, profileapp.Runtime{SandboxProfile: profile.Name}, input)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{
		"job_id": job.ID, "workflow": job.Workflow, "workflow_revision": job.WorkflowRevision,
		"required_provider_capabilities": investigation.WorkflowDefinition().RequiredProviderCapabilities,
		"created":                        created, "task_id": job.CurrentTaskID, "scheduled": true,
		"source": source, "working_tree_changes_excluded": workingTreeChangesExcluded,
	})
}

func requireRemoteGitAccess(profile core.SandboxProfile) error {
	if profile.Provider == core.SandboxProviderE2B && !profile.E2BAllowInternet {
		return fmt.Errorf("Sandbox profile %q blocks internet access and cannot use a remote Git source; use --local-repo when supported, or update and reverify the profile with internet access", profile.Name)
	}
	return nil
}

func message(ctx context.Context, store postgres.Store, client *absurd.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("message", flag.ContinueOnError)
	set.SetOutput(stderr)
	jobID := set.String("job", "", "existing Job ID")
	requestID := set.String("id", "", "stable human request identity")
	inputFile := set.String("input-file", "", "path containing the complete message input")
	intent := set.String("intent", string(core.MessageFollow), "harness delivery intent: follow or steer")
	if err := set.Parse(args); err != nil {
		return err
	}
	input, err := readInput(*inputFile, "message", "input")
	if err != nil {
		return err
	}
	application := coreApplication(store, client)
	jobHandle, err := application.OpenJob(ctx, *jobID)
	if err != nil {
		return err
	}
	sandbox, err := jobHandle.DefaultSandbox(ctx)
	if err != nil {
		return err
	}
	options := []core.MessageOption(nil)
	switch core.MessageDeliveryIntent(*intent) {
	case core.MessageFollow:
	case core.MessageSteer:
		options = append(options, core.Steer())
	default:
		return fmt.Errorf("message intent must be follow or steer")
	}
	accepted, err := sandbox.Agent().Message(ctx, *requestID, input, options...)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": accepted.JobID, "message_id": accepted.MessageID, "sequence": accepted.Sequence, "intent": accepted.Intent, "target_turn_id": accepted.TargetTurnID, "created": accepted.Created, "accepted": true, "delivery": "queued"})
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
	workerCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCtx, cancel := context.WithCancel(workerCtx)
	defer cancel()
	recoveryDone := make(chan error, 1)
	go func() {
		err := coreApplication(store, client).ReconcileCleanupRequests(runCtx, time.Second)
		recoveryDone <- err
		if err != nil && !errors.Is(err, context.Canceled) {
			cancel()
		}
	}()
	fmt.Fprintln(stdout, "Dorf durable worker started")
	err := client.RunWorker(runCtx, absurd.WorkerOptions{WorkerID: workerID(), ClaimTimeout: claimTimeout, BatchSize: *concurrency, Concurrency: *concurrency})
	cancel()
	recoveryErr := <-recoveryDone
	if recoveryErr != nil && !errors.Is(recoveryErr, context.Canceled) {
		return recoveryErr
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func inspect(ctx context.Context, store postgres.Store, client *absurd.Client, evidenceStore blob.Store, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("inspect", flag.ContinueOnError)
	set.SetOutput(stderr)
	jsonOutput := set.Bool("json", false, "render JSON")
	followOutput := set.Bool("follow", false, "follow durable Job history until attention or cleanup completes")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *jsonOutput && *followOutput {
		return fmt.Errorf("inspect --json and --follow cannot be combined")
	}
	if set.NArg() != 1 {
		return fmt.Errorf("inspect requires one Job ID")
	}
	job, err := store.Job(ctx, set.Arg(0))
	if err != nil {
		return err
	}
	profile, err := store.SandboxProfile(ctx, job.SandboxProfile)
	if err != nil {
		return err
	}
	if *followOutput {
		followCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return followJob(followCtx, store, client, evidenceStore, job.ID, stdout)
	}
	if job.Workflow == investigation.Workflow {
		return inspectCodebaseInvestigation(ctx, store, client, job, profile, *jsonOutput, stdout)
	}
	if job.Workflow != coding.Workflow {
		return fmt.Errorf("inspect does not support workflow %q", job.Workflow)
	}
	snapshot, err := coding.LoadSnapshot(ctx, store, set.Arg(0))
	if err != nil {
		return err
	}
	codingJob := snapshot.Job
	job = codingJob.Job
	projection, err := snapshot.Project(evidenceStore)
	if err != nil {
		return err
	}
	currentWork := projection.CurrentWork
	assessment := projection.Readiness
	history := workflowHistory(snapshot)
	definition := coding.WorkflowDefinition()
	executions, err := fetchJobTaskExecutions(ctx, store, client, job)
	if err != nil {
		return err
	}
	currentExecution := currentTaskExecution(executions)
	executionOperation := currentWork.Description()
	if operation, ok := cleanupOperation(definition, job, snapshot.Sandboxes, snapshot.Actions); ok {
		executionOperation = operation
	}
	if *jsonOutput {
		messages := make([]core.Message, 0, len(snapshot.Deliveries))
		agentRuns := make([]core.AgentRun, 0, len(snapshot.Deliveries))
		for _, delivery := range snapshot.Deliveries {
			messages = append(messages, delivery.Message)
			agentRuns = append(agentRuns, delivery.AgentRun)
		}
		view := map[string]any{
			"job":                            codingJob,
			"sandbox_profile":                profileView(profile),
			"current_work":                   currentWork,
			"readiness":                      assessment,
			"required_provider_capabilities": coding.WorkflowDefinition().RequiredProviderCapabilities,
			"proposal":                       snapshot.Proposal,
			"outcome":                        snapshot.Outcome,
			"observed_facts": map[string]any{
				"actions": snapshot.Actions, "agent_runs": agentRuns, "revisions": snapshot.Revisions,
				"evidence": snapshot.Evidence, "review_plans": snapshot.ReviewPlans,
				"sandboxes": snapshot.Sandboxes, "messages": messages,
			},
			"execution": executions,
		}
		return writeJSON(stdout, view)
	}
	readiness := "not ready"
	if assessment.Ready {
		readiness = "ready"
	}
	fmt.Fprintf(stdout, "Job %s\n  workflow: %s revision %s\n", job.ID, job.Workflow, job.WorkflowRevision)
	renderWorkflowExecutionAttention(stdout, job, currentExecution, executionOperation)
	fmt.Fprintf(stdout, "  goal: %s\n  repository: %s\n  current Revision: %s\n  required provider capabilities: %s\n  Sandbox profile: %s · %s · %s\n  admission: %s\n  cleanup: %s\n  readiness: %s — %s\n", job.Goal, codingJob.Repository, codingJob.Revision, joinProviderCapabilities(definition.RequiredProviderCapabilities), profile.Name, profile.Provider, profile.Harness, openClosed(job.AdmissionOpen), job.CleanupState, readiness, assessment.Reason)
	renderWorkflow(stdout, currentWork)
	if job.WorkflowAttention != "" {
		fmt.Fprintf(stdout, "  attention: %s\n", job.WorkflowAttention)
		renderWorkflowAttentionRecovery(stdout, job, currentExecution)
	}
	if job.CleanupAttention != "" {
		fmt.Fprintf(stdout, "  cleanup attention: %s\n", job.CleanupAttention)
	}
	if snapshot.Proposal == nil {
		fmt.Fprintln(stdout, "  proposal: none")
	} else {
		fmt.Fprintf(stdout, "  proposal: #%d %s Revision=%s", snapshot.Proposal.Number, snapshot.Proposal.URL, snapshot.Proposal.ProposedRevision)
		if snapshot.Proposal.ProposedRevision != codingJob.Revision {
			fmt.Fprint(stdout, " (stale)")
		}
		fmt.Fprintln(stdout)
	}
	if snapshot.Outcome == nil {
		fmt.Fprintln(stdout, "  outcome: none")
	} else {
		fmt.Fprintf(stdout, "  outcome: %s", snapshot.Outcome.Kind)
		if snapshot.Outcome.ObservedState != "" {
			fmt.Fprintf(stdout, " (GitHub %s)", snapshot.Outcome.ObservedState)
		}
		if snapshot.Outcome.MergeCommitOID != "" {
			fmt.Fprintf(stdout, " merge=%s", snapshot.Outcome.MergeCommitOID)
		}
		fmt.Fprintf(stdout, " observed-at=%s\n", snapshot.Outcome.ObservedAt.Format(time.RFC3339Nano))
	}
	renderHistory(stdout, history)
	return nil
}

func inspectCodebaseInvestigation(ctx context.Context, store postgres.Store, client *absurd.Client, job core.Job, profile core.SandboxProfile, jsonOutput bool, stdout io.Writer) error {
	snapshot, err := investigation.LoadSnapshot(ctx, store, job.ID)
	if err != nil {
		return err
	}
	work := snapshot.Project()
	artifacts, err := store.Artifacts(ctx, job.ID)
	if err != nil {
		return err
	}
	if artifacts == nil {
		artifacts = []core.Artifact{}
	}
	executions, err := fetchJobTaskExecutions(ctx, store, client, job)
	if err != nil {
		return err
	}
	currentExecution := currentTaskExecution(executions)
	definition := investigation.WorkflowDefinition()
	executionOperation := work.Description()
	if operation, ok := cleanupOperation(definition, job, []core.Sandbox{snapshot.MainSandbox}, snapshot.Actions); ok {
		executionOperation = operation
	}
	if jsonOutput {
		return writeJSON(stdout, map[string]any{
			"job": job, "source": snapshot.Source, "sandbox_profile": profileView(profile), "current_work": work, "drafts": snapshot.Drafts, "artifacts": artifacts,
			"required_provider_capabilities": definition.RequiredProviderCapabilities,
			"observed_facts":                 map[string]any{"actions": snapshot.Actions, "agent_runs": investigationAgentRuns(snapshot.Deliveries), "sandbox": snapshot.MainSandbox},
			"execution":                      executions,
		})
	}
	fmt.Fprintf(stdout, "Job %s\n  workflow: %s revision %s\n", job.ID, job.Workflow, job.WorkflowRevision)
	renderWorkflowExecutionAttention(stdout, job, currentExecution, executionOperation)
	fmt.Fprintf(stdout, "  brief: %s\n  source: %s\n  exact Revision: %s\n  required provider capabilities: %s\n  Sandbox profile: %s · %s · %s\n  admission: %s\n  cleanup: %s\n",
		job.Goal, investigationSourceSummary(snapshot.Source), snapshot.Source.Revision, joinProviderCapabilities(definition.RequiredProviderCapabilities), profile.Name, profile.Provider, profile.Harness, openClosed(job.AdmissionOpen), job.CleanupState)
	fmt.Fprintf(stdout, "  current work: %s", work.Kind)
	if work.Detail != "" {
		fmt.Fprintf(stdout, " — %s", work.Detail)
	}
	fmt.Fprintln(stdout)
	if job.WorkflowAttention != "" {
		fmt.Fprintf(stdout, "  attention: %s\n", job.WorkflowAttention)
		renderWorkflowAttentionRecovery(stdout, job, currentExecution)
	}
	if job.CleanupAttention != "" {
		fmt.Fprintf(stdout, "  cleanup attention: %s\n", job.CleanupAttention)
	}
	renderHistory(stdout, investigationHistory(snapshot))
	if len(snapshot.Drafts) == 0 {
		fmt.Fprintln(stdout, "  draft: none")
		return nil
	}
	latest := snapshot.Drafts[len(snapshot.Drafts)-1]
	fmt.Fprintf(stdout, "  latest draft: created-at=%s Artifact=%s\n", latest.CreatedAt.Format(time.RFC3339Nano), latest.ArtifactID)
	fmt.Fprintf(stdout, "  retrieve: dorf artifact get %s\n", latest.ArtifactID)
	if job.AdmissionOpen && job.CleanupState == core.CleanupPending {
		fmt.Fprintf(stdout, "  revise: dorf message --job %s --id REQUEST_ID --input-file FOLLOW_UP.md\n", job.ID)
		fmt.Fprintf(stdout, "  release resources: dorf cleanup %s\n", job.ID)
	}
	return nil
}

func investigationAgentRuns(deliveries []core.Delivery) []core.AgentRun {
	runs := make([]core.AgentRun, 0, len(deliveries))
	for _, delivery := range deliveries {
		runs = append(runs, delivery.AgentRun)
	}
	return runs
}

func investigationSourceSummary(source investigation.Source) string {
	if source.Kind == investigation.SourceGitBundle {
		return fmt.Sprintf("retained Git bundle sha256:%s (%d bytes)", source.BundleDigest, source.BundleByteSize)
	}
	return "remote Git " + source.Repository
}

func joinProviderCapabilities(capabilities []profileapp.Capability) string {
	if len(capabilities) == 0 {
		return "none"
	}
	values := make([]string, len(capabilities))
	for i := range capabilities {
		values[i] = string(capabilities[i])
	}
	return strings.Join(values, ", ")
}

func retry(ctx context.Context, store postgres.Store, client *absurd.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("retry", flag.ContinueOnError)
	set.SetOutput(stderr)
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("retry requires one Job ID")
	}
	receipt, err := coreApplication(store, client).RetryFailedJob(ctx, set.Arg(0))
	if err != nil {
		return err
	}
	return writeJSON(stdout, receipt)
}

func evidenceCommand(ctx context.Context, store postgres.Store, evidenceStore blob.Store, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("evidence", flag.ContinueOnError)
	set.SetOutput(stderr)
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 2 || set.Arg(0) != "verify" {
		return fmt.Errorf("evidence requires: verify JOB_ID")
	}
	job, err := store.CodingJob(ctx, set.Arg(1))
	if err != nil {
		return err
	}
	records, err := store.Evidence(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := evidenceStore.Verify(record.Digest, record.ByteSize); err != nil {
			return fmt.Errorf("Evidence %s: %w", record.ID, err)
		}
		fmt.Fprintf(stdout, "%s sha256=%s bytes=%d verified\n", record.ID, record.Digest, record.ByteSize)
	}
	fmt.Fprintf(stdout, "Revision %s: retained Evidence blobs independently verified\n", job.Revision)
	return nil
}

func cleanup(ctx context.Context, store postgres.Store, client *absurd.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	set.SetOutput(stderr)
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("cleanup requires one Job ID")
	}
	application := coreApplication(store, client)
	handle, err := application.OpenJob(ctx, set.Arg(0))
	if err != nil {
		return err
	}
	if err := handle.RequestCleanup(ctx); err != nil {
		return err
	}
	job, err := store.Job(ctx, handle.ID())
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": job.ID, "cleanup": job.CleanupState, "task_id": job.CurrentTaskID, "scheduled": job.CleanupState == core.CleanupScheduled})
}

func abandon(ctx context.Context, store postgres.Store, client *absurd.Client, githubClient githubapi.Client, args []string, stdout io.Writer) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("abandon requires one Job ID")
	}
	// This interactive command is the outcome authority; unlike a workflow
	// executor, it has no Absurd claim to revalidate before the write.
	direct := (outcomeapp.Service{Store: store, GitHub: githubClient}).WithClaimCheck(func(context.Context) error { return nil })
	receipt, created, err := direct.Record(ctx, strings.TrimSpace(args[0]), coding.OutcomeAbandoned)
	if err != nil {
		return err
	}
	application := coreApplication(store, client)
	handle, err := application.OpenJob(ctx, receipt.JobID)
	if err != nil {
		return fmt.Errorf("%s outcome receipt was retained, but loading its Job failed: %w", receipt.Kind, err)
	}
	if err := handle.RequestCleanup(ctx); err != nil {
		return fmt.Errorf("%s outcome receipt was retained, but durable cleanup scheduling failed: %w", receipt.Kind, err)
	}
	job, err := store.Job(ctx, handle.ID())
	if err != nil {
		return fmt.Errorf("%s outcome receipt was retained, but reloading cleanup state failed: %w", receipt.Kind, err)
	}
	return writeJSON(stdout, map[string]any{"outcome": receipt, "created": created, "cleanup": job.CleanupState, "task_id": job.CurrentTaskID})
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
func empty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
func openClosed(open bool) string {
	if open {
		return "open"
	}
	return "closed"
}

func renderWorkflow(output io.Writer, work coding.Work) {
	fmt.Fprintln(output, "\nWorkflow")
	fmt.Fprintln(output, "  Sandbox → repository clone → provider Route → Message → AgentRun → Revision → ReviewPolicy")
	fmt.Fprintln(output, "                                                ↑                                  │")
	fmt.Fprintln(output, "                                                └──── feedback ← review AgentRun ←──┤ review")
	fmt.Fprintln(output, "                                                                                   └ no review")
	fmt.Fprintln(output, "  ready exact Revision → Proposal → Outcome → Cleanup")
	fmt.Fprintf(output, "  → current: %s", work.Description())
	if work.Detail != "" {
		fmt.Fprintf(output, " — %s", work.Detail)
	}
	fmt.Fprintln(output)
}

type taskResultView struct {
	TaskID    string                 `json:"task_id,omitempty"`
	State     absurd.TaskResultState `json:"state,omitempty"`
	Result    json.RawMessage        `json:"result,omitempty"`
	LastError string                 `json:"last_error,omitempty"`
}

type jobTaskExecutionView struct {
	Attachment core.JobTask   `json:"attachment"`
	Current    bool           `json:"current"`
	Execution  taskResultView `json:"execution"`
}

func fetchJobTaskExecutions(ctx context.Context, store postgres.Store, client *absurd.Client, job core.Job) ([]jobTaskExecutionView, error) {
	attachments, err := store.JobTasks(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	executions := make([]jobTaskExecutionView, 0, len(attachments))
	for _, attachment := range attachments {
		execution, err := fetchTaskResult(ctx, client, attachment.TaskID)
		if err != nil {
			return nil, err
		}
		executions = append(executions, jobTaskExecutionView{
			Attachment: attachment,
			Current:    attachment.TaskID == job.CurrentTaskID,
			Execution:  execution,
		})
	}
	return executions, nil
}

func currentTaskExecution(executions []jobTaskExecutionView) taskResultView {
	for i := len(executions) - 1; i >= 0; i-- {
		if executions[i].Current {
			return executions[i].Execution
		}
	}
	return taskResultView{}
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

func renderWorkflowExecutionAttention(output io.Writer, job core.Job, execution taskResultView, operation string) {
	if execution.State != absurd.TaskFailed || job.CleanupState == core.CleanupComplete {
		return
	}
	label := "workflow stopped"
	if job.CleanupState == core.CleanupScheduled {
		label = "cleanup stopped"
	}
	fmt.Fprintln(output, "  attention: "+label)
	if operation = strings.TrimSpace(operation); operation != "" {
		fmt.Fprintf(output, "  operation: %s\n", operation)
	}
	if execution.LastError != "" {
		fmt.Fprintf(output, "  reason: %s\n", execution.LastError)
	}
	fmt.Fprintf(output, "  next: repair the cause, then run dorf retry %s\n", job.ID)
}

func renderWorkflowAttentionRecovery(output io.Writer, job core.Job, execution taskResultView) {
	if execution.State == absurd.TaskCompleted && job.CleanupState == core.CleanupPending {
		fmt.Fprintf(output, "  next: run dorf cleanup %s to release resources\n", job.ID)
	}
}

func usage(output io.Writer) error {
	fmt.Fprintln(output, "usage: dorf <version|update|setup|migrate|doctor|provider|profile|workflow|artifact|admit|message|worker|inspect|retry|evidence|abandon|cleanup> [options]")
	return fmt.Errorf("unknown or missing command")
}
