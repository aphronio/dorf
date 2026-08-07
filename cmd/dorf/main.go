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
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/proofbarrier"
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
	case "worker":
		return worker(ctx, client, cfg, args[1:], stdout, stderr)
	case "inspect":
		return inspect(ctx, store, args[1:], stdout, stderr)
	case "cleanup":
		return cleanup(ctx, store, client, service, args[1:], stdout, stderr)
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
	service := spine.Service{Store: postgres.Store{DB: db}, Externals: terminal.Externals{Sandbox: sandbox, Gateway: gateway.Gateway{StatePath: cfg.GatewayStatePath}, Agent: agent}, Barrier: barrier}
	workflow.Register(client, service)
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
	fmt.Fprintln(stdout, "PostgreSQL ready: Dorf migrations through 003_exactly_once_messages.sql; Absurd 0.5.0 queue dorf_jobs")
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
	job, created, err := workflow.Admit(ctx, store, client, postgres.NewJob{AdmissionKey: *key, Goal: goal, Repository: *repository, Revision: *revision, Branch: *branch, ProviderConnection: *provider, Model: *model, ReasoningEffort: *effort})
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
	if err := set.Parse(args); err != nil {
		return err
	}
	input, err := readInput(*inputFile, "message", "input")
	if err != nil {
		return err
	}
	accepted, created, err := workflow.AdmitMessage(ctx, store, client, postgres.NewMessage{JobID: *jobID, CallerID: *callerID, Input: input})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"job_id": accepted.JobID, "message_id": accepted.ID, "sequence": accepted.Sequence, "created": created, "accepted": true})
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

func inspect(ctx context.Context, store postgres.Store, args []string, stdout, stderr io.Writer) error {
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
	runEvidence, err := store.TaskEvidence(ctx, job.TaskID)
	if err != nil {
		return err
	}
	cleanupEvidence, err := store.TaskEvidence(ctx, job.CleanupTaskID)
	if err != nil {
		return err
	}
	view := map[string]any{"job": job, "messages": messages, "actions": actions, "absurd_run": runEvidence, "absurd_cleanup": cleanupEvidence, "transcript_authority": "Codex native Session (not copied into Dorf)"}
	if *jsonOutput {
		return writeJSON(stdout, view)
	}
	fmt.Fprintf(stdout, "Job %s\n  state: %s\n  admission: %s\n  cleanup: %s\n  goal: %s\n  repository: %s @ %s\n  sandbox: %s\n  route: %s\n  session: %s\n", job.ID, job.State, openClosed(job.AdmissionOpen), job.CleanupState, job.Goal, job.Repository, job.Revision, empty(job.SandboxID), empty(job.RouteID), empty(job.SessionID))
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
	fmt.Fprintln(stdout, "  transcript: Codex-owned native context (not stored by Dorf)")
	for _, message := range messages {
		description := describeMessage(message, messages)
		if !job.AdmissionOpen && message.State == spine.AgentRunPending {
			description += "; delivery closed for cleanup before this native turn started"
		}
		fmt.Fprintf(stdout, "  message %d %s: %s\n", message.Sequence, message.ID, description)
	}
	for _, action := range actions {
		fmt.Fprintf(stdout, "  action %s: %s attempts=%d external=%s\n", action.Kind, action.State, action.Attempts, empty(action.ExternalID))
	}
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
		detail = "active native turn"
	case spine.AgentRunCompleted:
		detail = "terminal: native turn completed"
	case spine.AgentRunFailed:
		detail = "terminal: native turn failed; later FIFO input is blocked"
	case spine.AgentRunInterrupted:
		detail = "terminal: native turn was interrupted; later FIFO input is blocked"
	case spine.AgentRunUncertain:
		detail = "genuinely uncertain; delivery stopped without resubmission"
	default:
		detail = string(message.State)
	}
	if message.NativeTurnID != "" {
		detail += fmt.Sprintf("; native=%s outcome=%s", message.NativeTurnID, empty(message.NativeOutcome))
	}
	if message.Attention != "" {
		detail += "; reason: " + message.Attention
	}
	if message.BlockingSeq > 0 {
		return detail + fmt.Sprintf("; blocked by sequence %d (%s)", message.BlockingSeq, message.BlockingReason)
	}
	if message.State == "" || message.State == spine.AgentRunPending {
		for _, earlier := range messages {
			if earlier.Sequence >= message.Sequence {
				break
			}
			if earlier.State != spine.AgentRunCompleted {
				return detail + fmt.Sprintf("; waiting behind sequence %d (%s)", earlier.Sequence, queuedState(earlier.State))
			}
		}
	}
	return detail
}
func usage(output io.Writer) error {
	fmt.Fprintln(output, "usage: dorf <migrate|doctor|admit|message|worker|inspect|cleanup> [options]")
	return fmt.Errorf("unknown or missing command")
}
