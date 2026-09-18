package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
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

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/clientconfig"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlclient"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/hostclientconfig"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/version"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const defaultControlAddress = "127.0.0.1:8745"

const maxControlModelBytes = 1024

func remoteCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "connect":
		return true, connectCommand(ctx, args[1:], os.Stdin, stdout, stderr)
	case "auth":
		return true, authCommand(ctx, args[1:], stdout, stderr)
	case "profile":
		if len(args) > 1 && args[1] == "list" {
			return true, remoteProfileList(ctx, args[2:], stdout, stderr)
		}
		return false, nil
	case "job":
		return true, remoteJobCommand(ctx, args[1:], stdout, stderr)
	case "sandbox", "run":
		cfg, _, client, err := loadConnectedClient()
		if err != nil {
			return true, err
		}
		switch args[0] {
		case "sandbox":
			err = remoteSandboxCommand(ctx, client, args[1:], stdout)
		case "run":
			err = remoteRun(ctx, client, cfg, args[1:], stdout, stderr)
		}
		return true, jobControlError(cfg.DeploymentURL, err)
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

func authCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "status" {
		return fmt.Errorf("auth requires: status")
	}
	set := flag.NewFlagSet("auth status", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("auth status does not accept positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	cfg, path, client, err := loadConnectedClient()
	if err != nil {
		return err
	}
	identity, err := client.Me(ctx)
	if err != nil {
		return jobControlError(cfg.DeploymentURL, err)
	}
	credentialSource := "client_config"
	if cfg.DeploymentURL == hostclientconfig.HostOrigin {
		credentialSource = "deployment_host"
	}
	if *output == "json" {
		return writeJSON(stdout, authStatusReceipt{
			Deployment: cfg.DeploymentURL, Principal: identity.Principal, Client: identity.Client,
			CredentialSource: credentialSource,
		})
	}
	return renderConnection(stdout, cfg.DeploymentURL, path, identity, true)
}

type authStatusReceipt struct {
	Deployment       string               `json:"deployment"`
	Principal        controlapi.Principal `json:"principal"`
	Client           controlapi.Client    `json:"client"`
	CredentialSource string               `json:"credential_source"`
}

func remoteRun(ctx context.Context, client *controlclient.Client, cfg clientconfig.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("run", flag.ContinueOnError)
	set.SetOutput(stderr)
	var attachmentPaths attachmentFlags
	clientReference := set.String("client-reference", cfg.ClientReference, "optional caller thread or task reference")
	key := set.String("key", "", "stable request identity for explicit replay")
	inputFile := set.String("input-file", "", "path containing the first Message")
	keepRunning := set.Bool("keep-running", false, "keep the Sandbox running between turns")
	agentsFile := set.String("agents-file", "", "path containing initial workspace AGENTS.md")
	connection := set.String("ai-connection", "", "named AI connection (default: deployment default)")
	model := set.String("model", "", "Harness model (default: selected AI connection)")
	effort := set.String("reasoning", "high", "Harness reasoning effort")
	profileName := set.String("profile", "", "named Sandbox profile (default: deployment default)")
	output := set.String("output", "human", "output format: human or json")
	set.Var(&attachmentPaths, "attach", "local file to attach to the first Message (repeatable)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("run does not accept positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	input, err := readMessageInput(*inputFile, "run", attachmentPaths)
	if err != nil {
		return err
	}
	requestKey, generated, err := directAdmissionKey(*key, rand.Reader)
	if err != nil {
		return err
	}
	agentsMD := ""
	if *agentsFile != "" {
		agentsMD, err = readInput(*agentsFile, "run", "AGENTS.md")
		if err != nil {
			return err
		}
	}
	request := controlapi.AdmitJobRequest{
		KeepRunning: *keepRunning, ClientReference: *clientReference,
		AgentsMD: agentsMD, Profile: strings.TrimSpace(*profileName), AIConnection: strings.TrimSpace(*connection), Model: strings.TrimSpace(*model), Reasoning: strings.TrimSpace(*effort),
	}
	job, err := runKeyedMutation(ctx, requestKey, generated, stderr, "Admission may have succeeded.", func() (controlapi.DirectJob, error) {
		return client.AdmitJob(ctx, requestKey, request)
	})
	if err != nil {
		return err
	}
	input.Intent = "follow"
	message, err := runKeyedMutation(ctx, requestKey, generated, stderr, "Message may have been accepted.", func() (controlapi.Message, error) {
		return client.SendMessage(ctx, job.ID, requestKey, input)
	})
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(stdout, remoteRunReceipt{Deployment: cfg.DeploymentURL, RequestID: requestKey, Job: job, Message: message})
	}
	fmt.Fprintf(stdout, "Job %s accepted by %s\n", job.ID, cfg.DeploymentURL)
	renderRemoteJob(stdout, job)
	renderRemoteMessage(stdout, message)
	fmt.Fprintf(stdout, "Next: dorf job inspect %s\n", job.ID)
	return nil
}

type remoteRunReceipt struct {
	Deployment string             `json:"deployment"`
	RequestID  string             `json:"request_id"`
	Job        controlapi.JobView `json:"job"`
	Message    controlapi.Message `json:"message"`
}

func remoteJobCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	cfg, _, client, err := loadConnectedClient()
	if err != nil {
		return err
	}
	defer func() { err = jobControlError(cfg.DeploymentURL, err) }()
	if len(args) == 0 {
		return fmt.Errorf("job requires: list, inspect, watch, message, retry, or cleanup")
	}
	switch args[0] {
	case "list":
		return remoteJobList(ctx, client, args[1:], stdout, stderr)
	case "inspect", "cleanup":
		return remoteJobSnapshot(ctx, cfg, client, args, stdout, stderr)
	case "watch":
		return remoteJobWatch(ctx, client, args[1:], stdout, stderr)
	case "message":
		if len(args) > 1 && args[1] == "interrupt" {
			return remoteMessageInterrupt(ctx, cfg, client, args[2:], stdout, stderr)
		}
		if len(args) > 1 && args[1] == "inspect" {
			return remoteMessageInspect(ctx, client, args[2:], stdout, stderr)
		}
		return remoteMessageSend(ctx, cfg, client, args[1:], stdout, stderr)
	case "retry":
		return remoteJobRetry(ctx, cfg, client, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("job requires: list, inspect, watch, message, retry, or cleanup")
	}
}

func remoteJobSnapshot(ctx context.Context, cfg clientconfig.Config, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
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
	var job controlapi.JobView
	var err error
	if args[0] == "inspect" {
		job, err = client.Job(ctx, set.Arg(0))
	} else {
		job, err = client.Cleanup(ctx, set.Arg(0))
	}
	if err != nil {
		return err
	}
	common := job.Common()
	if *output == "json" {
		if args[0] == "cleanup" {
			return writeJSON(stdout, remoteJobReceipt{Deployment: cfg.DeploymentURL, Job: job})
		}
		return writeJSON(stdout, job)
	}
	if args[0] == "cleanup" {
		fmt.Fprintf(stdout, "Cleanup requested for Job %s on %s\n", common.ID, cfg.DeploymentURL)
		renderRemoteJob(stdout, job)
		return nil
	}
	renderRemoteJobInspection(stdout, job)
	return nil
}

func remoteJobWatch(ctx context.Context, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("job watch", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or jsonl")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("job watch requires one Job ID")
	}
	if *output != "human" && *output != "jsonl" {
		return fmt.Errorf("output must be human or jsonl")
	}
	watchCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	encoder := json.NewEncoder(stdout)
	err := client.WatchJob(watchCtx, set.Arg(0), func(job controlapi.JobView) error {
		if *output == "jsonl" {
			return encoder.Encode(job)
		}
		fmt.Fprintf(stdout, "Job %s\n", job.Common().ID)
		renderRemoteJob(stdout, job)
		return nil
	})
	if errors.Is(err, context.Canceled) && watchCtx.Err() != nil {
		return nil
	}
	return err
}

func remoteMessageSend(ctx context.Context, cfg clientconfig.Config, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("job message", flag.ContinueOnError)
	set.SetOutput(stderr)
	var attachmentPaths attachmentFlags
	key := set.String("key", "", "stable request identity for explicit replay")
	inputFile := set.String("input-file", "", "path containing the complete Message")
	refreshSkills := set.Bool("refresh-skills", false, "reload installed skills before the next fresh Turn (Codex)")
	intent := set.String("intent", "auto", "delivery intent: auto (steer active work, follow when idle), follow, or steer")
	output := set.String("output", "human", "output format: human or json")
	set.Var(&attachmentPaths, "attach", "local file to attach to the Message (repeatable)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("job message requires one Job ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *intent != "auto" && *intent != "follow" && *intent != "steer" {
		return fmt.Errorf("message intent must be auto, follow, or steer")
	}
	input, err := readMessageInput(*inputFile, "job message", attachmentPaths)
	if err != nil {
		return err
	}
	requestKey, generated, err := operationKey("message", *key, rand.Reader)
	if err != nil {
		return err
	}
	input.Intent, input.RefreshSkills = *intent, *refreshSkills
	message, err := runKeyedMutation(ctx, requestKey, generated, stderr, "Message may have been accepted.", func() (controlapi.Message, error) {
		return client.SendMessage(ctx, set.Arg(0), requestKey, input)
	})
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(stdout, remoteMessageReceipt{Deployment: cfg.DeploymentURL, RequestID: requestKey, Message: message})
	}
	fmt.Fprintf(stdout, "Message %s accepted for Job %s\n", message.ID, message.JobID)
	renderRemoteMessage(stdout, message)
	fmt.Fprintf(stdout, "Next: dorf job message inspect %s %s\n", message.JobID, message.ID)
	return nil
}

func remoteMessageInspect(ctx context.Context, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("job message inspect", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 || set.NArg() > 2 {
		return fmt.Errorf("job message inspect requires one Job ID and an optional Message ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	messageID := "latest"
	if set.NArg() == 2 {
		messageID = set.Arg(1)
	}
	message, err := client.Message(ctx, set.Arg(0), messageID)
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(stdout, message)
	}
	fmt.Fprintf(stdout, "Message %s for Job %s\n", message.ID, message.JobID)
	renderRemoteMessage(stdout, message)
	return nil
}

func remoteJobRetry(ctx context.Context, cfg clientconfig.Config, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("job retry", flag.ContinueOnError)
	set.SetOutput(stderr)
	key := set.String("key", "", "stable request identity for explicit replay")
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("job retry requires one Job ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	requestKey, generated, err := operationKey("retry", *key, rand.Reader)
	if err != nil {
		return err
	}
	retry, err := runKeyedMutation(ctx, requestKey, generated, stderr, "Retry may have been accepted.", func() (controlapi.Retry, error) {
		return client.Retry(ctx, set.Arg(0), requestKey)
	})
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(stdout, remoteRetryReceipt{Deployment: cfg.DeploymentURL, RequestID: requestKey, Retry: retry})
	}
	fmt.Fprintf(stdout, "Retry %s for Job %s\n", retry.State, retry.JobID)
	return nil
}

func remoteSandboxCommand(ctx context.Context, client *controlclient.Client, args []string, stdout io.Writer) error {
	if len(args) < 2 || args[0] != "file" || args[1] != "get" {
		return fmt.Errorf("sandbox requires: file get SANDBOX_ID PATH --output DESTINATION")
	}
	sandboxID, relativePath, output, err := parseSandboxFileGet(args[2:])
	if err != nil {
		return err
	}
	return downloadSandboxFile(ctx, sandboxID, relativePath, output, stdout, func(ctx context.Context, path string) ([]byte, error) {
		return client.SandboxFile(ctx, sandboxID, path)
	})
}

func renderRemoteMessage(output io.Writer, message controlapi.Message) {
	fmt.Fprintf(output, "  sequence: %d\n  intent: %s\n  delivery: %s\n  admitted: %s\n",
		message.Sequence, message.Intent, humanMessageState(message), message.AdmittedAt.Format(time.RFC3339))
	if message.InterruptRequested {
		fmt.Fprintln(output, "  interrupt: requested")
	}
	if message.Result != nil {
		fmt.Fprintf(output, "  outcome: %s\n", message.Result.Outcome)
		if message.Result.Output != "" {
			fmt.Fprintf(output, "  output: %q\n", message.Result.Output)
		}
	}
	if message.Attention != nil {
		fmt.Fprintf(output, "  attention: %s\n", message.Attention.Detail)
	}
}

func humanMessageState(message controlapi.Message) string {
	if message.Attention != nil {
		return "Needs attention"
	}
	if message.Result != nil {
		if message.Result.Outcome == "completed" {
			return "Finished"
		}
		return "Needs attention"
	}
	switch message.Delivery.State {
	case "accepted":
		return "Queued"
	case "running":
		return "Working"
	case "completed":
		return "Delivered; awaiting result"
	case "failed":
		return "Needs attention"
	default:
		return message.Delivery.State
	}
}

func humanJobState(job controlapi.Job) string {
	if job.Attention != nil {
		return "Needs attention"
	}
	switch job.Execution.State {
	case "provisioning_sandbox":
		return "Starting"
	case "connecting_model_access":
		return "Connecting"
	case "awaiting_agent":
		return "Queued"
	case "running":
		return "Working"
	case "idle":
		return "Idle"
	case "complete":
		return "Finished"
	case "stopped":
		return "Stopped"
	case "failed":
		return "Needs attention"
	default:
		return job.Execution.State
	}
}

type remoteMessageReceipt struct {
	Deployment string             `json:"deployment"`
	RequestID  string             `json:"request_id"`
	Message    controlapi.Message `json:"message"`
}

type remoteRetryReceipt struct {
	Deployment string           `json:"deployment"`
	RequestID  string           `json:"request_id"`
	Retry      controlapi.Retry `json:"retry"`
}

type remoteJobReceipt struct {
	Deployment string             `json:"deployment"`
	RequestID  string             `json:"request_id,omitempty"`
	Job        controlapi.JobView `json:"job"`
}

func renderRemoteJob(output io.Writer, view controlapi.JobView) {
	job := view.Common()
	renderJobAttribution(output, job.CreatedByClient, job.ClientReference)
	fmt.Fprintf(output, "  profile: %s\n  model: %q (%s)\n  admission: %s\n  execution: %s\n  cleanup: %s\n",
		job.Profile, job.Model, job.Reasoning, openClosed(job.Admission.Open), humanJobState(job), job.Cleanup.State)
	if job.Attention != nil {
		fmt.Fprintf(output, "  attention: %s\n", job.Attention.Detail)
	}
	for _, sandbox := range job.Sandboxes {
		fmt.Fprintf(output, "  Sandbox: %s (%s)\n", sandbox.ID, sandbox.Name)
	}
}

func renderRemoteJobInspection(output io.Writer, view controlapi.JobView) {
	job := view.Common()
	fmt.Fprintf(output, "Job %s\n", job.ID)
	renderRemoteJob(output, view)
}

func renderConnection(output io.Writer, deploymentURL, path string, identity controlapi.Identity, existing bool) error {
	state := "connected"
	if existing {
		state = "authenticated"
	}
	fmt.Fprintf(output, "Dorf %s\n  Deployment: %s\n  Principal: %s\n  Client: %s (%s)\n  Credential expires: %s\n  Client configuration: %s\n",
		state, deploymentURL, identity.Principal.Name, identity.Client.Name, identity.Client.ID,
		formatClientExpiry(identity.Client.ExpiresAt), path)
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
	if found {
		client, err := controlclient.New(cfg.DeploymentURL, cfg.Credential, nil)
		return cfg, path, client, err
	}
	paths, err := config.CurrentOperatorPaths()
	if err != nil {
		return clientconfig.Config{}, path, nil, err
	}
	hostPath := hostclientconfig.Path(paths.StateDir)
	host, hostFound, err := hostclientconfig.Load(hostPath)
	if err != nil {
		return clientconfig.Config{}, hostPath, nil, err
	}
	if !hostFound {
		return clientconfig.Config{}, hostPath, nil, fmt.Errorf("Dorf Job control is not configured; run dorf setup on a deployment host or dorf connect HTTPS_URL on a remote client")
	}
	client, err := controlclient.NewLoopback(host.Credential)
	return clientconfig.Config{DeploymentURL: hostclientconfig.HostOrigin, Credential: host.Credential}, hostPath, client, err
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

func jobControlError(deploymentURL string, err error) error {
	if problemCode(err, "profile_not_found") {
		return fmt.Errorf("Sandbox profile not found; run dorf profile list to choose an available profile: %w", err)
	}
	if err == nil || deploymentURL != hostclientconfig.HostOrigin {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !controlclient.IsServiceError(err) {
		return err
	}
	if problemCode(err, "unauthenticated") {
		return fmt.Errorf("deployment-host Client is invalid, expired, or revoked; rerun dorf setup: %w", err)
	}
	if definitiveClientError(err) {
		return err
	}
	return fmt.Errorf("deployment-host control API is unavailable; run dorf doctor, inspect the Compose control-api service, then rerun dorf setup: %w", err)
}

func definitiveClientError(err error) bool {
	var problem *controlclient.ProblemError
	return errors.As(err, &problem) && problem.Problem.Status >= 400 && problem.Problem.Status < 500
}

func runKeyedMutation[T any](ctx context.Context, key string, generated bool, output io.Writer, ambiguity string, mutate func() (T, error)) (T, error) {
	result, err := mutate()
	uncertain := ambiguousMutationError(err)
	if retryableMutationError(ctx, err) {
		result, err = mutate()
	}
	if err != nil && generated && (uncertain || ambiguousMutationError(err)) {
		fmt.Fprintf(output, "%s Retry the same request with --key %s.\n", ambiguity, key)
	}
	return result, err
}

func retryableMutationError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var problem *controlclient.ProblemError
	if !errors.As(err, &problem) {
		return true
	}
	return problem.Problem.Status >= 500 && problem.Problem.Status < 600
}

func ambiguousMutationError(err error) bool {
	return err != nil && !definitiveClientError(err)
}

type controlAPIJobs struct {
	store            postgres.Store
	tasks            *absurd.Client
	directAdmissions direct.AdmissionService
	reader           controlReader
	blobs            blob.Store
	messageImages    messageImageCapability
}

type controlReader interface {
	ReadSandboxStatus(context.Context, string) (provider.Status, error)
	Exec(context.Context, string, provider.Command) (provider.CommandResult, error)
	ReadFile(context.Context, string, string) ([]byte, error)
	WriteFile(context.Context, string, string, []byte, bool) error
	ObserveMessage(context.Context, string, string) (core.MessageResult, error)
	DefaultConnection() (string, error)
	DefaultModel(string) (string, error)
	Check(context.Context, string) error
}

func (a controlAPIJobs) application() core.Application {
	return coreApplication(a.store, a.tasks)
}

type controlJobKind string

const (
	controlDirectJob controlJobKind = controlapi.JobKindDirect
)

type supportedControlJob struct {
	core.Job
	kind controlJobKind
}

func classifyControlJob(workflow core.WorkflowName, revision string) (controlJobKind, bool) {
	switch {
	case workflow == "" && revision == "":
		return controlDirectJob, true
	default:
		return "", false
	}
}

func (a controlAPIJobs) AdmitDirect(ctx context.Context, clientID, key string, input controlapi.AdmitJobRequest) (controlapi.DirectJob, bool, error) {
	admission, err := newControlJobAdmission(key, input.AgentsMD, input.Profile, input.AIConnection, input.Model, input.Reasoning)
	if err != nil {
		return controlapi.DirectJob{}, false, err
	}
	admission.CreatedByClientID = clientID
	admission.ClientReference = input.ClientReference
	admission.KeepRunning = input.KeepRunning
	job, created, err := a.directAdmissions.Admit(ctx, admission)
	if errors.Is(err, direct.ErrAdmissionConflict) {
		return controlapi.DirectJob{}, false, controlapi.ErrIdempotencyConflict
	}
	if errors.Is(err, postgres.ErrProfileNotFound) {
		return controlapi.DirectJob{}, false, controlapi.ErrProfileNotFound
	}
	if errors.Is(err, direct.ErrInvalidAdmission) {
		return controlapi.DirectJob{}, false, fmt.Errorf("%w: %v", controlapi.ErrInvalidInput, err)
	}
	if err != nil {
		return controlapi.DirectJob{}, false, err
	}
	view, err := a.projectDirect(ctx, job)
	return view, created, err
}

func validControlAdmissionKey(key string) bool {
	return key != "" && key == strings.TrimSpace(key) && len(key) <= 255 && !strings.ContainsRune(key, 0)
}

func newControlJobAdmission(key, agentsMD, profile, connection, model, reasoning string) (direct.AdmissionRequest, error) {
	profile = strings.TrimSpace(profile)
	connection = strings.TrimSpace(connection)
	model = strings.TrimSpace(model)
	reasoning = strings.TrimSpace(reasoning)
	if !validControlAdmissionKey(key) || invalidOptionalControlText(agentsMD, 1<<20) ||
		invalidOptionalControlText(profile, 255) || invalidOptionalControlText(connection, 255) || invalidOptionalControlText(model, maxControlModelBytes) {
		return direct.AdmissionRequest{}, controlapi.ErrInvalidInput
	}
	if reasoning == "" {
		reasoning = "high"
	}
	if !validControlReasoning(reasoning) {
		return direct.AdmissionRequest{}, controlapi.ErrInvalidInput
	}
	return direct.AdmissionRequest{
		AdmissionKey: key, AgentsMD: agentsMD, SandboxProfile: profile, ProviderConnection: connection, Model: model, ReasoningEffort: reasoning,
	}, nil
}

func invalidControlText(value string, limit int) bool {
	return value == "" || len(value) > limit || strings.ContainsRune(value, 0)
}

func invalidControlPrompt(value string, limit int) bool {
	return strings.TrimSpace(value) == "" || len(value) > limit || strings.ContainsRune(value, 0)
}

func invalidOptionalControlText(value string, limit int) bool {
	return len(value) > limit || strings.ContainsRune(value, 0)
}

func validControlReasoning(reasoning string) bool {
	return reasoning == "low" || reasoning == "medium" || reasoning == "high" || reasoning == "xhigh"
}

func (a controlAPIJobs) Get(ctx context.Context, jobID string) (controlapi.JobView, error) {
	job, err := a.supportedJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	view, err := a.project(ctx, job)
	if err != nil {
		return nil, err
	}
	deliveries, err := a.store.Deliveries(ctx, jobID)
	if err != nil {
		return nil, err
	}
	switch value := view.(type) {
	case controlapi.DirectJob:
		value.LatestReplyID = latestReplyID(jobID, deliveries)
		return value, nil
	default:
		return nil, controlapi.ErrJobNotFound
	}
}

func latestReplyID(jobID string, deliveries []core.Delivery) string {
	var id string
	var sequence int64
	for _, delivery := range deliveries {
		run, message := delivery.AgentRun, delivery.Message
		if run.SandboxID != core.MainSandboxName(jobID) || message.Sequence <= sequence {
			continue
		}
		if run.State == core.AgentRunCompleted && run.TurnOutcome != "" || run.State == core.AgentRunFailed || run.State == core.AgentRunInterrupted {
			id, sequence = message.ID, message.Sequence
		}
	}
	return id
}

func (a controlAPIJobs) SendMessage(ctx context.Context, jobID, key string, input controlapi.SendMessageRequest) (controlapi.Message, bool, error) {
	job, err := a.supportedJob(ctx, jobID)
	if err != nil {
		return controlapi.Message{}, false, err
	}
	var options []core.MessageOption
	switch input.Intent {
	case "", string(core.MessageAuto):
		input.Intent = string(core.MessageAuto)
		options = append(options, core.PreferSteer())
	case string(core.MessageFollow):
	case string(core.MessageSteer):
		options = append(options, core.Steer())
	default:
		return controlapi.Message{}, false, controlapi.ErrInvalidInput
	}
	if !core.ValidObservationDelivery(input.Observation, core.MessageDeliveryIntent(input.Intent), len(input.Attachments)) ||
		(len(input.Attachments) == 0 && strings.TrimSpace(input.Text) == "") ||
		!core.ValidDeveloperInstructions(&input.Text) || !core.ValidDeveloperInstructions(input.DeveloperInstructions) {
		return controlapi.Message{}, false, controlapi.ErrInvalidInput
	}
	attachments, err := a.retainMessageAttachments(ctx, job.ProfileRef(), input.Attachments)
	if err != nil {
		return controlapi.Message{}, false, err
	}
	if input.RefreshSkills {
		options = append(options, core.RefreshSkills())
	}
	handle, err := a.application().OpenJob(ctx, job.ID)
	if err != nil {
		return controlapi.Message{}, false, err
	}
	sandbox, err := handle.DefaultSandbox(ctx)
	if err != nil {
		return controlapi.Message{}, false, err
	}
	receipt, err := sandbox.Agent().Message(ctx, key, core.MessageInput{Text: input.Text, Attachments: attachments, Observation: input.Observation, DeveloperInstructions: input.DeveloperInstructions}, options...)
	if err != nil {
		return controlapi.Message{}, receipt.Created, controlMessageError(err)
	}
	message, err := a.GetMessage(ctx, job.ID, receipt.MessageID)
	return message, receipt.Created, err
}

func (a controlAPIJobs) GetMessage(ctx context.Context, jobID, messageID string) (controlapi.Message, error) {
	job, err := a.supportedJob(ctx, jobID)
	if err != nil {
		return controlapi.Message{}, err
	}
	deliveries, err := a.store.Deliveries(ctx, job.ID)
	if err != nil {
		return controlapi.Message{}, err
	}
	if messageID == "latest" {
		messageID = latestReplyID(job.ID, deliveries)
	}
	index := slices.IndexFunc(deliveries, func(delivery core.Delivery) bool { return delivery.Message.ID == messageID })
	if index < 0 {
		return controlapi.Message{}, controlapi.ErrMessageNotFound
	}
	delivery := deliveries[index]
	message, run := delivery.Message, delivery.AgentRun
	waitReason, err := a.messageWaitReason(ctx, delivery)
	if err != nil {
		return controlapi.Message{}, err
	}
	deliveryState, err := publicMessageDeliveryState(run.State)
	if err != nil {
		return controlapi.Message{}, err
	}
	result, err := a.messageResult(ctx, job.Job, delivery)
	if err != nil {
		return controlapi.Message{}, err
	}
	attention := (*controlapi.Attention)(nil)
	if run.Attention != "" || run.State == core.AgentRunUncertain {
		attention = &controlapi.Attention{Code: "agent_delivery_attention", Detail: "Message delivery needs operator attention; inspect the deployment service logs."}
	}
	return controlapi.Message{
		ID: message.ID, JobID: job.ID, Sequence: message.Sequence, Intent: string(message.Intent),
		InterruptRequested: run.InterruptRequested,
		WaitReason:         waitReason,
		Delivery:           controlapi.State{State: deliveryState}, Result: result, Attention: attention, AdmittedAt: message.AdmittedAt,
	}, nil
}

func (a controlAPIJobs) messageResult(ctx context.Context, job core.Job, delivery core.Delivery) (*controlapi.MessageResult, error) {
	message, run := delivery.Message, delivery.AgentRun
	result := (*controlapi.MessageResult)(nil)
	if job.CleanupState == core.CleanupPending {
		switch run.State {
		case core.AgentRunCompleted:
			if run.TurnOutcome == "" {
				break
			}
			if a.reader == nil {
				return nil, fmt.Errorf("control reader is not configured")
			}
			observed, observeErr := a.reader.ObserveMessage(ctx, job.ID, message.ID)
			if observeErr != nil {
				if errors.Is(observeErr, controlreader.ErrUnavailable) || errors.Is(observeErr, controlreader.ErrResponseTooLarge) {
					return nil, controlapi.ErrMessageUnavailable
				}
				return nil, observeErr
			}
			result = &controlapi.MessageResult{Outcome: observed.Outcome, Output: observed.Output}
		case core.AgentRunFailed, core.AgentRunInterrupted:
			outcome := run.TurnOutcome
			if outcome == "" {
				outcome = string(run.State)
			}
			result = &controlapi.MessageResult{Outcome: outcome}
		}
	}
	return result, nil
}

func (a controlAPIJobs) InterruptMessage(ctx context.Context, jobID, messageID string) (controlapi.Message, error) {
	job, err := a.supportedJob(ctx, jobID)
	if err != nil {
		return controlapi.Message{}, err
	}
	if job.Workflow != "" {
		return controlapi.Message{}, controlapi.ErrInterruptUnavailable
	}
	execution, err := a.store.AgentMessageExecution(ctx, messageID)
	if errors.Is(err, postgres.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return controlapi.Message{}, controlapi.ErrMessageNotFound
	}
	if err != nil {
		return controlapi.Message{}, err
	}
	if execution.Job.ID != job.ID {
		return controlapi.Message{}, controlapi.ErrMessageNotFound
	}
	if execution.AgentRun.Harness != codex.Harness {
		return controlapi.Message{}, controlapi.ErrInterruptUnavailable
	}
	if _, err := a.application().RequestMessageInterrupt(ctx, job.ID, messageID); err != nil {
		if errors.Is(err, core.ErrMessageInterruptUnavailable) || errors.Is(err, core.ErrMessageAdmissionClosed) {
			return controlapi.Message{}, controlapi.ErrInterruptUnavailable
		}
		return controlapi.Message{}, err
	}
	return a.GetMessage(ctx, job.ID, messageID)
}

func remoteMessageInterrupt(ctx context.Context, cfg clientconfig.Config, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("job message interrupt", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 2 {
		return fmt.Errorf("job message interrupt requires one Job ID and Message ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	message, err := client.InterruptMessage(ctx, set.Arg(0), set.Arg(1))
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(stdout, remoteMessageReceipt{Deployment: cfg.DeploymentURL, Message: message})
	}
	renderRemoteMessage(stdout, message)
	return nil
}

func publicMessageDeliveryState(state core.AgentRunState) (string, error) {
	switch state {
	case core.AgentRunPending, core.AgentRunSubmitting:
		return "accepted", nil
	case core.AgentRunActive, core.AgentRunUncertain:
		return "running", nil
	case core.AgentRunCompleted:
		return "completed", nil
	case core.AgentRunFailed, core.AgentRunInterrupted:
		return "failed", nil
	default:
		return "", fmt.Errorf("unknown Agent delivery state %q", state)
	}
}

func (a controlAPIJobs) Retry(ctx context.Context, jobID, key string) (controlapi.Retry, bool, error) {
	job, err := a.supportedJob(ctx, jobID)
	if err != nil {
		return controlapi.Retry{}, false, err
	}
	receipt, err := a.application().RetryFailedJob(ctx, job.ID, key)
	if err != nil {
		return controlapi.Retry{}, false, controlRetryError(err)
	}
	return controlapi.Retry{JobID: receipt.JobID, State: receipt.Retry}, receipt.Created, nil
}

func (a controlAPIJobs) ReadSandboxFile(ctx context.Context, sandboxID, relativePath string) ([]byte, error) {
	owned, err := a.store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return nil, controlapi.ErrSandboxNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := a.supportedJob(ctx, owned.JobID); err != nil {
		return nil, err
	}
	if a.reader == nil {
		return nil, fmt.Errorf("control reader is not configured")
	}
	contents, err := a.reader.ReadFile(ctx, owned.ID, relativePath)
	switch {
	case errors.Is(err, controlreader.ErrUnavailable):
		return nil, controlapi.ErrFileUnavailable
	case errors.Is(err, controlreader.ErrFileTooLarge):
		return nil, controlapi.ErrFileTooLarge
	case errors.Is(err, controlreader.ErrInvalidFilePath):
		return nil, controlapi.ErrInvalidFilePath
	case errors.Is(err, controlreader.ErrFileNotFound):
		return nil, controlapi.ErrFileNotFound
	case errors.Is(err, controlreader.ErrSandboxNotFound):
		return nil, controlapi.ErrSandboxNotFound
	case err != nil:
		return nil, err
	default:
		return contents, nil
	}
}

func (a controlAPIJobs) WriteSandboxFile(ctx context.Context, sandboxID, relativePath string, contents []byte, ifAbsent bool) error {
	owned, err := a.store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return controlapi.ErrSandboxNotFound
	}
	if err != nil {
		return err
	}
	if _, err := a.supportedJob(ctx, owned.JobID); err != nil {
		return err
	}
	if a.reader == nil {
		return fmt.Errorf("control reader is not configured")
	}
	err = a.reader.WriteFile(ctx, owned.ID, relativePath, contents, ifAbsent)
	switch {
	case errors.Is(err, controlreader.ErrUnavailable):
		return controlapi.ErrFileUnavailable
	case errors.Is(err, controlreader.ErrInvalidFilePath):
		return controlapi.ErrInvalidFilePath
	case errors.Is(err, controlreader.ErrFileNotFound):
		return controlapi.ErrFileNotFound
	case errors.Is(err, controlreader.ErrSandboxNotFound):
		return controlapi.ErrSandboxNotFound
	case err != nil:
		return err
	default:
		return nil
	}
}

func controlMessageError(err error) error {
	switch {
	case errors.Is(err, core.ErrMessageAdmissionClosed):
		return controlapi.ErrMessageUnavailable
	case errors.Is(err, core.ErrMessageSteerUnavailable):
		return controlapi.ErrSteerUnavailable
	case errors.Is(err, core.ErrMessageReplayConflict):
		return controlapi.ErrIdempotencyConflict
	default:
		return err
	}
}

func controlRetryError(err error) error {
	switch {
	case errors.Is(err, core.ErrRetryReplayConflict):
		return controlapi.ErrIdempotencyConflict
	case errors.Is(err, core.ErrRetryNotEligible):
		return controlapi.ErrRetryUnavailable
	default:
		return err
	}
}

func (a controlAPIJobs) RequestCleanup(ctx context.Context, jobID string) (controlapi.JobView, error) {
	job, err := a.supportedJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	handle, err := a.application().OpenJob(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	if err := handle.RequestCleanup(ctx); err != nil {
		return nil, err
	}
	return a.Get(ctx, job.ID)
}

func (a controlAPIJobs) supportedJob(ctx context.Context, jobID string) (supportedControlJob, error) {
	job, err := a.store.Job(ctx, jobID)
	if errors.Is(err, postgres.ErrNotFound) {
		return supportedControlJob{}, controlapi.ErrJobNotFound
	}
	if err != nil {
		return supportedControlJob{}, err
	}
	kind, ok := classifyControlJob(job.Workflow, job.WorkflowRevision)
	if !ok {
		return supportedControlJob{}, controlapi.ErrJobNotFound
	}
	return supportedControlJob{Job: job, kind: kind}, nil
}

func (a controlAPIJobs) project(ctx context.Context, job supportedControlJob) (controlapi.JobView, error) {
	switch job.kind {
	case controlDirectJob:
		return a.projectDirect(ctx, job.Job)
	default:
		return nil, controlapi.ErrJobNotFound
	}
}

func (a controlAPIJobs) projectDirect(ctx context.Context, job core.Job) (controlapi.DirectJob, error) {
	snapshot, err := direct.LoadSnapshot(ctx, a.store, job)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return controlapi.DirectJob{}, controlapi.ErrJobNotFound
		}
		return controlapi.DirectJob{}, err
	}
	projection := snapshot.Project()
	task, err := fetchTaskResult(ctx, a.tasks, job.CurrentTaskID)
	if err != nil {
		return controlapi.DirectJob{}, err
	}
	executionState := map[direct.ExecutionState]string{
		direct.ExecutionProvisioningSandbox: "provisioning_sandbox",
		direct.ExecutionConnectingRoute:     "connecting_model_access",
		direct.ExecutionQueued:              "awaiting_agent",
		direct.ExecutionWorking:             "running",
		direct.ExecutionAttention:           "stopped",
		direct.ExecutionIdle:                "idle",
	}[projection.State]
	if executionState == "" {
		return controlapi.DirectJob{}, fmt.Errorf("Job %s has unknown direct execution state %q", job.ID, projection.State)
	}
	var attention *controlapi.Attention
	if projection.State == direct.ExecutionAttention {
		code := "agent_attention"
		if job.WorkflowAttention != "" {
			code = "job_attention"
		}
		attention = &controlapi.Attention{Code: code, Detail: "Job execution needs operator attention; inspect the deployment service logs."}
	}
	common, err := a.projectCommonJob(ctx, job, controlapi.JobKindDirect, executionState, attention, task, snapshot.Sandboxes)
	return controlapi.DirectJob{Job: common}, err
}

func publicJobCreator(id, name string) *controlapi.JobCreator {
	if id == "" {
		return nil
	}
	return &controlapi.JobCreator{ID: id, Name: name}
}

func publicCommonJob(job core.Job, kind, executionState string, attention *controlapi.Attention, task taskResultView, owned []core.Sandbox) (controlapi.Job, error) {
	if executionState == "" {
		return controlapi.Job{}, fmt.Errorf("Job %s has an incomplete public projection", job.ID)
	}
	cleanupState := map[core.CleanupState]string{
		core.CleanupPending: "not_requested", core.CleanupRequested: "requested",
		core.CleanupScheduled: "running", core.CleanupComplete: "complete",
	}[job.CleanupState]
	if cleanupState == "" {
		return controlapi.Job{}, fmt.Errorf("Job %s has unknown cleanup state %q", job.ID, job.CleanupState)
	}
	if (job.CleanupState == core.CleanupPending && failedExecutionTask(task.State)) ||
		(job.CleanupState == core.CleanupRequested && task.State == absurd.TaskFailed) {
		executionState = "failed"
		attention = publicExecutionFailure(task)
	}
	if job.CleanupState != core.CleanupPending {
		if executionState == "provisioning_sandbox" || executionState == "connecting_model_access" || executionState == "awaiting_agent" || executionState == "running" {
			executionState = "stopped"
		}
		if job.CleanupState == core.CleanupScheduled && failedExecutionTask(task.State) {
			cleanupState = "failed"
			attention = &controlapi.Attention{Code: "cleanup_failed", Detail: "Cleanup stopped before all resources were released; inspect the deployment service logs."}
		}
	}
	sandboxes := make([]controlapi.Sandbox, 0, len(owned))
	for _, sandbox := range owned {
		if sandbox.ID == "" || sandbox.JobID != job.ID {
			return controlapi.Job{}, fmt.Errorf("Job %s has a mismatched Sandbox projection", job.ID)
		}
		sandboxes = append(sandboxes, controlapi.Sandbox{ID: sandbox.ID, Name: sandbox.Name, ResourceID: sandbox.ResourceID, ProviderID: sandbox.ProviderID})
	}
	return controlapi.Job{
		CreatedByClient: publicJobCreator(job.CreatedByClientID, job.CreatedByClientName), ClientReference: job.ClientReference,
		ID: job.ID, Kind: kind, Profile: job.SandboxProfile,
		KeepRunning: job.KeepRunning, Model: job.Model, Reasoning: job.ReasoningEffort,
		Admission: controlapi.Admission{Open: job.AdmissionOpen}, Execution: controlapi.State{State: executionState},
		Attention: attention, Cleanup: controlapi.State{State: cleanupState}, Sandboxes: sandboxes,
	}, nil
}

func publicExecutionFailure(task taskResultView) *controlapi.Attention {
	var failure struct {
		Message string `json:"message"`
	}
	if task.State == absurd.TaskFailed && json.Unmarshal(task.failure, &failure) == nil {
		_, creation, found := strings.Cut(failure.Message, "create Incus instance ")
		_, detail, _ := strings.Cut(creation, ": ")
		if found && (strings.HasPrefix(detail, "Reached maximum number of instances in project ") ||
			strings.HasPrefix(detail, `Reached maximum number of instances of type "virtual-machine" in project `)) {
			return &controlapi.Attention{Code: "sandbox_capacity_exhausted", Detail: "Sandbox creation failed because the VM limit was reached. Free capacity or increase the limit, then retry."}
		}
	}
	return &controlapi.Attention{Code: "execution_failed", Detail: "Job execution stopped; inspect the deployment service logs, repair the cause, then retry."}
}

func failedExecutionTask(state absurd.TaskResultState) bool {
	return state == "" || state == "missing" || state == absurd.TaskFailed || state == absurd.TaskCancelled
}

func serveCommand(ctx context.Context, store postgres.Store, tasks *absurd.Client, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	set.SetOutput(stderr)
	address := set.String("listen", defaultControlAddress, "private loopback HTTP listen address")
	allowContainerListen := set.Bool("allow-container-listen", false, "allow 0.0.0.0 for container port publishing")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("serve does not accept positional arguments")
	}
	listenAddress, err := controlListenAddress(*address, *allowContainerListen)
	if err != nil {
		return err
	}
	network := "tcp6"
	if listenAddress.Addr().Is4() {
		network = "tcp4"
	}
	listener, err := net.Listen(network, listenAddress.String())
	if err != nil {
		return fmt.Errorf("listen for Dorf control API: %w", err)
	}
	defer listener.Close()
	runtimes := profileRuntimeResolver{cfg: cfg, store: store, client: tasks}
	reader, err := configuredControlReader(cfg, store, runtimes)
	if err != nil {
		return err
	}
	auth := controlauth.Service{Store: store}
	jobs := controlAPIJobs{
		store: store, tasks: tasks,
		directAdmissions: direct.NewAdmissionService(store, config.QueueName, reader),
		reader:           reader, blobs: blob.Store{Root: cfg.BlobRoot}, messageImages: runtimes,
	}
	server := controlapi.NewServer(controlapi.Discovery{
		Product: "dorf", Version: version.Version,
		Capabilities: []string{"direct_jobs", "job_list", "profile_list", "job_watch", "job_timeline", "messages", "message_interrupt", "job_retry", "sandbox_files", "sandbox_exec", "sandbox_status", "latest_reply"},
	}, auth, jobs, controlAPIProfiles{store: store})
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
	if *allowContainerListen && listenAddress.Addr().Is4() && listenAddress.Addr().IsUnspecified() {
		fmt.Fprintf(stdout, "Dorf control API listening inside its container on http://%s\n", listener.Addr())
	} else {
		fmt.Fprintf(stdout, "Dorf control API listening privately on http://%s\n", listener.Addr())
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}

func configuredControlReader(cfg config.Config, store postgres.Store, runtimes core.SandboxRuntimeResolver) (controlReader, error) {
	origin := strings.TrimSpace(os.Getenv("DORF_CONTROL_READER_ORIGIN"))
	token := strings.TrimSpace(os.Getenv("DORF_CONTROL_READER_TOKEN"))
	if origin == "" && token == "" {
		// Explicitly manually supervised local `dorf serve` remains useful in
		// development. Compose always supplies the isolated HTTP capability.
		return controlreader.Service{
			Store: store, Runtimes: runtimes,
			Provider: configuredProviderGateway(cfg),
		}, nil
	}
	if origin == "" || token == "" {
		return nil, fmt.Errorf("control reader requires both DORF_CONTROL_READER_ORIGIN and DORF_CONTROL_READER_TOKEN")
	}
	client, err := controlreader.NewClient(origin, token, nil)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func controlListenAddress(address string, allowContainerListen bool) (netip.AddrPort, error) {
	parsed, err := netip.ParseAddrPort(strings.TrimSpace(address))
	if err != nil || parsed.Port() < 1024 {
		return netip.AddrPort{}, fmt.Errorf("control API listen address must use an exact IP and port 1024-65535")
	}
	if parsed.Addr().IsLoopback() {
		return parsed, nil
	}
	if allowContainerListen && parsed.Addr().Is4() && parsed.Addr().IsUnspecified() {
		return parsed, nil
	}
	return netip.AddrPort{}, fmt.Errorf("control API listen address must use an exact loopback IP; only --allow-container-listen permits 0.0.0.0 for container port publishing")
}

func renderJobAttribution(output io.Writer, creator *controlapi.JobCreator, reference string) {
	if creator == nil {
		fmt.Fprintln(output, "  created by: unknown")
	} else {
		fmt.Fprintf(output, "  created by: %s (%s)\n", creator.Name, creator.ID)
	}
	if reference != "" {
		fmt.Fprintf(output, "  client reference: %q\n", reference)
	}
}

func (a controlAPIJobs) ExecSandbox(ctx context.Context, sandboxID string, command provider.Command) (provider.CommandResult, error) {
	owned, err := a.store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return provider.CommandResult{}, controlapi.ErrSandboxNotFound
	}
	if err != nil {
		return provider.CommandResult{}, err
	}
	if _, err := a.supportedJob(ctx, owned.JobID); err != nil {
		return provider.CommandResult{}, err
	}
	if a.reader == nil {
		return provider.CommandResult{}, controlapi.ErrSandboxExecUnavailable
	}
	result, err := a.reader.Exec(ctx, sandboxID, command)
	switch {
	case errors.Is(err, controlreader.ErrInvalidRequest):
		return provider.CommandResult{}, controlapi.ErrInvalidInput
	case errors.Is(err, controlreader.ErrSandboxNotFound):
		return provider.CommandResult{}, controlapi.ErrSandboxNotFound
	case errors.Is(err, controlreader.ErrUnavailable):
		return provider.CommandResult{}, controlapi.ErrSandboxExecUnavailable
	case err != nil:
		return provider.CommandResult{}, controlapi.ErrSandboxExecFailed
	default:
		return result, nil
	}
}

func (a controlAPIJobs) ReadSandboxStatus(ctx context.Context, sandboxID string) (provider.Status, error) {
	owned, err := a.store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return provider.Status{}, controlapi.ErrSandboxNotFound
	}
	if err != nil {
		return provider.Status{}, err
	}
	if _, err := a.supportedJob(ctx, owned.JobID); err != nil {
		return provider.Status{}, err
	}
	if a.reader == nil {
		return provider.Status{}, controlapi.ErrSandboxStatusUnavailable
	}
	result, err := a.reader.ReadSandboxStatus(ctx, sandboxID)
	if errors.Is(err, controlreader.ErrSandboxNotFound) {
		return provider.Status{}, controlapi.ErrSandboxNotFound
	}
	if err != nil {
		return provider.Status{}, controlapi.ErrSandboxStatusUnavailable
	}
	return result, nil
}
