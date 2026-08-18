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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/doctor"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/evidence"
	"github.com/aphronio/dorf/internal/gateway"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/hostsetup"
	"github.com/aphronio/dorf/internal/incus"
	outcomeapp "github.com/aphronio/dorf/internal/outcome"
	piagent "github.com/aphronio/dorf/internal/pi"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/proofbarrier"
	"github.com/aphronio/dorf/internal/publication"
	releaseapp "github.com/aphronio/dorf/internal/release"
	"github.com/aphronio/dorf/internal/repository"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/terminal"
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
	if args[0] == "host" {
		return hostCommand(ctx, args[1:], stdout, stderr)
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
	case "setup":
		return setup(ctx, store, db, cfg, args[1:], stdout, stderr)
	case "provider":
		return providerCommand(ctx, cfg, args[1:], stdout, stderr)
	case "image":
		return imageCommand(ctx, cfg, args[1:], stdout, stderr)
	case "release-manifest":
		return releaseManifest(args[1:], stdout, stderr)
	case "retry":
		client, err := absurdClient(db)
		if err != nil {
			return err
		}
		defer client.Close()
		return retry(ctx, store, client, args[1:], stdout, stderr)
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
	case "inspect":
		evidenceStore := evidence.Store{Root: cfg.EvidenceRoot}
		return inspect(ctx, store, client, evidenceStore, args[1:], stdout, stderr)
	case "evidence":
		return evidenceCommand(ctx, store, evidence.Store{Root: cfg.EvidenceRoot}, args[1:], stdout, stderr)
	case "cleanup":
		return cleanup(ctx, store, client, args[1:], stdout, stderr)
	case "abandon":
		githubClient := githubapi.Client{APIURL: cfg.GitHubAPIURL, Metadata: cfg.GitHubMetadata, PrivateKey: cfg.GitHubPrivateKey}
		return abandon(ctx, store, client, githubClient, args[1:], stdout)
	default:
		return usage(stderr)
	}
}

func hostCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "install" {
		return fmt.Errorf("host requires: install [--yes]")
	}
	set := flag.NewFlagSet("host install", flag.ContinueOnError)
	set.SetOutput(stderr)
	yes := set.Bool("yes", false, "approve the displayed Ubuntu 24.04 package, service, and group changes")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	return hostsetup.Ubuntu(ctx, *yes, stdout, stderr)
}

func application(db *sql.DB, cfg config.Config) (*absurd.Client, error) {
	client, err := absurdClient(db)
	if err != nil {
		return nil, err
	}
	store := postgres.Store{DB: db}
	ownership := func(ctx context.Context, sandboxID string) (provider.Ownership, error) {
		owned, err := store.Sandbox(ctx, sandboxID)
		if err != nil {
			return provider.Ownership{}, err
		}
		return provider.Ownership{JobID: owned.JobID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce}, nil
	}
	sandbox, err := sandboxForConfig(cfg)
	if err != nil {
		client.Close()
		return nil, err
	}
	var agent terminal.Harness
	switch cfg.Harness {
	case codex.Harness:
		agent = codex.Agent{Sandbox: sandbox, Port: cfg.AppServerPort, Timeout: cfg.TurnTimeout}
	case piagent.Harness:
		agent = piagent.Agent{Sandbox: sandbox, Timeout: cfg.TurnTimeout}
	default:
		client.Close()
		return nil, fmt.Errorf("unsupported harness %q", cfg.Harness)
	}
	barrier, err := proofbarrier.FromEnv()
	if err != nil {
		client.Close()
		return nil, err
	}
	externals := terminal.Externals{Sandbox: sandbox, Gateway: gateway.Gateway{StatePath: cfg.GatewayStatePath}, Agent: agent, Ownership: ownership}
	service := spine.NewService(store, externals, evidence.Store{Root: cfg.EvidenceRoot}, barrier, absurdruntime.RequireClaim)
	githubClient := githubapi.Client{APIURL: cfg.GitHubAPIURL, Metadata: cfg.GitHubMetadata, PrivateKey: cfg.GitHubPrivateKey}
	publicationService := publication.Service{Store: store, GitHub: githubClient, Repository: publication.GitRepository{Sandbox: sandbox, Workspace: cfg.Workspace, Ownership: ownership}, Evidence: evidence.Store{Root: cfg.EvidenceRoot}, Barrier: barrier}
	workflow.Register(client, service, store, workflow.ProposalRuntime{Publication: publicationService, GitHub: githubClient, Outcome: outcomeapp.Service{Store: store, GitHub: githubClient}, Store: store, Client: client}, workflow.ConfiguredRuntimeProfile(cfg.SandboxProfile))
	return client, nil
}

func absurdClient(db *sql.DB) (*absurd.Client, error) {
	return absurd.New(absurd.Options{DB: db, QueueName: config.QueueName, DefaultMaxAttempts: 5})
}

func sandboxForConfig(cfg config.Config) (provider.Sandbox, error) {
	switch cfg.SandboxProfile {
	case config.SandboxProfileIncus:
		return incus.Adapter{Sandbox: incus.Sandbox{Config: incus.Config{Image: cfg.IncusImage, Network: cfg.IncusNetwork, DiskSize: cfg.IncusDiskSize, Workspace: cfg.Workspace}}}, nil
	case config.SandboxProfileE2B:
		if strings.TrimSpace(cfg.E2BAPIKey) == "" {
			return nil, fmt.Errorf("invalid e2b Sandbox profile: E2B_API_KEY is empty")
		}
		adapter := e2b.Adapter{
			Client: e2b.Client{APIKey: cfg.E2BAPIKey},
			Config: e2b.AdapterConfig{
				Template: cfg.E2BTemplate, Workspace: cfg.Workspace,
				SandboxTimeout: cfg.E2BSandboxTimeout, ProcessTimeout: cfg.TurnTimeout,
				ProviderGatewayURL: cfg.E2BGatewayURL, AllowInternet: cfg.E2BAllowInternet,
			},
		}
		if err := adapter.Validate(); err != nil {
			return nil, fmt.Errorf("invalid e2b Sandbox profile: %w", err)
		}
		return adapter, nil
	default:
		return nil, fmt.Errorf("unsupported Sandbox profile %q", cfg.SandboxProfile)
	}
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

func providerCommand(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("provider requires: connect chatgpt --name NAME [--bind INCUS_BRIDGE_IP]")
	}
	if args[0] != "connect" || len(args) < 2 || args[1] != "chatgpt" {
		return fmt.Errorf("the supported provider command is: provider connect chatgpt --name NAME [--bind INCUS_BRIDGE_IP]")
	}
	set := flag.NewFlagSet("provider connect chatgpt", flag.ContinueOnError)
	set.SetOutput(stderr)
	name := set.String("name", "personal-chatgpt", "stable Provider Connection name")
	bind := set.String("bind", "", "exact private Incus bridge IPv4")
	if err := set.Parse(args[2:]); err != nil {
		return err
	}
	if strings.TrimSpace(*bind) == "" {
		result, err := (incus.CommandRunner{}).Run(ctx, "incus", nil, "network", "get", cfg.IncusNetwork, "ipv4.address")
		if err != nil || result.ExitCode != 0 {
			return fmt.Errorf("resolve private Incus bridge address; initialize %s or pass --bind", cfg.IncusNetwork)
		}
		*bind = strings.Split(strings.TrimSpace(result.Stdout), "/")[0]
	}
	g := gateway.Gateway{StatePath: cfg.GatewayStatePath, PrivateBridge: cfg.IncusNetwork}
	if err := g.ConnectChatGPT(ctx, *name, *bind, func(url, code string) {
		fmt.Fprintf(stdout, "Open %s and enter %s\n", url, code)
	}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Provider Connection ready: %s (ChatGPT subscription; broker %s on %s)\n", *name, gateway.BackendVersion, *bind)
	return nil
}

func imageCommand(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "install" {
		return fmt.Errorf("image requires: install --release vX.Y.Z (or --manifest FILE --archive FILE)")
	}
	set := flag.NewFlagSet("image install", flag.ContinueOnError)
	set.SetOutput(stderr)
	manifest := set.String("manifest", "", "verified release image manifest")
	archive := set.String("archive", "", "matching Incus VM archive")
	releaseTag := set.String("release", "", "immutable Dorf GitHub release tag")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	local := *manifest != "" || *archive != ""
	if (*releaseTag == "" && !local) || (*releaseTag != "" && local) || (local && (*manifest == "" || *archive == "")) {
		return fmt.Errorf("image install requires exactly --release or both --manifest and --archive")
	}
	var installed releaseapp.Manifest
	var err error
	if *releaseTag != "" {
		installed, err = releaseapp.InstallPublishedImage(ctx, *releaseTag, cfg.IncusImage)
	} else {
		installed, err = releaseapp.InstallImage(ctx, *manifest, *archive, cfg.IncusImage)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Official credential-free image ready: %s fingerprint=%s Codex=%s Pi=%s\n", cfg.IncusImage, installed.ImageFingerprint, installed.Harnesses["codex"].Version, installed.Harnesses["pi"].Version)
	return nil
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

func setup(ctx context.Context, store postgres.Store, db *sql.DB, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("setup", flag.ContinueOnError)
	set.SetOutput(stderr)
	connection := set.String("provider", "", "named Provider Connection")
	absurdSchema := set.String("absurd-schema", "", "optional local copy of the pinned Absurd schema")
	if err := set.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*connection) == "" {
		return fmt.Errorf("setup requires --provider; create one with dorf provider connect")
	}
	migrateArgs := []string{}
	if *absurdSchema != "" {
		migrateArgs = append(migrateArgs, "--absurd-schema", *absurdSchema)
	}
	if err := migrate(ctx, store, migrateArgs, stdout, stderr); err != nil {
		return err
	}
	checks := doctor.Run(ctx, db, cfg, *connection)
	if err := json.NewEncoder(stdout).Encode(checks); err != nil {
		return err
	}
	if !doctor.Ready(checks) {
		return fmt.Errorf("setup is not converged; apply the failed check remediations and rerun this command")
	}
	fmt.Fprintf(stdout, "Dorf is ready: PostgreSQL, Absurd, Sandbox profile %s, Harness %s, and Provider Gateway checks passed\n", cfg.SandboxProfile, cfg.Harness)
	return nil
}

func runDoctor(ctx context.Context, db *sql.DB, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("doctor", flag.ContinueOnError)
	set.SetOutput(stderr)
	connection := set.String("provider", "", "named Provider Connection")
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
	checks := doctor.Run(ctx, db, cfg, *connection)
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
	job, created, err := workflow.Admit(ctx, store, client, providers, postgres.NewJob{AdmissionKey: *key, Goal: goal, Repository: *repository, Revision: *revision, Branch: *branch, SandboxProfile: cfg.SandboxProfile, ProviderConnection: *provider, Model: *model, ReasoningEffort: *effort, GitHubRepository: *githubRepository, GitHubInstallation: *githubInstallation, BaseBranch: *base})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": job.ID, "workflow": job.Workflow, "workflow_revision": job.WorkflowRevision, "created": created, "task_id": job.TaskID, "scheduled": true})
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
	repository := set.String("repo", "", "clone URL")
	revision := set.String("revision", "", "exact repository Revision")
	provider := set.String("provider", "", "named Provider Connection")
	model := set.String("model", "", "Harness model")
	effort := set.String("reasoning", "high", "Harness reasoning effort")
	if err := set.Parse(args[2:]); err != nil {
		return err
	}
	brief, err := readInput(*briefFile, "workflow run codebase-investigation", "brief")
	if err != nil {
		return err
	}
	jobID := spine.JobID(strings.TrimSpace(*key))
	input := postgres.NewJob{
		AdmissionKey: *key, Goal: brief, Repository: *repository, Revision: *revision,
		Branch:         "dorf/investigation-" + strings.TrimPrefix(jobID, "job-"),
		SandboxProfile: cfg.SandboxProfile, ProviderConnection: *provider,
		Model: *model, ReasoningEffort: *effort,
	}
	job, created, err := workflow.AdmitCodebaseInvestigation(ctx, store, client, gateway.Gateway{StatePath: cfg.GatewayStatePath}, workflow.ConfiguredRuntimeProfile(cfg.SandboxProfile), input)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{
		"job_id": job.ID, "workflow": job.Workflow, "workflow_revision": job.WorkflowRevision,
		"required_provider_capabilities": workflow.CodebaseInvestigationDefinition().RequiredProviderCapabilities,
		"created":                        created, "task_id": job.TaskID, "scheduled": true,
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

func inspect(ctx context.Context, store postgres.Store, client *absurd.Client, evidenceStore evidence.Store, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("inspect", flag.ContinueOnError)
	set.SetOutput(stderr)
	jsonOutput := set.Bool("json", false, "render JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("inspect requires one Job ID")
	}
	job, err := store.Job(ctx, set.Arg(0))
	if err != nil {
		return err
	}
	if job.Workflow == spine.WorkflowCodebaseInvestigation {
		return inspectCodebaseInvestigation(ctx, store, client, evidenceStore, job, *jsonOutput, stdout)
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
	if *jsonOutput {
		messages := make([]spine.Message, 0, len(snapshot.Deliveries))
		agentRuns := make([]spine.AgentRun, 0, len(snapshot.Deliveries))
		for _, delivery := range snapshot.Deliveries {
			messages = append(messages, delivery.Message)
			agentRuns = append(agentRuns, delivery.AgentRun)
		}
		runEvidence, err := fetchTaskResult(ctx, client, job.TaskID)
		if err != nil {
			return err
		}
		cleanupEvidence, err := fetchTaskResult(ctx, client, job.CleanupTaskID)
		if err != nil {
			return err
		}
		view := map[string]any{
			"job":                            job,
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
			"absurd_run":     runEvidence,
			"absurd_cleanup": cleanupEvidence,
		}
		return writeJSON(stdout, view)
	}
	readiness := "not ready"
	if assessment.Ready {
		readiness = "ready"
	}
	fmt.Fprintf(stdout, "Job %s\n  workflow: %s revision %s\n  goal: %s\n  repository: %s\n  current Revision: %s\n  required provider capabilities: %s\n  Sandbox profile: %s\n  admission: %s\n  cleanup: %s\n  readiness: %s — %s\n", job.ID, job.Workflow, job.WorkflowRevision, job.Goal, job.Repository, job.Revision, joinProviderCapabilities(workflow.CodingToProposalDefinition().RequiredProviderCapabilities), job.SandboxProfile, openClosed(job.AdmissionOpen), job.CleanupState, readiness, assessment.Reason)
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

func inspectCodebaseInvestigation(ctx context.Context, store postgres.Store, client *absurd.Client, records evidence.Store, job spine.Job, jsonOutput bool, stdout io.Writer) error {
	snapshot, err := workflow.LoadCodebaseInvestigation(ctx, store, job.ID)
	if err != nil {
		return err
	}
	work := snapshot.Project()
	var report string
	if snapshot.Report != nil {
		var record *spine.Evidence
		evidenceRecords, err := store.Evidence(ctx, job.ID)
		if err != nil {
			return err
		}
		for i := range evidenceRecords {
			if evidenceRecords[i].ID == snapshot.Report.ReportEvidenceID {
				record = &evidenceRecords[i]
				break
			}
		}
		if record == nil {
			return fmt.Errorf("investigation Report Evidence %s is missing", snapshot.Report.ReportEvidenceID)
		}
		contents, err := records.ReadVerified(record.Digest, record.ByteSize)
		if err != nil {
			return err
		}
		report = string(contents)
	}
	runEvidence, err := fetchTaskResult(ctx, client, job.TaskID)
	if err != nil {
		return err
	}
	cleanupEvidence, err := fetchTaskResult(ctx, client, job.CleanupTaskID)
	if err != nil {
		return err
	}
	definition := workflow.CodebaseInvestigationDefinition()
	if jsonOutput {
		return writeJSON(stdout, map[string]any{
			"job": job, "current_work": work, "report": snapshot.Report, "report_markdown": report,
			"required_provider_capabilities": definition.RequiredProviderCapabilities,
			"observed_facts":                 map[string]any{"actions": snapshot.Actions, "agent_run": snapshot.Delivery.AgentRun, "sandbox": snapshot.MainSandbox},
			"absurd_run":                     runEvidence, "absurd_cleanup": cleanupEvidence,
		})
	}
	fmt.Fprintf(stdout, "Job %s\n  workflow: %s revision %s\n  brief: %s\n  repository: %s\n  exact Revision: %s\n  required provider capabilities: %s\n  Sandbox profile: %s\n  admission: %s\n  cleanup: %s\n  current work: %s",
		job.ID, job.Workflow, job.WorkflowRevision, job.Goal, job.Repository, job.Revision, joinProviderCapabilities(definition.RequiredProviderCapabilities), job.SandboxProfile, openClosed(job.AdmissionOpen), job.CleanupState, work.Kind)
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
	if snapshot.Report == nil {
		fmt.Fprintln(stdout, "  report: none")
		return nil
	}
	fmt.Fprintf(stdout, "  report: observed-at=%s Evidence=%s\n", snapshot.Report.ObservedAt.Format(time.RFC3339Nano), snapshot.Report.ReportEvidenceID)
	fmt.Fprintln(stdout, "\nReport\n------")
	fmt.Fprint(stdout, report)
	return nil
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

func evidenceCommand(ctx context.Context, store postgres.Store, evidenceStore evidence.Store, args []string, stdout, stderr io.Writer) error {
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
	return writeJSON(stdout, map[string]any{"job_id": job.ID, "cleanup": job.CleanupState, "task_id": job.CleanupTaskID, "scheduled": job.CleanupState == spine.CleanupScheduled})
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
	return writeJSON(stdout, map[string]any{"outcome": receipt, "created": created, "cleanup": job.CleanupState, "cleanup_task_id": job.CleanupTaskID})
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

type historyEntry struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
}

// workflowHistory is a disposable human projection over product facts. It
// never decides eligibility and is never persisted; CurrentWork remains the
// execution and inspection authority for what happens next.
func workflowHistory(snapshot workflow.Snapshot) []historyEntry {
	job := snapshot.Job
	deliveries := snapshot.Deliveries
	actions := snapshot.Actions
	revisions := snapshot.Revisions
	checks := snapshot.Checks
	plans := snapshot.ReviewPlans
	records := snapshot.Evidence
	proposal := snapshot.Proposal
	outcome := snapshot.Outcome
	entries := make([]historyEntry, 0, 1+3*len(deliveries)+2*len(actions)+len(revisions)+2*len(checks)+len(plans)+len(records)+2)
	add := func(at time.Time, kind, detail string) {
		if !at.IsZero() {
			entries = append(entries, historyEntry{At: at, Kind: kind, Detail: detail})
		}
	}
	add(job.AdmittedAt, "Job", "admitted")
	add(job.WorkflowAttentionAt, "Attention", job.WorkflowAttention)
	for _, delivery := range deliveries {
		message := delivery.Message
		add(message.AdmittedAt, "Message", fmt.Sprintf("%d admitted from %s", message.Sequence, message.FromKind))
	}
	for _, action := range actions {
		add(action.CreatedAt, "Action", fmt.Sprintf("%s created", action.Kind))
		add(action.SettledAt, "Action", fmt.Sprintf("%s %s", action.Kind, action.State))
		if action.Kind == spine.ActionGitHubPullRequest && action.State == spine.ActionSucceeded && proposal != nil && proposal.ProposedRevision == action.Scope {
			add(action.SettledAt, "Proposal", fmt.Sprintf("#%d recorded for Revision %s", proposal.Number, proposal.ProposedRevision))
		}
	}
	for _, delivery := range deliveries {
		run := delivery.AgentRun
		role := run.Role
		if role == "implement" {
			role = "implementation"
		} else {
			role += " review"
		}
		input := ""
		if delivery.Message.Sequence > 0 {
			input = fmt.Sprintf(" for Message %d", delivery.Message.Sequence)
		}
		if run.InputRevision != "" {
			input += " at Revision " + run.InputRevision
		}
		add(run.StartedAt, "AgentRun", fmt.Sprintf("%s started%s", role, input))
		add(run.FinishedAt, "AgentRun", fmt.Sprintf("%s %s%s", role, run.State, input))
	}
	for _, revision := range revisions {
		detail := fmt.Sprintf("generation %d observed Revision %s", revision.Generation, revision.OID)
		if revision.Generation == 0 {
			detail = "starting Revision " + revision.OID + " accepted"
		} else if revision.ComparisonBase != "" {
			detail += " from " + revision.ComparisonBase
		}
		add(revision.ObservedAt, "Revision", detail)
	}
	for _, check := range checks {
		add(check.StartedAt, "Check", fmt.Sprintf("%s started for Revision %s", check.Name, check.Revision))
		add(check.FinishedAt, "Check", fmt.Sprintf("%s %s for Revision %s (exit %d)", check.Name, check.State, check.Revision, check.ExitCode))
	}
	for _, plan := range plans {
		add(plan.RecordedAt, "ReviewPolicy", fmt.Sprintf("%s for Revision %s Roles=%v", plan.Plan.Decision, plan.Revision, plan.Plan.Roles))
	}
	for _, record := range records {
		detail := record.Kind + " recorded"
		if record.Revision != "" {
			detail += " for Revision " + record.Revision
		}
		add(record.FinishedAt, "Evidence", detail)
	}
	if outcome != nil {
		detail := string(outcome.Kind)
		if outcome.ObservedState != "" {
			detail += fmt.Sprintf(" (GitHub state=%s)", outcome.ObservedState)
		}
		add(outcome.ObservedAt, "Outcome", detail)
	}
	add(job.CleanedAt, "Cleanup", "complete")
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].At.Before(entries[j].At) })
	return entries
}

func renderHistory(output io.Writer, entries []historyEntry) {
	fmt.Fprintln(output, "\nHistory")
	for _, entry := range entries {
		fmt.Fprintf(output, "  %s  %-12s %s\n", entry.At.Format(time.RFC3339Nano), entry.Kind, entry.Detail)
	}
}

type taskResultView struct {
	TaskID  string                 `json:"task_id,omitempty"`
	State   absurd.TaskResultState `json:"state,omitempty"`
	Result  json.RawMessage        `json:"result,omitempty"`
	Failure json.RawMessage        `json:"failure,omitempty"`
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
	return taskResultView{TaskID: taskID, State: snapshot.State, Result: snapshot.Result, Failure: snapshot.Failure}, nil
}

func usage(output io.Writer) error {
	fmt.Fprintln(output, "usage: dorf <version|host|setup|migrate|doctor|provider|image|workflow|admit|message|setup-retry|worker|inspect|retry|evidence|abandon|cleanup> [options]")
	return fmt.Errorf("unknown or missing command")
}
