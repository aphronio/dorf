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
	"strings"
	"syscall"
	"time"

	"charm.land/huh/v2"
	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/doctor"
	"github.com/aphronio/dorf/internal/gateway"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/hostsetup"
	"github.com/aphronio/dorf/internal/incus"
	outcomeapp "github.com/aphronio/dorf/internal/outcome"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/proofbarrier"
	releaseapp "github.com/aphronio/dorf/internal/release"
	"github.com/aphronio/dorf/internal/repository"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/version"
	"github.com/aphronio/dorf/internal/workflow"
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
	case "setup-retry":
		return setupRetry(ctx, store, client, args[1:], stdout, stderr)
	case "worker":
		return worker(ctx, client, cfg, args[1:], stdout, stderr)
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
	workflow.Register(client, store, profileRuntimeResolver{cfg: cfg, store: store, client: client, barrier: barrier})
	return client, nil
}

func absurdClient(db *sql.DB) (*absurd.Client, error) {
	return absurd.New(absurd.Options{DB: db, QueueName: config.QueueName, DefaultMaxAttempts: 5})
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
		return fmt.Errorf("provider requires: connect chatgpt or status")
	}
	if args[0] == "status" {
		return providerStatusCommand(ctx, store, cfg, args[1:], stdout, stderr)
	}
	if args[0] != "connect" || len(args) < 2 || args[1] != "chatgpt" {
		return fmt.Errorf("the supported provider commands are: provider connect chatgpt and provider status")
	}
	set := flag.NewFlagSet("provider connect chatgpt", flag.ContinueOnError)
	set.SetOutput(stderr)
	name := set.String("name", "personal-chatgpt", "stable Provider Connection name")
	bind := set.String("bind", "", "exact broker bind IP; a non-loopback address requires its matching Incus --profile")
	profileName := set.String("profile", "", "Incus profile used to resolve its private bridge")
	if err := set.Parse(args[2:]); err != nil {
		return err
	}
	privateBridge := ""
	if strings.TrimSpace(*bind) == "" || strings.TrimSpace(*profileName) != "" {
		profile, err := sandboxProfileByNameOrDefault(ctx, store, *profileName)
		if err != nil {
			return err
		}
		if profile.Provider == spine.SandboxProviderIncus {
			privateBridge = profile.IncusNetwork
		} else if strings.TrimSpace(*bind) == "" {
			return fmt.Errorf("Sandbox profile %q uses %s; remote deployments require an explicit --bind address", profile.Name, profile.Provider)
		}
		if strings.TrimSpace(*bind) == "" {
			result, err := (incus.CommandRunner{}).Run(ctx, "incus", nil, "network", "get", profile.IncusNetwork, "ipv4.address")
			if err != nil || result.ExitCode != 0 {
				return fmt.Errorf("resolve private Incus bridge address; initialize %s or pass --bind", profile.IncusNetwork)
			}
			*bind = strings.Split(strings.TrimSpace(result.Stdout), "/")[0]
		}
	}
	g := gateway.Gateway{StatePath: cfg.GatewayStatePath, PrivateBridge: privateBridge}
	if err := g.ConnectChatGPT(ctx, *name, *bind, func(url, code string) {
		fmt.Fprintf(stdout, "Open %s and enter %s\n", url, code)
	}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Provider Connection ready: %s (ChatGPT subscription; broker %s on %s)\n", *name, gateway.BackendVersion, *bind)
	return nil
}

type providerGatewayCheckView struct {
	Status string `json:"status"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail"`
}

type providerGatewayStatusView struct {
	Profile           string                   `json:"profile"`
	SandboxProvider   spine.SandboxProvider    `json:"sandbox_provider"`
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
	connection := set.String("name", "personal-chatgpt", "stable Provider Connection name")
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
	authorityErr := g.Check(ctx, *connection)
	var sandboxPathErr error
	if profile.Provider == spine.SandboxProviderE2B {
		sandboxPathErr = g.CheckRemote(ctx, profile.E2BGatewayURL)
	}
	view := newProviderGatewayStatusView(profile, strings.TrimSpace(*connection), authorityErr, sandboxPathErr)
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

func newProviderGatewayStatusView(profile spine.SandboxProfile, connection string, authorityErr, sandboxPathErr error) providerGatewayStatusView {
	check := func(target string, err error) providerGatewayCheckView {
		if err != nil {
			return providerGatewayCheckView{Status: "failed", Target: target, Detail: err.Error()}
		}
		return providerGatewayCheckView{Status: "ready", Target: target, Detail: "ready"}
	}
	view := providerGatewayStatusView{
		Profile: profile.Name, SandboxProvider: profile.Provider, ProfileVerified: profile.BaseVerified(),
		Connection: connection, Lifecycle: "persistent host process started by provider connect",
		Authority: check("private broker and named Provider Connection", authorityErr),
	}
	if profile.BaseVerified() {
		verifiedAt := profile.Verification.ProbeCompletedAt
		view.ProfileVerifiedAt = &verifiedAt
	}
	if profile.Provider == spine.SandboxProviderE2B {
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
		(profile.Provider != spine.SandboxProviderE2B || view.SandboxPath.Status == "ready")
	switch {
	case !view.ProfileVerified:
		view.Impact = "new Jobs cannot use this Sandbox profile"
		view.Next = "run dorf profile verify " + profile.Name
	case view.Authority.Status != "ready":
		view.Impact = "new AgentRuns cannot obtain authenticated inference routes"
		view.Next = "restore the named Provider Connection and private broker, then rerun provider status"
	case profile.Provider == spine.SandboxProviderE2B && view.SandboxPath.Status != "ready":
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
	Yes          bool
	Connection   string
	ProfileName  string
	AbsurdSchema string
}

func parseSetupOptions(args []string, stderr io.Writer) (setupOptions, error) {
	set := flag.NewFlagSet("setup", flag.ContinueOnError)
	set.SetOutput(stderr)
	yes := set.Bool("yes", false, "approve every host change shown by setup")
	connection := set.String("provider", "", "named Provider Connection")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	absurdSchema := set.String("absurd-schema", "", "optional local copy of the pinned Absurd schema")
	if err := set.Parse(args); err != nil {
		return setupOptions{}, err
	}
	return setupOptions{
		Yes: *yes, Connection: strings.TrimSpace(*connection),
		ProfileName: strings.TrimSpace(*profileName), AbsurdSchema: strings.TrimSpace(*absurdSchema),
	}, nil
}

func setupCommand(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	options, err := parseSetupOptions(args, stderr)
	if err != nil {
		return err
	}
	plan, err := hostsetup.ObserveHost(ctx, !cfg.DatabaseExternal)
	if err != nil {
		return err
	}
	if !plan.Empty() {
		if err := approveHostPlan(ctx, plan, options.Yes, stdout); err != nil {
			return err
		}
	}
	if err := hostsetup.ApplyHost(ctx, plan, stdout, stderr); err != nil {
		return err
	}
	if !cfg.DatabaseExternal {
		database, err := hostsetup.EnsureDatabase(ctx, cfg.DeploymentPath, stdout)
		if err != nil {
			return err
		}
		cfg.DatabaseURL, err = database.URL()
		if err != nil {
			return err
		}
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer db.Close()
	store := postgres.Store{DB: db}
	migrateArgs := []string{}
	if options.AbsurdSchema != "" {
		migrateArgs = append(migrateArgs, "--absurd-schema", options.AbsurdSchema)
	}
	if err := migrate(ctx, store, migrateArgs, stdout, stderr); err != nil {
		return err
	}
	if options.Connection == "" {
		fmt.Fprintln(stdout, "Dorf storage is ready. Next: create and verify a Sandbox profile, connect a Provider, then rerun dorf setup --provider NAME")
		return nil
	}
	profile, err := sandboxProfileByNameOrDefault(ctx, store, options.ProfileName)
	if err != nil {
		return err
	}
	checks := doctor.Run(ctx, db, cfg, profile, options.Connection)
	checks = appendProfileVerificationCheck(checks, profile)
	if err := json.NewEncoder(stdout).Encode(checks); err != nil {
		return err
	}
	if !doctor.Ready(checks) {
		return fmt.Errorf("setup is not converged; apply the failed check remediations and rerun this command")
	}
	fmt.Fprintf(stdout, "Dorf is ready: PostgreSQL, Absurd, Sandbox profile %s, Harness %s, and Provider Gateway checks passed\n", profile.Name, profile.Harness)
	return nil
}

func approveHostPlan(ctx context.Context, plan hostsetup.HostPlan, yes bool, output io.Writer) error {
	if yes || plan.Empty() {
		return nil
	}
	inputFile := os.Stdin
	outputFile, outputIsFile := output.(*os.File)
	if !outputIsFile || !isTerminal(inputFile) || !isTerminal(outputFile) {
		fmt.Fprintln(output, "Host changes required:")
		fmt.Fprintln(output, plan.Description())
		return fmt.Errorf("host changes require approval; rerun dorf setup --yes")
	}
	approved := false
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Apply these host changes?").
			Description(plan.Description()).
			Affirmative("Apply").
			Negative("Cancel").
			Value(&approved),
	)).WithInput(inputFile).WithOutput(outputFile).WithAccessible(strings.TrimSpace(os.Getenv("ACCESSIBLE")) != "")
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return fmt.Errorf("setup cancelled")
		}
		return fmt.Errorf("confirm host changes: %w", err)
	}
	if !approved {
		return fmt.Errorf("setup cancelled")
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
	connection := set.String("provider", "", "named Provider Connection")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	cloneURL := set.String("repo", "", "managed GitHub clone URL")
	githubRepository := set.String("github-repo", "", "canonical lower-case owner/repository")
	githubInstallation := set.String("github-installation", "", "GitHub App installation identity")
	base := set.String("base", "", "explicit GitHub base branch")
	contractPath := set.String("contract", "", "local repository contract to validate")
	if err := set.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*connection) == "" {
		return fmt.Errorf("doctor requires --provider")
	}
	profile, err := sandboxProfileByNameOrDefault(ctx, postgres.Store{DB: db}, *profileName)
	if err != nil {
		return err
	}
	checks := doctor.Run(ctx, db, cfg, profile, *connection)
	checks = appendProfileVerificationCheck(checks, profile)
	if *contractPath != "" {
		contents, readErr := os.ReadFile(*contractPath)
		if readErr == nil {
			_, readErr = repository.ParseContract(string(contents))
		}
		detail := "ready"
		status := "ready"
		if readErr != nil {
			status, detail = "failed", readErr.Error()+"; provide commands.prepare and Go-first commands.check/smoke"
		}
		checks = append(checks, doctor.Check{Name: "repository-contract", Status: status, Detail: detail})
	}
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
	provider := set.String("provider", "", "named Provider Connection")
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
		*branch = "dorf/" + spine.JobID(strings.TrimSpace(*key))
	}
	providers := gateway.Gateway{StatePath: cfg.GatewayStatePath}
	profile, err := selectedSandboxProfile(ctx, store, *profileName)
	if err != nil {
		return err
	}
	job, created, err := workflow.Admit(ctx, store, client, providers, workflow.RuntimeProfile{SandboxProfile: profile.Name}, postgres.NewJob{AdmissionKey: *key, Goal: goal, Repository: *repository, Revision: *revision, Branch: *branch, SandboxProfile: profile.Name, ProviderConnection: *provider, Model: *model, ReasoningEffort: *effort, GitHubRepository: *githubRepository, GitHubInstallation: *githubInstallation, BaseBranch: *base})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": job.ID, "workflow": job.Workflow, "workflow_revision": job.WorkflowRevision, "created": created, "task_id": job.CurrentTaskID, "scheduled": true})
}

func workflowCommand(ctx context.Context, store postgres.Store, client *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || args[0] != "run" {
		return fmt.Errorf("workflow requires: run codebase-investigation [options]")
	}
	if args[1] != string(spine.WorkflowCodebaseInvestigation) {
		return fmt.Errorf("unsupported workflow %q", args[1])
	}
	set := flag.NewFlagSet("workflow run codebase-investigation", flag.ContinueOnError)
	set.SetOutput(stderr)
	key := set.String("key", "", "stable caller admission identity")
	briefFile := set.String("brief-file", "", "path containing the complete investigation brief")
	repositoryURL := set.String("repo", "", "clone URL")
	localRepository := set.String("local-repo", "", "local Git repository containing the committed Revision")
	revision := set.String("revision", "", "exact repository Revision (default HEAD with --local-repo)")
	provider := set.String("provider", "", "named Provider Connection")
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
	jobID := spine.JobID(strings.TrimSpace(*key))
	source, workingTreeChangesExcluded, err := prepareInvestigationSource(ctx, blob.Store{Root: cfg.BlobRoot}, *repositoryURL, *localRepository, *revision)
	if err != nil {
		return err
	}
	input := postgres.NewJob{
		AdmissionKey: *key, Goal: brief, Repository: source.Repository, Revision: source.Revision,
		Branch:         "dorf/investigation-" + strings.TrimPrefix(jobID, "job-"),
		SandboxProfile: profile.Name, ProviderConnection: *provider,
		Model: *model, ReasoningEffort: *effort, InvestigationSource: source,
	}
	job, created, err := workflow.AdmitCodebaseInvestigation(ctx, store, client, gateway.Gateway{StatePath: cfg.GatewayStatePath}, workflow.RuntimeProfile{SandboxProfile: profile.Name}, input)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{
		"job_id": job.ID, "workflow": job.Workflow, "workflow_revision": job.WorkflowRevision,
		"required_provider_capabilities": workflow.CodebaseInvestigationDefinition().RequiredProviderCapabilities,
		"created":                        created, "task_id": job.CurrentTaskID, "scheduled": true,
		"source": source, "working_tree_changes_excluded": workingTreeChangesExcluded,
	})
}

func message(ctx context.Context, store postgres.Store, client *absurd.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("message", flag.ContinueOnError)
	set.SetOutput(stderr)
	jobID := set.String("job", "", "existing Job ID")
	requestID := set.String("id", "", "stable human request identity")
	inputFile := set.String("input-file", "", "path containing the complete message input")
	intent := set.String("intent", string(spine.MessageFollow), "harness delivery intent: follow or steer")
	if err := set.Parse(args); err != nil {
		return err
	}
	input, err := readInput(*inputFile, "message", "input")
	if err != nil {
		return err
	}
	accepted, created, err := workflow.AdmitMessage(ctx, store, client, postgres.NewMessage{JobID: *jobID, FromKind: spine.MessageFromHuman, FromID: *requestID, Input: input, Intent: spine.MessageDeliveryIntent(*intent)})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": accepted.JobID, "message_id": accepted.ID, "sequence": accepted.Sequence, "intent": accepted.Intent, "target_turn_id": accepted.TargetTurnID, "created": created, "accepted": true, "delivery": "queued"})
}

func setupRetry(ctx context.Context, store postgres.Store, client *absurd.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("setup-retry", flag.ContinueOnError)
	set.SetOutput(stderr)
	retryID := set.String("id", "", "stable operator identity for this retry generation")
	inputFile := set.String("input-file", "", "file containing the durable repair/wake note")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("setup-retry requires one Job ID")
	}
	input, err := readInput(*inputFile, "setup-retry", "repair note")
	if err != nil {
		return err
	}
	action, message, created, err := workflow.RetrySetup(ctx, store, client, set.Arg(0), *retryID, input)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": action.JobID, "action_id": action.ID, "action_state": action.State, "message_id": message.ID, "retry_created": created, "wake_sequence": message.Sequence})
}

func worker(ctx context.Context, client *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
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
	fmt.Fprintln(stdout, "Dorf durable worker started")
	err := client.RunWorker(workerCtx, absurd.WorkerOptions{WorkerID: workerID(), ClaimTimeout: claimTimeout, BatchSize: *concurrency, Concurrency: *concurrency})
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
	if job.Workflow == spine.WorkflowCodebaseInvestigation {
		return inspectCodebaseInvestigation(ctx, store, client, job, profile, *jsonOutput, stdout)
	}
	if job.Workflow != spine.WorkflowCodingToProposal {
		return fmt.Errorf("inspect does not support workflow %q", job.Workflow)
	}
	snapshot, err := workflow.LoadSnapshot(ctx, store, set.Arg(0))
	if err != nil {
		return err
	}
	job = snapshot.Job
	projection, err := snapshot.Project(evidenceStore)
	if err != nil {
		return err
	}
	currentWork := projection.CurrentWork
	assessment := projection.Readiness
	history := workflowHistory(snapshot)
	definition := workflow.CodingToProposalDefinition()
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
		messages := make([]spine.Message, 0, len(snapshot.Deliveries))
		agentRuns := make([]spine.AgentRun, 0, len(snapshot.Deliveries))
		for _, delivery := range snapshot.Deliveries {
			messages = append(messages, delivery.Message)
			agentRuns = append(agentRuns, delivery.AgentRun)
		}
		view := map[string]any{
			"job":                            job,
			"sandbox_profile":                profileView(profile),
			"current_work":                   currentWork,
			"readiness":                      assessment,
			"required_provider_capabilities": workflow.CodingToProposalDefinition().RequiredProviderCapabilities,
			"proposal":                       snapshot.Proposal,
			"outcome":                        snapshot.Outcome,
			"observed_facts": map[string]any{
				"actions": snapshot.Actions, "agent_runs": agentRuns, "revisions": snapshot.Revisions,
				"checks": snapshot.Checks, "evidence": snapshot.Evidence, "review_plans": snapshot.ReviewPlans,
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
	fmt.Fprintf(stdout, "  goal: %s\n  repository: %s\n  current Revision: %s\n  required provider capabilities: %s\n  Sandbox profile: %s · %s · %s\n  admission: %s\n  cleanup: %s\n  readiness: %s — %s\n", job.Goal, job.Repository, job.Revision, joinProviderCapabilities(definition.RequiredProviderCapabilities), profile.Name, profile.Provider, profile.Harness, openClosed(job.AdmissionOpen), job.CleanupState, readiness, assessment.Reason)
	renderWorkflow(stdout, currentWork)
	if job.WorkflowAttention != "" {
		fmt.Fprintf(stdout, "  attention: %s\n", job.WorkflowAttention)
	}
	if job.CleanupAttention != "" {
		fmt.Fprintf(stdout, "  cleanup attention: %s\n", job.CleanupAttention)
	}
	if snapshot.Proposal == nil {
		fmt.Fprintln(stdout, "  proposal: none")
	} else {
		fmt.Fprintf(stdout, "  proposal: #%d %s Revision=%s", snapshot.Proposal.Number, snapshot.Proposal.URL, snapshot.Proposal.ProposedRevision)
		if snapshot.Proposal.ProposedRevision != job.Revision {
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

func inspectCodebaseInvestigation(ctx context.Context, store postgres.Store, client *absurd.Client, job spine.Job, profile spine.SandboxProfile, jsonOutput bool, stdout io.Writer) error {
	snapshot, err := workflow.LoadCodebaseInvestigation(ctx, store, job.ID)
	if err != nil {
		return err
	}
	work := snapshot.Project()
	artifacts, err := store.Artifacts(ctx, job.ID)
	if err != nil {
		return err
	}
	if artifacts == nil {
		artifacts = []spine.Artifact{}
	}
	executions, err := fetchJobTaskExecutions(ctx, store, client, job)
	if err != nil {
		return err
	}
	currentExecution := currentTaskExecution(executions)
	definition := workflow.CodebaseInvestigationDefinition()
	executionOperation := work.Description()
	if operation, ok := cleanupOperation(definition, job, []spine.Sandbox{snapshot.MainSandbox}, snapshot.Actions); ok {
		executionOperation = operation
	}
	if jsonOutput {
		return writeJSON(stdout, map[string]any{
			"job": job, "source": snapshot.Source, "sandbox_profile": profileView(profile), "current_work": work, "report": snapshot.Report, "artifacts": artifacts,
			"required_provider_capabilities": definition.RequiredProviderCapabilities,
			"observed_facts":                 map[string]any{"actions": snapshot.Actions, "agent_run": snapshot.Delivery.AgentRun, "sandbox": snapshot.MainSandbox},
			"execution":                      executions,
		})
	}
	fmt.Fprintf(stdout, "Job %s\n  workflow: %s revision %s\n", job.ID, job.Workflow, job.WorkflowRevision)
	renderWorkflowExecutionAttention(stdout, job, currentExecution, executionOperation)
	fmt.Fprintf(stdout, "  brief: %s\n  source: %s\n  exact Revision: %s\n  required provider capabilities: %s\n  Sandbox profile: %s · %s · %s\n  admission: %s\n  cleanup: %s\n",
		job.Goal, investigationSourceSummary(snapshot.Source), job.Revision, joinProviderCapabilities(definition.RequiredProviderCapabilities), profile.Name, profile.Provider, profile.Harness, openClosed(job.AdmissionOpen), job.CleanupState)
	fmt.Fprintf(stdout, "  current work: %s", work.Kind)
	if work.Detail != "" {
		fmt.Fprintf(stdout, " — %s", work.Detail)
	}
	fmt.Fprintln(stdout)
	if job.WorkflowAttention != "" {
		fmt.Fprintf(stdout, "  attention: %s\n", job.WorkflowAttention)
	}
	if job.CleanupAttention != "" {
		fmt.Fprintf(stdout, "  cleanup attention: %s\n", job.CleanupAttention)
	}
	renderHistory(stdout, investigationHistory(snapshot))
	if snapshot.Report == nil {
		fmt.Fprintln(stdout, "  report: none")
		return nil
	}
	fmt.Fprintf(stdout, "  report: observed-at=%s Artifact=%s\n", snapshot.Report.ObservedAt.Format(time.RFC3339Nano), snapshot.Report.ReportArtifactID)
	fmt.Fprintf(stdout, "  retrieve: dorf artifact get %s\n", snapshot.Report.ReportArtifactID)
	return nil
}

func investigationSourceSummary(source spine.CodebaseInvestigationSource) string {
	if source.Kind == spine.InvestigationSourceGitBundle {
		return fmt.Sprintf("retained Git bundle sha256:%s (%d bytes)", source.BundleDigest, source.BundleByteSize)
	}
	return "remote Git " + source.Repository
}

func joinProviderCapabilities(capabilities []workflow.ProviderCapability) string {
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
	receipt, err := workflow.RetryFailedJob(ctx, store, client, set.Arg(0))
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
	job, err := store.Job(ctx, set.Arg(1))
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
	declared, err := store.DeclaredChecks(ctx, job.ID)
	if err != nil {
		return err
	}
	checks, err := store.Checks(ctx, job.ID)
	if err != nil {
		return err
	}
	if err := spine.VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, evidenceStore); err != nil {
		return fmt.Errorf("current Revision %s readiness Evidence: %w", job.Revision, err)
	}
	fmt.Fprintf(stdout, "Revision %s: proving Evidence artifacts independently verified against Check rows\n", job.Revision)
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
	job, err := workflow.ScheduleCleanup(ctx, store, client, set.Arg(0))
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": job.ID, "cleanup": job.CleanupState, "task_id": job.CurrentTaskID, "scheduled": job.CleanupState == spine.CleanupScheduled})
}

func abandon(ctx context.Context, store postgres.Store, client *absurd.Client, githubClient githubapi.Client, args []string, stdout io.Writer) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("abandon requires one Job ID")
	}
	// This interactive command is the outcome authority; unlike a workflow
	// executor, it has no Absurd claim to revalidate before the write.
	direct := (outcomeapp.Service{Store: store, GitHub: githubClient}).WithClaimCheck(func(context.Context) error { return nil })
	receipt, created, err := direct.Record(ctx, strings.TrimSpace(args[0]), spine.OutcomeAbandoned)
	if err != nil {
		return err
	}
	job, err := workflow.ScheduleCleanup(ctx, store, client, receipt.JobID)
	if err != nil {
		return fmt.Errorf("%s outcome receipt was retained, but durable cleanup scheduling failed: %w", receipt.Kind, err)
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

func renderWorkflow(output io.Writer, work workflow.Work) {
	fmt.Fprintln(output, "\nWorkflow")
	fmt.Fprintln(output, "  Sandbox → repository clone → Setup → provider Route → Message → AgentRun → Revision → Checks → ReviewPolicy")
	fmt.Fprintln(output, "                                                        ↑                                        │")
	fmt.Fprintln(output, "                                                        └──── feedback ← review AgentRun ←────────┤ review")
	fmt.Fprintln(output, "                                                                                                 └ no review")
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
	Attachment spine.JobTask  `json:"attachment"`
	Current    bool           `json:"current"`
	Execution  taskResultView `json:"execution"`
}

func fetchJobTaskExecutions(ctx context.Context, store postgres.Store, client *absurd.Client, job spine.Job) ([]jobTaskExecutionView, error) {
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

func renderWorkflowExecutionAttention(output io.Writer, job spine.Job, execution taskResultView, operation string) {
	if execution.State != absurd.TaskFailed || job.CleanupState == spine.CleanupComplete {
		return
	}
	label := "workflow stopped"
	if job.CleanupState == spine.CleanupScheduled {
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

func usage(output io.Writer) error {
	fmt.Fprintln(output, "usage: dorf <version|setup|migrate|doctor|provider|profile|workflow|artifact|admit|message|setup-retry|worker|inspect|retry|evidence|abandon|cleanup> [options]")
	return fmt.Errorf("unknown or missing command")
}
