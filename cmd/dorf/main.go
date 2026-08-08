package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/proofbarrier"
	"github.com/aphronio/dorf/internal/publication"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/terminal"
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
	cfg, err := config.Load()
	if err != nil {
		return err
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
	}
	client, service, err := application(db, cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	switch args[0] {
	case "admit":
		return admit(ctx, store, client, args[1:], stdout, stderr)
	case "message":
		return message(ctx, store, client, args[1:], stdout, stderr)
	case "setup-retry":
		return setupRetry(ctx, store, client, args[1:], stdout, stderr)
	case "worker":
		return worker(ctx, client, cfg, args[1:], stdout, stderr)
	case "inspect":
		return inspect(ctx, store, evidence.Store{Root: cfg.EvidenceRoot}, args[1:], stdout, stderr)
	case "evidence":
		return evidenceCommand(ctx, store, evidence.Store{Root: cfg.EvidenceRoot}, args[1:], stdout, stderr)
	case "cleanup":
		return cleanup(ctx, store, client, service, args[1:], stdout, stderr)
	case "review":
		return reviewCommand(ctx, store, service, evidence.Store{Root: cfg.EvidenceRoot}, args[1:], stdout, stderr)
	case "publication":
		return publicationCommand(ctx, store, client, service.Barrier, args[1:], stdout, stderr)
	default:
		_ = service
		return usage(stderr)
	}
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
	workflow.Register(client, service, store)
	publication.Register(client, publicationService)
	return client, service, nil
}

func migrate(ctx context.Context, store postgres.Store, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("migrate", flag.ContinueOnError)
	set.SetOutput(stderr)
	absurdSchema := set.String("absurd-schema", "", "path to the exact upstream Absurd 0.5.0 absurd.sql (required only for first initialization)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *absurdSchema != "" {
		contents, err := os.ReadFile(*absurdSchema)
		if err != nil {
			return fmt.Errorf("read Absurd schema: %w", err)
		}
		if err := store.BootstrapAbsurd(ctx, contents); err != nil {
			return err
		}
	}
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "PostgreSQL ready: Dorf migrations through 009_github_publication.sql; Absurd 0.5.0 queue dorf_jobs")
	return nil
}

func runDoctor(ctx context.Context, db *sql.DB, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("doctor", flag.ContinueOnError)
	set.SetOutput(stderr)
	connection := set.String("provider", "", "named Provider Connection")
	if err := set.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*connection) == "" {
		return fmt.Errorf("doctor requires --provider")
	}
	checks := doctor.Run(ctx, db, cfg, *connection)
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

func admit(ctx context.Context, store postgres.Store, client *absurd.Client, args []string, stdout, stderr io.Writer) error {
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
	job, created, err := workflow.Admit(ctx, store, client, postgres.NewJob{AdmissionKey: *key, Goal: goal, Repository: *repository, Revision: *revision, Branch: *branch, ProviderConnection: *provider, Model: *model, ReasoningEffort: *effort, GitHubRepository: *githubRepository, GitHubInstallation: *githubInstallation, BaseBranch: *base})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": job.ID, "created": created, "state": job.State, "task_id": job.TaskID, "scheduled": true})
}

func message(ctx context.Context, store postgres.Store, client *absurd.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("message", flag.ContinueOnError)
	set.SetOutput(stderr)
	jobID := set.String("job", "", "existing Job ID")
	callerID := set.String("id", "", "stable caller message identity")
	inputFile := set.String("input-file", "", "path containing the complete message input")
	intent := set.String("intent", string(spine.MessageFollow), "harness delivery intent: follow or steer")
	if err := set.Parse(args); err != nil {
		return err
	}
	input, err := readInput(*inputFile, "message", "input")
	if err != nil {
		return err
	}
	accepted, created, err := workflow.AdmitMessage(ctx, store, client, postgres.NewMessage{JobID: *jobID, CallerID: *callerID, Input: input, Intent: spine.MessageDeliveryIntent(*intent)})
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

func inspect(ctx context.Context, store postgres.Store, evidenceStore evidence.Store, args []string, stdout, stderr io.Writer) error {
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
	runEvidence, err := store.TaskEvidence(ctx, job.TaskID)
	if err != nil {
		return err
	}
	cleanupEvidence, err := store.TaskEvidence(ctx, job.CleanupTaskID)
	if err != nil {
		return err
	}
	publicationEvidence, err := store.PublicationTaskHistory(ctx, job)
	if err != nil {
		return err
	}
	view := map[string]any{"job": job, "readiness": assessment, "proposal": proposal, "review_plans": plans, "review_agent_runs": reviewRuns, "claims": map[string]any{"implementation_agent_runs": messages, "review_findings": reviewRuns, "authority": "Codex native Sessions; agent statements and findings are claims and do not satisfy Checks"}, "observed_facts": map[string]any{"actions": actions, "checks": checks, "evidence": evidenceRecords, "current_revision_evidence_verification": assessment.Evidence}, "absurd_run": runEvidence, "absurd_publications": publicationEvidence, "absurd_cleanup": cleanupEvidence, "transcript_authority": "Codex native Sessions (not copied into Dorf)"}
	if *jsonOutput {
		return writeJSON(stdout, view)
	}
	fmt.Fprintf(stdout, "Job %s\n  state: %s\n  workflow: %s\n  readiness: %s — %s\n  admission: %s\n  cleanup: %s\n  goal: %s\n  repository: %s\n  starting Revision: %s\n  current Revision: %s\n  sandbox: %s\n  route: %s\n  session: %s\n", job.ID, job.State, job.WorkflowPhase, assessment.Status, assessment.Reason, openClosed(job.AdmissionOpen), job.CleanupState, job.Goal, job.Repository, job.StartingRevision, job.Revision, empty(job.SandboxID), empty(job.RouteID), empty(job.SessionID))
	if job.WorkflowAttention != "" {
		fmt.Fprintf(stdout, "  attention: %s\n", job.WorkflowAttention)
	}
	if proposal == nil {
		fmt.Fprintln(stdout, "  proposal: none")
	} else {
		fmt.Fprintf(stdout, "  proposal: #%d %s Revision=%s remote=%s body=%s stale=%t\n", proposal.Number, proposal.URL, proposal.ProposedRevision, proposal.ObservedRemoteHead, proposal.BodyDigest, proposal.Stale)
	}
	fmt.Fprintf(stdout, "  Absurd run: %s state=%s attempts=%d checkpoints=%d\n", empty(runEvidence.TaskID), empty(runEvidence.State), runEvidence.Attempts, runEvidence.Checkpoints)
	if job.RunTerminalState != "" {
		if job.RunTerminalState == "cancelled" && !job.AdmissionOpen {
			fmt.Fprintln(stdout, "  delivery task: cancelled after admission closed for cleanup")
		} else {
			fmt.Fprintf(stdout, "  durable run terminal: %s (delivery infrastructure stopped; cleanup remains independent)\n", job.RunTerminalState)
		}
	}
	if cleanupEvidence.TaskID != "" {
		fmt.Fprintf(stdout, "  Absurd cleanup: %s state=%s attempts=%d checkpoints=%d\n", cleanupEvidence.TaskID, cleanupEvidence.State, cleanupEvidence.Attempts, cleanupEvidence.Checkpoints)
	}
	for _, task := range publicationEvidence {
		fmt.Fprintf(stdout, "  Absurd publication: %s key=%s generation=%d state=%s attempts=%d checkpoints=%d current=%t\n", task.TaskID, task.IdempotencyKey, task.Attempt, task.State, task.Attempts, task.Checkpoints, task.Current)
	}
	fmt.Fprintln(stdout, "  claims: implementation and repair prose remain in the Codex-owned native context; claims do not prove readiness")
	for _, message := range messages {
		description := describeMessage(message, messages)
		if !job.AdmissionOpen && message.State == spine.AgentRunPending {
			description += "; delivery closed for cleanup before this native turn started"
		}
		fmt.Fprintf(stdout, "  message %d %s: %s\n", message.Sequence, message.ID, description)
	}
	for _, action := range actions {
		fmt.Fprintf(stdout, "  observed action %s: %s attempts=%d external=%s evidence=%s\n", action.Kind, action.State, action.Attempts, empty(action.ExternalID), empty(action.EvidenceDigest))
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
		fmt.Fprintf(stdout, "  review plan [%s] Revision=%s state=%s decision=%s Roles=%v requested=%v requested-by=%s digest=%s\n", scope, plan.Revision, plan.State, plan.Final.Decision, plan.Final.Roles, plan.RequestedRoles, empty(plan.RequestedByRunID), empty(plan.PolicyDigest))
		for _, reason := range plan.Final.Reasons {
			fmt.Fprintf(stdout, "    reason %s [%s]: %s\n", reason.Role, reason.Source, reason.Detail)
		}
		if plan.TriageRationale != "" {
			fmt.Fprintf(stdout, "    triage claim %s: %s\n", plan.TriageRunID, plan.TriageRationale)
		}
	}
	for _, run := range reviewRuns {
		latency := time.Duration(0)
		if !run.StartedAt.IsZero() && !run.FinishedAt.IsZero() {
			latency = run.FinishedAt.Sub(run.StartedAt)
		}
		fmt.Fprintf(stdout, "  review AgentRun %s Role=%s Revision=%s state=%s capability=%s session=%s turn=%s latency=%s usage-available=%t usage=%d/%d cached=%d cost-microusd=%d yield=%d workspace=%s checkout=%s post=%s tree=%s reviewer-sandbox=%s/%s reviewer-route=%s/%s controller=%s stale=%t\n", run.ID, run.Role, run.Revision, run.State, empty(run.Capability), empty(run.SessionID), empty(run.NativeTurnID), latency, run.UsageAvailable, run.InputTokens, run.OutputTokens, run.CachedInputTokens, run.CostMicrousd, run.YieldCount, empty(run.Workspace), empty(run.CheckoutState), empty(run.PostReviewState), empty(run.RevisionTree), empty(run.ReviewerSandboxID), empty(run.ReviewerSandboxState), empty(run.ReviewerRouteID), empty(run.ReviewerRouteState), empty(run.ReviewerAppServer), run.Stale)
		if run.Finding != nil {
			fmt.Fprintf(stdout, "    claim material=%t adjudication=%s evidence=%s summary=%s\n", run.Finding.Material, run.Finding.Adjudication, run.Finding.EvidenceID, run.Finding.Summary)
		}
	}
	for _, record := range evidenceRecords {
		verification := "verified"
		if err := evidenceStore.Verify(record.Digest, record.ByteSize); err != nil {
			verification = "INVALID: " + err.Error()
		}
		fmt.Fprintf(stdout, "  Evidence %s: %s sha256=%s bytes=%d provenance=%s producer=%s Revision=%s rehash=%s\n", record.Kind, record.ID, record.Digest, record.ByteSize, record.Provenance, record.Producer, empty(record.Revision), verification)
	}
	return nil
}

type roleFlags []policy.Role

func (r *roleFlags) String() string {
	values := make([]string, 0, len(*r))
	for _, role := range *r {
		values = append(values, string(role))
	}
	return strings.Join(values, ",")
}

func (r *roleFlags) Set(value string) error {
	*r = append(*r, policy.Role(strings.TrimSpace(value)))
	return nil
}

func reviewCommand(ctx context.Context, store postgres.Store, service spine.Service, evidenceStore evidence.Store, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "activate" {
		return fmt.Errorf("review requires: activate JOB_ID --revision EXACT_OID [--requested-role ROLE]")
	}
	set := flag.NewFlagSet("review activate", flag.ContinueOnError)
	set.SetOutput(stderr)
	revision := set.String("revision", "", "exact committed Revision already proven by Checks")
	requestedBy := set.String("requested-by-agent-run", "", "original implementation AgentRun that requested additional review")
	var roles roleFlags
	set.Var(&roles, "requested-role", "allowlisted additional Role requested by the implementation AgentRun (repeatable)")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() != 1 || !postgres.ValidRevision(*revision) {
		return fmt.Errorf("review activate requires one Job ID and --revision with a lowercase full commit OID")
	}
	job, err := store.Job(ctx, set.Arg(0))
	if err != nil {
		return err
	}
	if job.Revision != *revision {
		return fmt.Errorf("activation Revision %s conflicts with Job current Revision %s", *revision, job.Revision)
	}
	declared, err := store.DeclaredChecks(ctx, job.ID)
	if err != nil {
		return err
	}
	checks, err := store.Checks(ctx, job.ID)
	if err != nil {
		return err
	}
	records, err := store.Evidence(ctx, job.ID)
	if err != nil {
		return err
	}
	if _, err := spine.VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, evidenceStore); err != nil {
		return fmt.Errorf("review activation requires independently verified exact-Revision Check Evidence: %w", err)
	}
	activation, created, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: *revision, RequestedRoles: []policy.Role(roles), RequestedByRunID: *requestedBy})
	if err != nil {
		return err
	}
	disposition, err := service.RunUntilIdle(ctx, job.ID)
	if err != nil {
		return err
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil {
		return err
	}
	plan, planErr := store.ReviewPlan(ctx, job.ID, job.Revision)
	runs, runsErr := store.AllReviewRuns(ctx, job.ID)
	if planErr != nil || runsErr != nil {
		return errors.Join(planErr, runsErr)
	}
	if err := writeJSON(stdout, map[string]any{"job_id": job.ID, "revision": job.Revision, "activation_created": created, "activation": activation, "plan": plan, "review_agent_runs": runs, "workflow_phase": job.WorkflowPhase, "disposition": disposition}); err != nil {
		return err
	}
	if disposition == spine.RunBlocked || job.WorkflowPhase == "blocked" {
		return fmt.Errorf("review workflow stopped visibly: %s", job.WorkflowAttention)
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
	now := set.Bool("now", false, "reconcile the exact route and Sandbox synchronously after durable scheduling")
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
	if *now && job.CleanupState != spine.CleanupComplete {
		if err := service.Cleanup(ctx, job.ID); err != nil {
			return fmt.Errorf("synchronous exact cleanup: %w", err)
		}
		job, err = store.Job(ctx, job.ID)
		if err != nil {
			return err
		}
	}
	return writeJSON(stdout, map[string]any{"job_id": job.ID, "cleanup": job.CleanupState, "task_id": job.CleanupTaskID, "scheduled": job.CleanupState == spine.CleanupScheduled, "synchronous": *now})
}

func publicationCommand(ctx context.Context, store postgres.Store, client *absurd.Client, barrier any, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "publish" {
		return fmt.Errorf("publication requires: publish JOB_ID --revision EXACT_OID")
	}
	set := flag.NewFlagSet("publication publish", flag.ContinueOnError)
	set.SetOutput(stderr)
	revision := set.String("revision", "", "exact ready Revision")
	jobID, err := parsePublicationTarget(set, args[1:], "publication publish")
	if err != nil {
		return err
	}
	if !postgres.ValidRevision(*revision) {
		return fmt.Errorf("publication publish requires one Job ID and --revision with a lowercase full commit OID")
	}
	params, taskID, created, err := publication.Schedule(ctx, store, client, barrier, jobID, *revision)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": params.JobID, "revision": params.Revision, "attempt": params.Attempt, "task_id": taskID, "created": created, "scheduled": true})
}

func parsePublicationTarget(set *flag.FlagSet, args []string, command string) (string, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" || strings.HasPrefix(args[0], "-") {
		return "", fmt.Errorf("%s requires one Job ID before its flags", command)
	}
	jobID := strings.TrimSpace(args[0])
	if err := set.Parse(args[1:]); err != nil {
		return "", err
	}
	if set.NArg() != 0 {
		return "", fmt.Errorf("%s accepts exactly one Job ID before its flags", command)
	}
	return jobID, nil
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
		if message.Intent == spine.MessageSteer && message.NativeTurnID != message.TargetTurnID {
			detail = "active native turn started after the requested steer target became terminal"
		} else {
			detail = "active native turn"
		}
	case spine.AgentRunCompleted:
		if message.Intent == spine.MessageSteer {
			if message.NativeTurnID == message.TargetTurnID {
				detail = "delivered: steer accepted by the active native turn"
			} else {
				detail = "terminal: native turn started after the requested steer target became terminal"
			}
		} else {
			detail = "terminal: native turn completed"
		}
	case spine.AgentRunFailed:
		if message.NativeTurnID == "" {
			if strings.HasPrefix(message.Attention, "cleanup closed") {
				detail = "cleanup closed this input after history proved no native acceptance"
			} else {
				detail = "native delivery was not accepted; the same stable input remains retryable"
			}
		} else {
			detail = "terminal: native turn failed"
		}
	case spine.AgentRunInterrupted:
		detail = "terminal: native turn was interrupted"
	case spine.AgentRunUncertain:
		detail = "genuinely uncertain; delivery stopped without resubmission"
	default:
		detail = string(message.State)
	}
	if message.NativeTurnID != "" {
		detail += fmt.Sprintf("; native=%s outcome=%s", message.NativeTurnID, empty(message.NativeOutcome))
	}
	if message.Intent == spine.MessageSteer && message.TargetTurnID != "" && message.NativeTurnID != "" && message.NativeTurnID != message.TargetTurnID {
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
	fmt.Fprintln(output, "usage: dorf <migrate|doctor|admit|message|setup-retry|worker|inspect|evidence|review|publication|cleanup> [options]")
	return fmt.Errorf("unknown or missing command")
}
