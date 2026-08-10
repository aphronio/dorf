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

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/doctor"
	"github.com/aphronio/dorf/internal/evidence"
	"github.com/aphronio/dorf/internal/gateway"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/hostsetup"
	"github.com/aphronio/dorf/internal/incus"
	outcomeapp "github.com/aphronio/dorf/internal/outcome"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/proofbarrier"
	"github.com/aphronio/dorf/internal/publication"
	releaseapp "github.com/aphronio/dorf/internal/release"
	"github.com/aphronio/dorf/internal/repository"
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
	}
	client, service, err := application(db, cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	switch args[0] {
	case "admit":
		return admit(ctx, store, client, cfg, args[1:], stdout, stderr)
	case "message":
		return message(ctx, store, client, args[1:], stdout, stderr)
	case "setup-retry":
		return setupRetry(ctx, store, client, args[1:], stdout, stderr)
	case "worker":
		return worker(ctx, client, cfg, args[1:], stdout, stderr)
	case "inspect":
		return inspect(ctx, store, client, evidence.Store{Root: cfg.EvidenceRoot}, args[1:], stdout, stderr)
	case "evidence":
		return evidenceCommand(ctx, store, evidence.Store{Root: cfg.EvidenceRoot}, args[1:], stdout, stderr)
	case "cleanup":
		return cleanup(ctx, store, client, service, args[1:], stdout, stderr)
	case "outcome":
		githubClient := githubapi.Client{APIURL: cfg.GitHubAPIURL, Metadata: cfg.GitHubMetadata, PrivateKey: cfg.GitHubPrivateKey}
		return outcomeCommand(ctx, store, client, githubClient, args[1:], stdout)
	default:
		_ = service
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

func application(db *sql.DB, cfg config.Config) (*absurd.Client, spine.Service, error) {
	client, err := absurd.New(absurd.Options{DB: db, QueueName: config.QueueName, DefaultMaxAttempts: 5})
	if err != nil {
		return nil, spine.Service{}, err
	}
	sandbox := incus.Sandbox{Config: incus.Config{Image: cfg.IncusImage, Network: cfg.IncusNetwork, DiskSize: cfg.IncusDiskSize, Workspace: cfg.Workspace}}
	agent := codex.Agent{Sandbox: sandbox, Port: cfg.AppServerPort, Timeout: cfg.TurnTimeout}
	barrier, err := proofbarrier.FromEnv()
	if err != nil {
		client.Close()
		return nil, spine.Service{}, err
	}
	externals := terminal.Externals{Sandbox: sandbox, Gateway: gateway.Gateway{StatePath: cfg.GatewayStatePath}, Agent: agent}
	store := postgres.Store{DB: db}
	service := spine.Service{Store: store, Externals: externals, Repository: externals, Evidence: evidence.Store{Root: cfg.EvidenceRoot}, Barrier: barrier}
	githubClient := githubapi.Client{APIURL: cfg.GitHubAPIURL, Metadata: cfg.GitHubMetadata, PrivateKey: cfg.GitHubPrivateKey}
	publicationService := publication.Service{Store: store, GitHub: githubClient, Repository: publication.GitRepository{Sandbox: sandbox, Workspace: cfg.Workspace}, Evidence: evidence.Store{Root: cfg.EvidenceRoot}, Barrier: barrier}
	workflow.Register(client, service, store, workflow.ProposalRuntime{Publication: publicationService, GitHub: githubClient, Outcome: outcomeapp.Service{Store: store, GitHub: githubClient}, Store: store, Client: client})
	return client, service, nil
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
	fmt.Fprintf(stdout, "Official credential-free image ready: %s fingerprint=%s Codex=%s\n", cfg.IncusImage, installed.ImageFingerprint, installed.Codex.Version)
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
	fmt.Fprintln(stdout, "Dorf is ready: Go, PostgreSQL, Absurd, Incus, the credential-free Codex image, and Provider Gateway checks passed")
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
	job, created, err := workflow.Admit(ctx, store, client, providers, postgres.NewJob{AdmissionKey: *key, Goal: goal, Repository: *repository, Revision: *revision, Branch: *branch, ProviderConnection: *provider, Model: *model, ReasoningEffort: *effort, GitHubRepository: *githubRepository, GitHubInstallation: *githubInstallation, BaseBranch: *base})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": job.ID, "created": created, "task_id": job.TaskID, "scheduled": true})
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
	delivery := "queued"
	var blockingSequence int64
	var blockingReason string
	views, err := store.Messages(ctx, accepted.JobID)
	if err != nil {
		return err
	}
	for _, view := range views {
		if view.ID == accepted.ID && view.BlockingSeq > 0 {
			delivery, blockingSequence, blockingReason = "blocked", view.BlockingSeq, view.BlockingReason
			break
		}
	}
	return writeJSON(stdout, map[string]any{"job_id": accepted.JobID, "message_id": accepted.ID, "sequence": accepted.Sequence, "intent": accepted.Intent, "target_turn_id": accepted.TargetTurnID, "created": created, "accepted": true, "delivery": delivery, "blocking_sequence": blockingSequence, "blocking_reason": blockingReason})
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
	actions, err := store.Actions(ctx, job.ID)
	if err != nil {
		return err
	}
	messages, err := store.Messages(ctx, job.ID)
	if err != nil {
		return err
	}
	checks, err := store.Checks(ctx, job.ID)
	if err != nil {
		return err
	}
	evidenceRecords, err := store.Evidence(ctx, job.ID)
	if err != nil {
		return err
	}
	declared, declaredErr := store.DeclaredChecks(ctx, job.ID)
	if declaredErr != nil {
		declared = nil
	}
	plans, err := store.ReviewPlans(ctx, job.ID)
	if err != nil {
		return err
	}
	reviewRuns, err := store.AllReviewRuns(ctx, job.ID)
	if err != nil {
		return err
	}
	var currentPlan *spine.ReviewPlanRecord
	for i := range plans {
		if plans[i].Revision == job.Revision {
			currentPlan = &plans[i]
		}
	}
	assessment := spine.AssessReviewReadiness(job, declared, checks, evidenceRecords, evidenceStore, currentPlan, reviewRuns)
	proposal, err := store.Proposal(ctx, job.ID)
	if err != nil {
		return err
	}
	outcome, err := store.Outcome(ctx, job.ID)
	if err != nil {
		return err
	}
	runEvidence, err := fetchTaskResult(ctx, client, job.TaskID)
	if err != nil {
		return err
	}
	cleanupEvidence, err := fetchTaskResult(ctx, client, job.CleanupTaskID)
	if err != nil {
		return err
	}
	continuation := continuationFor(job, outcome, runEvidence, cleanupEvidence)
	view := map[string]any{"job": job, "continuation": continuation, "readiness": assessment, "proposal": proposal, "outcome": outcome, "review_plans": plans, "review_agent_runs": reviewRuns, "claims": map[string]any{"messages": messages, "review_agent_runs": reviewRuns, "authority": "Agent text is a claim carried by Message; it does not satisfy Checks"}, "observed_facts": map[string]any{"actions": actions, "checks": checks, "evidence": evidenceRecords, "current_revision_evidence_verification": assessment.Evidence}, "absurd_run": runEvidence, "absurd_cleanup": cleanupEvidence, "absurd_inspection": "Use absurdctl dump-task --task-id=<task-id> for runs, attempts, checkpoints, leases, waits, and history", "transcript_authority": "Harness threads (not copied into Dorf)"}
	if *jsonOutput {
		return writeJSON(stdout, view)
	}
	fmt.Fprintf(stdout, "Job %s\n  workflow: %s\n  continuation: %s — %s\n  readiness: %s — %s\n  admission: %s\n  cleanup: %s\n  goal: %s\n  repository: %s\n  starting Revision: %s\n  current Revision: %s\n  sandbox: %s state=%s\n  route: %s state=%s\n", job.ID, job.WorkflowPhase, continuation.Mode, continuation.Detail, assessment.Status, assessment.Reason, openClosed(job.AdmissionOpen), job.CleanupState, job.Goal, job.Repository, job.StartingRevision, job.Revision, empty(job.SandboxID), empty(job.SandboxState), empty(job.RouteID), empty(job.RouteState))
	if job.WorkflowAttention != "" {
		fmt.Fprintf(stdout, "  attention: %s\n", job.WorkflowAttention)
	}
	if job.CleanupAttention != "" {
		fmt.Fprintf(stdout, "  cleanup attention: %s\n", job.CleanupAttention)
	}
	if proposal == nil {
		fmt.Fprintln(stdout, "  proposal: none")
	} else {
		fmt.Fprintf(stdout, "  proposal: #%d %s Revision=%s remote=%s body=%s stale=%t\n", proposal.Number, proposal.URL, proposal.ProposedRevision, proposal.ObservedRemoteHead, proposal.BodyDigest, proposal.Stale)
	}
	if outcome == nil {
		fmt.Fprintln(stdout, "  outcome: none")
	} else {
		fmt.Fprintf(stdout, "  outcome: %s PR=#%d state=%s merged=%t proposed=%s observed-head=%s merge=%s observed-at=%s\n", outcome.Kind, outcome.Number, outcome.ObservedState, outcome.ObservedMerged, outcome.ProposedRevision, outcome.ObservedHead, empty(outcome.MergeCommitOID), outcome.ObservedAt.Format(time.RFC3339Nano))
	}
	fmt.Fprintf(stdout, "  Absurd run: %s state=%s\n", empty(runEvidence.TaskID), empty(string(runEvidence.State)))
	if cleanupEvidence.TaskID != "" {
		fmt.Fprintf(stdout, "  Absurd cleanup: %s state=%s\n", cleanupEvidence.TaskID, cleanupEvidence.State)
	}
	fmt.Fprintln(stdout, "  Absurd history: use absurdctl dump-task --task-id=<task-id>")
	fmt.Fprintln(stdout, "  claims: agent text is carried by Message; claims do not prove readiness")
	for _, message := range messages {
		description := describeMessage(message, messages)
		if !job.AdmissionOpen && message.State == spine.AgentRunPending {
			description += "; delivery closed for cleanup before this harness turn started"
		}
		fmt.Fprintf(stdout, "  message %d %s: %s\n", message.Sequence, message.ID, description)
	}
	for _, action := range actions {
		fmt.Fprintf(stdout, "  observed action %s: %s external=%s evidence=%s\n", action.Kind, action.State, empty(action.ExternalID), empty(action.EvidenceDigest))
	}
	for _, check := range checks {
		scope := "historical"
		if check.Revision == job.Revision {
			scope = "current"
		}
		fmt.Fprintf(stdout, "  observed Check %s [%s]: %s Revision=%s exit=%d evidence=%s\n", check.Name, scope, check.State, check.Revision, check.ExitCode, empty(check.EvidenceDigest))
	}
	for _, plan := range plans {
		scope := "historical"
		if plan.Revision == job.Revision {
			scope = "current"
		}
		fmt.Fprintf(stdout, "  review plan [%s] Revision=%s state=%s decision=%s Roles=%v digest=%s\n", scope, plan.Revision, plan.State, plan.Plan.Decision, plan.Plan.Roles, empty(plan.PolicyDigest))
		for _, reason := range plan.Plan.Reasons {
			fmt.Fprintf(stdout, "    reason %s [%s]: %s\n", reason.Role, reason.Source, reason.Detail)
		}
	}
	for _, run := range reviewRuns {
		latency := time.Duration(0)
		if !run.StartedAt.IsZero() && !run.FinishedAt.IsZero() {
			latency = run.FinishedAt.Sub(run.StartedAt)
		}
		fmt.Fprintf(stdout, "  review AgentRun %s Role=%s Revision=%s state=%s feedback-message=%s capability=%s harness=%s thread=%s turn=%s latency=%s checkout=%s post=%s tree=%s reviewer-sandbox=%s/%s reviewer-route=%s/%s stale=%t\n", run.ID, run.Role, run.Revision, run.State, empty(run.FeedbackMessageID), empty(run.Capability), empty(run.Harness), empty(run.ThreadID), empty(run.TurnID), latency, empty(run.CheckoutState), empty(run.PostReviewState), empty(run.RevisionTree), empty(run.ReviewerSandboxID), empty(run.ReviewerSandboxState), empty(run.ReviewerRouteID), empty(run.ReviewerRouteState), run.Stale)
	}
	for _, record := range evidenceRecords {
		verification := "verified"
		if err := evidenceStore.Verify(record.Digest, record.ByteSize); err != nil {
			verification = "INVALID: " + err.Error()
		}
		fmt.Fprintf(stdout, "  Evidence %s: %s sha256=%s bytes=%d producer=%s Revision=%s rehash=%s\n", record.Kind, record.ID, record.Digest, record.ByteSize, record.Producer, empty(record.Revision), verification)
	}
	return nil
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
	verified, err := spine.VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, evidenceStore)
	if err != nil {
		return fmt.Errorf("current Revision %s readiness Evidence: %w", job.Revision, err)
	}
	fmt.Fprintf(stdout, "Revision %s: %d proving Evidence artifacts independently verified against Check rows\n", job.Revision, len(verified))
	return nil
}

func cleanup(ctx context.Context, store postgres.Store, client *absurd.Client, service spine.Service, args []string, stdout, stderr io.Writer) error {
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

func outcomeCommand(ctx context.Context, store postgres.Store, client *absurd.Client, githubClient githubapi.Client, args []string, stdout io.Writer) error {
	if len(args) != 2 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("outcome requires: JOB_ID <accepted|rejected|abandoned>")
	}
	requested := spine.JobOutcomeKind(strings.TrimSpace(args[1]))
	receipt, created, err := (outcomeapp.Service{Store: store, GitHub: githubClient}).Record(ctx, strings.TrimSpace(args[0]), requested)
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

type continuationStatus struct {
	Mode   string `json:"mode"`
	Actor  string `json:"actor"`
	Detail string `json:"detail"`
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

func continuationFor(job spine.Job, outcome *spine.JobOutcome, run, cleanup taskResultView) continuationStatus {
	if job.CleanupState == spine.CleanupComplete {
		if outcome == nil {
			return continuationStatus{Mode: "terminal", Actor: "none", Detail: "exact deterministic cleanup is complete and no GitHub proposal outcome was recorded"}
		}
		return continuationStatus{Mode: "terminal", Actor: "none", Detail: "authoritative outcome and exact deterministic cleanup are complete"}
	}
	if outcome != nil {
		if job.CleanupState == spine.CleanupPending || job.CleanupTaskID == "" {
			return continuationStatus{Mode: "attention", Actor: "outcome caller", Detail: "outcome is recorded but cleanup scheduling was interrupted; repeat the identical outcome command"}
		}
		if cleanup.State == "failed" || cleanup.State == "cancelled" || cleanup.State == "missing" {
			return continuationStatus{Mode: "attention", Actor: "operator", Detail: "the deterministic cleanup task is terminal without completing exact cleanup"}
		}
		return continuationStatus{Mode: "automatic-cleanup", Actor: "Dorf cleanup task", Detail: "the recorded external outcome authorizes only deterministic cleanup; Dorf does not merge or infer another outcome"}
	}
	switch job.WorkflowPhase {
	case "published":
		if run.State == absurd.TaskFailed || run.State == absurd.TaskCancelled || run.State == "missing" {
			return continuationStatus{Mode: "attention", Actor: "operator", Detail: "the Job task stopped observing the live proposal"}
		}
		return continuationStatus{Mode: "external-authority", Actor: "GitHub owner", Detail: "proposal is live; trusted comments continue the Job and merge or close records its outcome"}
	case "blocked", "publication-blocked":
		return continuationStatus{Mode: "attention", Actor: "operator", Detail: "durable attention must be resolved; no orchestration agent is silently advancing this Job"}
	}
	if job.AdmissionOpen && (run.State == "failed" || run.State == "cancelled" || run.State == "missing") {
		return continuationStatus{Mode: "attention", Actor: "operator", Detail: "the admitted Job task is terminal before the workflow reached its authority boundary"}
	}
	return continuationStatus{Mode: "self-advancing", Actor: "admitted Dorf worker", Detail: "persisted Job phase and Absurd tasks continue checks, review feedback, and exact-Revision publication"}
}

func queuedState(state spine.AgentRunState) string {
	if state == "" {
		return "queued"
	}
	return string(state)
}
func describeMessage(message spine.MessageView, messages []spine.MessageView) string {
	var detail string
	switch message.State {
	case "":
		detail = "queued for serialized delivery"
	case spine.AgentRunPending:
		detail = "queued for serialized delivery"
	case spine.AgentRunSubmitting:
		detail = "queued; delivery reconciliation is in progress"
	case spine.AgentRunActive:
		if message.Intent == spine.MessageSteer && message.TurnID != message.TargetTurnID {
			detail = "active harness turn started after the requested steer target became terminal"
		} else {
			detail = "active harness turn"
		}
	case spine.AgentRunCompleted:
		if message.Intent == spine.MessageSteer {
			if message.TurnID == message.TargetTurnID {
				detail = "delivered: steer accepted by the active harness turn"
			} else {
				detail = "terminal: harness turn started after the requested steer target became terminal"
			}
		} else {
			detail = "terminal: harness turn completed"
		}
	case spine.AgentRunFailed:
		if message.TurnID == "" {
			if strings.HasPrefix(message.Attention, "cleanup closed") {
				detail = "cleanup closed this input after history proved no harness acceptance"
			} else {
				detail = "harness delivery was not accepted; the same stable input remains retryable"
			}
		} else {
			detail = "terminal: harness turn failed"
		}
	case spine.AgentRunInterrupted:
		detail = "terminal: harness turn was interrupted"
	case spine.AgentRunUncertain:
		detail = "genuinely uncertain; delivery stopped without resubmission"
	default:
		detail = string(message.State)
	}
	if message.Harness != "" {
		detail += "; harness=" + message.Harness
	}
	if message.ThreadID != "" {
		detail += "; thread=" + message.ThreadID
	}
	if message.TurnID != "" {
		detail += fmt.Sprintf("; turn=%s outcome=%s", message.TurnID, empty(message.TurnOutcome))
	}
	if message.Intent == spine.MessageSteer && message.TargetTurnID != "" && message.TurnID != "" && message.TurnID != message.TargetTurnID {
		detail += "; requested steer target=" + message.TargetTurnID
	}
	if message.Attention != "" {
		detail += "; reason: " + message.Attention
	}
	if !message.Delivered && (message.State == spine.AgentRunFailed || message.State == spine.AgentRunInterrupted || message.State == spine.AgentRunUncertain) {
		detail += "; later FIFO input is blocked"
	}
	if message.BlockingSeq > 0 {
		return detail + fmt.Sprintf("; blocked by sequence %d (%s)", message.BlockingSeq, message.BlockingReason)
	}
	if message.State == "" || message.State == spine.AgentRunPending {
		for _, earlier := range messages {
			if earlier.Sequence >= message.Sequence {
				break
			}
			if !earlier.Delivered || earlier.State == spine.AgentRunActive || earlier.State == spine.AgentRunUncertain {
				return detail + fmt.Sprintf("; waiting behind sequence %d (%s)", earlier.Sequence, queuedState(earlier.State))
			}
		}
	}
	return detail
}
func usage(output io.Writer) error {
	fmt.Fprintln(output, "usage: dorf <version|host|setup|migrate|doctor|provider|image|admit|message|setup-retry|worker|inspect|evidence|outcome|cleanup> [options]")
	return fmt.Errorf("unknown or missing command")
}
