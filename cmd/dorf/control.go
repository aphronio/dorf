package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aphronio/dorf/internal/clientconfig"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlclient"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/postgres"
	profileapp "github.com/aphronio/dorf/internal/profile"
	"github.com/aphronio/dorf/internal/version"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const defaultControlAddress = "127.0.0.1:8745"

const maxControlModelBytes = 1024

// remoteCommand handles commands that can run on a client-only machine. A
// saved connection makes `run` remote; without one, the existing local command
// remains authoritative.
func remoteCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "connect":
		return true, connectCommand(ctx, args[1:], os.Stdin, stdout, stderr)
	case "auth":
		return true, authCommand(ctx, args[1:], stdout)
	case "job":
		return true, remoteJobCommand(ctx, args[1:], stdout, stderr)
	case "run":
		cfg, _, found, err := loadClientConfig()
		if err != nil || !found {
			return false, err
		}
		client, err := controlclient.New(cfg.DeploymentURL, cfg.Credential, nil)
		if err != nil {
			return true, err
		}
		return true, remoteRun(ctx, client, cfg.DeploymentURL, args[1:], stdout, stderr)
	default:
		return false, nil
	}
}

func connectCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("connect", flag.ContinueOnError)
	set.SetOutput(stderr)
	name := set.String("name", defaultClientName(), "name for this CLI installation")
	enrollmentFile := set.String("enrollment-file", "", "one-time enrollment code file; use - for standard input")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("connect requires one HTTPS Dorf Deployment URL")
	}
	deploymentURL, err := clientconfig.NormalizeDeploymentURL(set.Arg(0))
	if err != nil {
		return err
	}

	stored, path, found, err := loadClientConfig()
	if err != nil {
		return err
	}
	credential := ""
	if found && stored.DeploymentURL == deploymentURL {
		credential = stored.Credential
	}
	if credential == "" {
		credential, err = controlauth.GenerateCredential()
		if err != nil {
			return err
		}
	}
	client, err := controlclient.New(deploymentURL, credential, nil)
	if err != nil {
		return err
	}
	discovery, err := client.Discover(ctx)
	if err != nil {
		return err
	}
	if discovery.Product != "dorf" || !slices.Contains(discovery.Capabilities, "direct_jobs") {
		return fmt.Errorf("the HTTPS endpoint is not a compatible Dorf direct-Job API")
	}
	if found && stored.DeploymentURL == deploymentURL {
		if identity, authErr := client.Me(ctx); authErr == nil {
			return renderConnection(stdout, deploymentURL, path, identity, true)
		} else if !problemCode(authErr, "unauthenticated") {
			return authErr
		}
		credential, err = controlauth.GenerateCredential()
		if err != nil {
			return err
		}
		client, err = controlclient.New(deploymentURL, credential, nil)
		if err != nil {
			return err
		}
	}

	if strings.TrimSpace(*enrollmentFile) == "" {
		file, interactive := stdin.(*os.File)
		if !interactive || !isTerminal(file) {
			return fmt.Errorf("non-interactive connect requires --enrollment-file PATH or -")
		}
	}
	code, err := readEnrollment(*enrollmentFile, stdin, stderr)
	if err != nil {
		return err
	}
	newConfig := clientconfig.Config{DeploymentURL: deploymentURL, Credential: credential}
	if err := clientconfig.Save(path, newConfig); err != nil {
		return err
	}
	identity, err := client.RedeemEnrollment(ctx, code, *name)
	if err != nil {
		if definitiveClientError(err) {
			var restoreErr error
			if found {
				restoreErr = clientconfig.Save(path, stored)
			} else {
				restoreErr = clientconfig.Remove(path)
			}
			if restoreErr != nil {
				return fmt.Errorf("%w; restore prior Dorf connection: %v", err, restoreErr)
			}
		}
		return err
	}
	return renderConnection(stdout, deploymentURL, path, identity, false)
}

func authCommand(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "status" {
		return fmt.Errorf("auth requires: status")
	}
	cfg, path, client, err := loadConnectedClient()
	if err != nil {
		return err
	}
	identity, err := client.Me(ctx)
	if err != nil {
		return err
	}
	return renderConnection(stdout, cfg.DeploymentURL, path, identity, true)
}

func remoteRun(ctx context.Context, client *controlclient.Client, deploymentURL string, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("run", flag.ContinueOnError)
	set.SetOutput(stderr)
	key := set.String("key", "", "stable request identity for explicit replay")
	goalFile := set.String("goal-file", "", "path containing the complete goal")
	model := set.String("model", "", "Harness model")
	effort := set.String("reasoning", "high", "Harness reasoning effort")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("run does not accept positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	goal, err := readInput(*goalFile, "run", "goal")
	if err != nil {
		return err
	}
	requestKey, generated, err := directAdmissionKey(*key, rand.Reader)
	if err != nil {
		return err
	}
	request := controlapi.AdmitJobRequest{
		Goal: goal, Profile: strings.TrimSpace(*profileName), Model: strings.TrimSpace(*model), Reasoning: strings.TrimSpace(*effort),
	}
	job, err := client.AdmitJob(ctx, requestKey, request)
	retried := retryableAdmissionError(ctx, err)
	if retried {
		job, err = client.AdmitJob(ctx, requestKey, request)
	}
	if err != nil {
		if generated && retried {
			fmt.Fprintf(stderr, "Admission may have succeeded. Retry the same request with --key %s.\n", requestKey)
		}
		return err
	}
	if *output == "json" {
		return writeJSON(stdout, remoteJobReceipt{Deployment: deploymentURL, RequestID: requestKey, Job: job})
	}
	fmt.Fprintf(stdout, "Job %s accepted by %s\n", job.ID, deploymentURL)
	renderRemoteJob(stdout, job)
	fmt.Fprintf(stdout, "Next: dorf job inspect %s\n", job.ID)
	return nil
}

func remoteJobCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "inspect" && args[0] != "cleanup") {
		return fmt.Errorf("job requires: inspect JOB_ID or cleanup JOB_ID")
	}
	cfg, _, client, err := loadConnectedClient()
	if err != nil {
		return err
	}
	set := flag.NewFlagSet("job "+args[0], flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("job %s requires one Job ID", args[0])
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var job controlapi.Job
	if args[0] == "inspect" {
		job, err = client.Job(ctx, set.Arg(0))
	} else {
		job, err = client.Cleanup(ctx, set.Arg(0))
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		if args[0] == "cleanup" {
			return writeJSON(stdout, remoteJobReceipt{Deployment: cfg.DeploymentURL, Job: job})
		}
		return writeJSON(stdout, job)
	}
	if args[0] == "cleanup" {
		fmt.Fprintf(stdout, "Cleanup requested for Job %s on %s\n", job.ID, cfg.DeploymentURL)
	}
	renderRemoteJob(stdout, job)
	return nil
}

type remoteJobReceipt struct {
	Deployment string         `json:"deployment"`
	RequestID  string         `json:"request_id,omitempty"`
	Job        controlapi.Job `json:"job"`
}

func renderRemoteJob(output io.Writer, job controlapi.Job) {
	fmt.Fprintf(output, "  goal: %q\n  profile: %s\n  model: %q (%s)\n  admission: %s\n  execution: %s\n  cleanup: %s\n",
		job.Goal, job.Profile, job.Model, job.Reasoning, openClosed(job.Admission.Open), job.Execution.State, job.Cleanup.State)
	if job.Attention != nil {
		fmt.Fprintf(output, "  attention: %s\n", job.Attention.Detail)
	}
	for _, sandbox := range job.Sandboxes {
		fmt.Fprintf(output, "  Sandbox: %s (%s)\n", sandbox.ID, sandbox.Name)
	}
}

func renderConnection(output io.Writer, deploymentURL, path string, identity controlapi.Identity, existing bool) error {
	state := "connected"
	if existing {
		state = "authenticated"
	}
	fmt.Fprintf(output, "Dorf %s\n  Deployment: %s\n  Principal: %s\n  Client: %s (%s)\n  Credential expires: %s\n  Client configuration: %s\n",
		state, deploymentURL, identity.Principal.Name, identity.Client.Name, identity.Client.ID,
		identity.Client.ExpiresAt.Format(time.RFC3339), path)
	return nil
}

func readEnrollment(path string, stdin io.Reader, output io.Writer) (string, error) {
	if strings.TrimSpace(path) != "" {
		return readSecretFile(path, stdin)
	}
	fmt.Fprint(output, "Paste the one-time Dorf enrollment code and press Enter: ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("enrollment code is empty")
	}
	return line, nil
}

func loadClientConfig() (clientconfig.Config, string, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return clientconfig.Config{}, "", false, fmt.Errorf("resolve user home for Dorf client configuration: %w", err)
	}
	path := clientconfig.Path(home)
	cfg, found, err := clientconfig.Load(path)
	return cfg, path, found, err
}

func loadConnectedClient() (clientconfig.Config, string, *controlclient.Client, error) {
	cfg, path, found, err := loadClientConfig()
	if err != nil {
		return clientconfig.Config{}, path, nil, err
	}
	if !found {
		return clientconfig.Config{}, path, nil, fmt.Errorf("Dorf is not connected; run dorf connect HTTPS_URL")
	}
	client, err := controlclient.New(cfg.DeploymentURL, cfg.Credential, nil)
	return cfg, path, client, err
}

func defaultClientName() string {
	hostname, _ := os.Hostname()
	value := strings.ToLower(strings.TrimSpace(hostname))
	var normalized strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			normalized.WriteRune(r)
		} else {
			normalized.WriteByte('-')
		}
		if normalized.Len() == 63 {
			break
		}
	}
	value = strings.Trim(normalized.String(), ".-_")
	if value == "" {
		return "dorf-cli"
	}
	return value
}

func validateOutput(value string) error {
	if value != "human" && value != "json" {
		return fmt.Errorf("output must be human or json")
	}
	return nil
}

func problemCode(err error, code string) bool {
	var problem *controlclient.ProblemError
	return errors.As(err, &problem) && problem.Problem.Code == code
}

func definitiveClientError(err error) bool {
	var problem *controlclient.ProblemError
	return errors.As(err, &problem) && problem.Problem.Status >= 400 && problem.Problem.Status < 500
}

func retryableAdmissionError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var problem *controlclient.ProblemError
	if !errors.As(err, &problem) {
		return true
	}
	return problem.Problem.Status >= 500 && problem.Problem.Status < 600
}

type controlAPIJobs struct {
	store   postgres.Store
	tasks   *absurd.Client
	gateway gateway.Gateway
}

func (a controlAPIJobs) AdmitDirect(ctx context.Context, key string, input controlapi.AdmitJobRequest) (controlapi.Job, bool, error) {
	if key == "" || key != strings.TrimSpace(key) || len(key) > 255 {
		return controlapi.Job{}, false, controlapi.ErrInvalidInput
	}
	input.Profile = strings.TrimSpace(input.Profile)
	input.Model = strings.TrimSpace(input.Model)
	input.Reasoning = strings.TrimSpace(input.Reasoning)
	if strings.TrimSpace(input.Goal) == "" || len(input.Goal) > 1<<20 || strings.ContainsRune(input.Goal, 0) ||
		input.Model == "" || len(input.Model) > maxControlModelBytes || strings.ContainsRune(input.Model, 0) ||
		strings.ContainsRune(input.Profile, 0) || strings.ContainsRune(input.Reasoning, 0) {
		return controlapi.Job{}, false, controlapi.ErrInvalidInput
	}
	if input.Reasoning == "" {
		input.Reasoning = "high"
	}
	if input.Reasoning != "low" && input.Reasoning != "medium" && input.Reasoning != "high" && input.Reasoning != "xhigh" {
		return controlapi.Job{}, false, controlapi.ErrInvalidInput
	}

	admission := core.JobAdmission{AdmissionKey: key, Goal: input.Goal, Model: input.Model, ReasoningEffort: input.Reasoning}
	profile := core.SandboxProfile{}
	existing, err := a.store.Job(ctx, core.JobID(key))
	switch {
	case err == nil:
		if existing.Workflow != "" || existing.WorkflowRevision != "" || (input.Profile != "" && input.Profile != existing.SandboxProfile) {
			return controlapi.Job{}, false, controlapi.ErrIdempotencyConflict
		}
		admission.SandboxProfile = existing.SandboxProfile
		admission.ProviderConnection = existing.ProviderConnection
	case errors.Is(err, postgres.ErrNotFound):
		profile, err = selectedSandboxProfile(ctx, a.store, input.Profile)
		if err != nil {
			if input.Profile != "" && errors.Is(err, postgres.ErrProfileNotFound) {
				return controlapi.Job{}, false, fmt.Errorf("%w: %v", controlapi.ErrInvalidInput, err)
			}
			return controlapi.Job{}, false, err
		}
		admission.SandboxProfile = profile.Name
		admission.ProviderConnection, err = selectedAIConnection(a.gateway, "")
		if err != nil {
			return controlapi.Job{}, false, err
		}
	default:
		return controlapi.Job{}, false, err
	}

	job, created, err := direct.Admit(ctx, a.store, coreApplication(a.store, a.tasks), a.gateway,
		profileapp.Runtime{SandboxProfile: admission.SandboxProfile}, admission)
	if errors.Is(err, postgres.ErrAdmissionConflict) {
		return controlapi.Job{}, false, controlapi.ErrIdempotencyConflict
	}
	if err != nil {
		return controlapi.Job{}, false, err
	}
	view, err := a.project(ctx, job)
	return view, created, err
}

func (a controlAPIJobs) Get(ctx context.Context, jobID string) (controlapi.Job, error) {
	job, err := a.directJob(ctx, jobID)
	if err != nil {
		return controlapi.Job{}, err
	}
	return a.project(ctx, job)
}

func (a controlAPIJobs) RequestCleanup(ctx context.Context, jobID string) (controlapi.Job, error) {
	job, err := a.directJob(ctx, jobID)
	if err != nil {
		return controlapi.Job{}, err
	}
	handle, err := coreApplication(a.store, a.tasks).OpenJob(ctx, job.ID)
	if err != nil {
		return controlapi.Job{}, err
	}
	if err := handle.RequestCleanup(ctx); err != nil {
		return controlapi.Job{}, err
	}
	return a.Get(ctx, job.ID)
}

func (a controlAPIJobs) directJob(ctx context.Context, jobID string) (core.Job, error) {
	job, err := a.store.Job(ctx, jobID)
	if errors.Is(err, postgres.ErrNotFound) || (err == nil && (job.Workflow != "" || job.WorkflowRevision != "")) {
		return core.Job{}, controlapi.ErrJobNotFound
	}
	return job, err
}

func (a controlAPIJobs) project(ctx context.Context, job core.Job) (controlapi.Job, error) {
	snapshot, err := direct.LoadSnapshot(ctx, a.store, job)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return controlapi.Job{}, controlapi.ErrJobNotFound
		}
		return controlapi.Job{}, err
	}
	projection := snapshot.Project()
	task, err := fetchTaskResult(ctx, a.tasks, job.CurrentTaskID)
	if err != nil {
		return controlapi.Job{}, err
	}
	executionState, cleanupState, attention, err := publicJobStates(job, projection, task.State)
	if err != nil {
		return controlapi.Job{}, err
	}
	sandboxes := make([]controlapi.Sandbox, 0, len(snapshot.Sandboxes))
	for _, sandbox := range snapshot.Sandboxes {
		sandboxes = append(sandboxes, controlapi.Sandbox{ID: sandbox.ID, Name: sandbox.Name})
	}
	return controlapi.Job{
		ID: job.ID, Kind: "direct", Goal: job.Goal, Profile: job.SandboxProfile, Model: job.Model, Reasoning: job.ReasoningEffort,
		Admission: controlapi.Admission{Open: job.AdmissionOpen}, Execution: controlapi.State{State: executionState},
		Attention: attention, Cleanup: controlapi.State{State: cleanupState}, Sandboxes: sandboxes,
	}, nil
}

func publicJobStates(job core.Job, projection direct.Projection, taskState absurd.TaskResultState) (string, string, *controlapi.Attention, error) {
	executionState := map[direct.ExecutionState]string{
		direct.ExecutionProvisioningSandbox: "provisioning_sandbox",
		direct.ExecutionConnectingRoute:     "connecting_model_access",
		direct.ExecutionAwaitingAgent:       "awaiting_agent",
		direct.ExecutionAttention:           "stopped",
		direct.ExecutionIdle:                "idle",
	}[projection.State]
	if executionState == "" {
		return "", "", nil, fmt.Errorf("Job %s has unknown direct execution state %q", job.ID, projection.State)
	}
	cleanupState := map[core.CleanupState]string{
		core.CleanupPending: "not_requested", core.CleanupRequested: "requested",
		core.CleanupScheduled: "running", core.CleanupComplete: "complete",
	}[job.CleanupState]
	if cleanupState == "" {
		return "", "", nil, fmt.Errorf("Job %s has unknown cleanup state %q", job.ID, job.CleanupState)
	}
	var attention *controlapi.Attention
	if (job.CleanupState == core.CleanupPending && failedExecutionTask(taskState)) ||
		(job.CleanupState == core.CleanupRequested && taskState == absurd.TaskFailed) {
		executionState = "failed"
		attention = &controlapi.Attention{Code: "execution_failed", Detail: "Job execution stopped; inspect the deployment service logs, repair the cause, then retry."}
	} else if projection.State == direct.ExecutionAttention {
		code := "agent_attention"
		if job.WorkflowAttention != "" {
			code = "job_attention"
		}
		attention = &controlapi.Attention{Code: code, Detail: "Job execution needs operator attention; inspect the deployment service logs."}
	}
	if job.CleanupState != core.CleanupPending {
		if executionState == "provisioning_sandbox" || executionState == "connecting_model_access" || executionState == "awaiting_agent" {
			executionState = "stopped"
		}
		if job.CleanupState == core.CleanupScheduled && failedExecutionTask(taskState) {
			cleanupState = "failed"
			attention = &controlapi.Attention{Code: "cleanup_failed", Detail: "Cleanup stopped before all resources were released; inspect the deployment service logs."}
		}
	}
	return executionState, cleanupState, attention, nil
}

func failedExecutionTask(state absurd.TaskResultState) bool {
	return state == "" || state == "missing" || state == absurd.TaskFailed || state == absurd.TaskCancelled
}

func clientCommand(ctx context.Context, store postgres.Store, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("client requires: enroll or revoke CLIENT_ID")
	}
	auth := controlauth.Service{Store: store}
	switch args[0] {
	case "enroll":
		if len(args) != 1 {
			return fmt.Errorf("client enroll does not accept arguments")
		}
		enrollment, err := auth.CreateEnrollment(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "One-time Dorf enrollment (expires %s):\n%s\n", enrollment.ExpiresAt.Format(time.RFC3339), enrollment.Token)
		return nil
	case "revoke":
		if len(args) != 2 {
			return fmt.Errorf("client revoke requires one Client ID")
		}
		if err := auth.Revoke(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Dorf Client revoked: %s\n", strings.TrimSpace(args[1]))
		return nil
	default:
		return fmt.Errorf("client requires: enroll or revoke CLIENT_ID")
	}
}

func serveCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	set.SetOutput(stderr)
	address := set.String("listen", defaultControlAddress, "private loopback HTTP listen address")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("serve does not accept positional arguments")
	}
	if err := requireLoopbackAddress(*address); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return fmt.Errorf("listen for Dorf control API: %w", err)
	}
	defer listener.Close()
	auth := controlauth.Service{Store: store}
	jobs := controlAPIJobs{store: store, tasks: tasks, gateway: gateway.Gateway{StatePath: cfg.GatewayStatePath}}
	server := controlapi.NewServer(controlapi.Discovery{
		Product: "dorf", Version: version.Version, Capabilities: []string{"direct_jobs"},
	}, auth, jobs)
	serverCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		<-serverCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		close(done)
	}()
	fmt.Fprintf(stdout, "Dorf control API listening privately on http://%s\n", listener.Addr())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}

func requireLoopbackAddress(address string) error {
	parsed, err := netip.ParseAddrPort(strings.TrimSpace(address))
	if err != nil || !parsed.Addr().IsLoopback() || parsed.Port() < 1024 {
		return fmt.Errorf("control API listen address must use an exact loopback IP and port 1024-65535")
	}
	return nil
}
