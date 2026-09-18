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
	case "session":
		return true, remoteSessionCommand(ctx, args[1:], stdout, stderr)
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
		return true, sessionControlError(cfg.DeploymentURL, err)
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
	if discovery.Product != "dorf" || !slices.Contains(discovery.Capabilities, "direct_sessions") {
		return fmt.Errorf("the HTTPS endpoint is not a compatible Dorf direct-Session API")
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
		return sessionControlError(cfg.DeploymentURL, err)
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
	request := controlapi.CreateSessionRequest{
		KeepRunning: *keepRunning, ClientReference: *clientReference,
		AgentsMD: agentsMD, Profile: strings.TrimSpace(*profileName), AIConnection: strings.TrimSpace(*connection), Model: strings.TrimSpace(*model), Reasoning: strings.TrimSpace(*effort),
	}
	session, err := runKeyedMutation(ctx, requestKey, generated, stderr, "Admission may have succeeded.", func() (controlapi.Session, error) {
		return client.CreateSession(ctx, requestKey, request)
	})
	if err != nil {
		return err
	}
	input.Intent = "follow"
	message, err := runKeyedMutation(ctx, requestKey, generated, stderr, "Message may have been accepted.", func() (controlapi.Message, error) {
		return client.SendMessage(ctx, session.ID, requestKey, input)
	})
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(stdout, remoteRunReceipt{Deployment: cfg.DeploymentURL, RequestID: requestKey, Session: session, Message: message})
	}
	fmt.Fprintf(stdout, "Session %s accepted by %s\n", session.ID, cfg.DeploymentURL)
	renderRemoteSession(stdout, session)
	renderRemoteMessage(stdout, message)
	fmt.Fprintf(stdout, "Next: dorf session inspect %s\n", session.ID)
	return nil
}

type remoteRunReceipt struct {
	Deployment string             `json:"deployment"`
	RequestID  string             `json:"request_id"`
	Session    controlapi.Session `json:"session"`
	Message    controlapi.Message `json:"message"`
}

func remoteSessionCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	cfg, _, client, err := loadConnectedClient()
	if err != nil {
		return err
	}
	defer func() { err = sessionControlError(cfg.DeploymentURL, err) }()
	if len(args) == 0 {
		return fmt.Errorf("session requires: list, inspect, watch, message, retry, or cleanup")
	}
	switch args[0] {
	case "list":
		return remoteSessionList(ctx, client, args[1:], stdout, stderr)
	case "inspect", "cleanup":
		return remoteSessionSnapshot(ctx, cfg, client, args, stdout, stderr)
	case "watch":
		return remoteSessionWatch(ctx, client, args[1:], stdout, stderr)
	case "message":
		if len(args) > 1 && args[1] == "interrupt" {
			return remoteMessageInterrupt(ctx, cfg, client, args[2:], stdout, stderr)
		}
		if len(args) > 1 && args[1] == "inspect" {
			return remoteMessageInspect(ctx, client, args[2:], stdout, stderr)
		}
		return remoteMessageSend(ctx, cfg, client, args[1:], stdout, stderr)
	case "retry":
		return remoteSessionRetry(ctx, cfg, client, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("session requires: list, inspect, watch, message, retry, or cleanup")
	}
}

func remoteSessionSnapshot(ctx context.Context, cfg clientconfig.Config, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("session "+args[0], flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("session %s requires one Session ID", args[0])
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var session controlapi.Session
	var err error
	if args[0] == "inspect" {
		session, err = client.Session(ctx, set.Arg(0))
	} else {
		session, err = client.Cleanup(ctx, set.Arg(0))
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		if args[0] == "cleanup" {
			return writeJSON(stdout, remoteSessionReceipt{Deployment: cfg.DeploymentURL, Session: session})
		}
		return writeJSON(stdout, session)
	}
	if args[0] == "cleanup" {
		fmt.Fprintf(stdout, "Cleanup requested for Session %s on %s\n", session.ID, cfg.DeploymentURL)
		renderRemoteSession(stdout, session)
		return nil
	}
	renderRemoteSessionInspection(stdout, session)
	return nil
}

func remoteSessionWatch(ctx context.Context, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("session watch", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or jsonl")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("session watch requires one Session ID")
	}
	if *output != "human" && *output != "jsonl" {
		return fmt.Errorf("output must be human or jsonl")
	}
	watchCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	encoder := json.NewEncoder(stdout)
	err := client.WatchSession(watchCtx, set.Arg(0), func(session controlapi.Session) error {
		if *output == "jsonl" {
			return encoder.Encode(session)
		}
		fmt.Fprintf(stdout, "Session %s\n", session.ID)
		renderRemoteSession(stdout, session)
		return nil
	})
	if errors.Is(err, context.Canceled) && watchCtx.Err() != nil {
		return nil
	}
	return err
}

func remoteMessageSend(ctx context.Context, cfg clientconfig.Config, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("session message", flag.ContinueOnError)
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
		return fmt.Errorf("session message requires one Session ID")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *intent != "auto" && *intent != "follow" && *intent != "steer" {
		return fmt.Errorf("message intent must be auto, follow, or steer")
	}
	input, err := readMessageInput(*inputFile, "session message", attachmentPaths)
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
	fmt.Fprintf(stdout, "Message %s accepted for Session %s\n", message.ID, message.SessionID)
	renderRemoteMessage(stdout, message)
	fmt.Fprintf(stdout, "Next: dorf session message inspect %s %s\n", message.SessionID, message.ID)
	return nil
}

func remoteMessageInspect(ctx context.Context, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("session message inspect", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 || set.NArg() > 2 {
		return fmt.Errorf("session message inspect requires one Session ID and an optional Message ID")
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
	fmt.Fprintf(stdout, "Message %s for Session %s\n", message.ID, message.SessionID)
	renderRemoteMessage(stdout, message)
	return nil
}

func remoteSessionRetry(ctx context.Context, cfg clientconfig.Config, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("session retry", flag.ContinueOnError)
	set.SetOutput(stderr)
	key := set.String("key", "", "stable request identity for explicit replay")
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("session retry requires one Session ID")
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
	fmt.Fprintf(stdout, "Retry %s for Session %s\n", retry.State, retry.SessionID)
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

func humanSessionState(session controlapi.Session) string {
	if session.Attention != nil {
		return "Needs attention"
	}
	switch session.Execution.State {
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
		return session.Execution.State
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

type remoteSessionReceipt struct {
	Deployment string             `json:"deployment"`
	RequestID  string             `json:"request_id,omitempty"`
	Session    controlapi.Session `json:"session"`
}

func renderRemoteSession(output io.Writer, session controlapi.Session) {
	renderSessionAttribution(output, session.CreatedByClient, session.ClientReference)
	fmt.Fprintf(output, "  profile: %s\n  model: %q (%s)\n  admission: %s\n  execution: %s\n  cleanup: %s\n",
		session.Profile, session.Model, session.Reasoning, openClosed(session.Admission.Open), humanSessionState(session), session.Cleanup.State)
	if session.Attention != nil {
		fmt.Fprintf(output, "  attention: %s\n", session.Attention.Detail)
	}
	for _, sandbox := range session.Sandboxes {
		fmt.Fprintf(output, "  Sandbox: %s (%s)\n", sandbox.ID, sandbox.Name)
	}
}

func renderRemoteSessionInspection(output io.Writer, session controlapi.Session) {
	fmt.Fprintf(output, "Session %s\n", session.ID)
	renderRemoteSession(output, session)
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
		return clientconfig.Config{}, hostPath, nil, fmt.Errorf("Dorf Session control is not configured; run dorf setup on a deployment host or dorf connect HTTPS_URL on a remote client")
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

func sessionControlError(deploymentURL string, err error) error {
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

type controlAPISessions struct {
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

func (a controlAPISessions) application() core.Application {
	return coreApplication(a.store, a.tasks)
}

func (a controlAPISessions) Create(ctx context.Context, clientID, key string, input controlapi.CreateSessionRequest) (controlapi.Session, bool, error) {
	admission, err := newControlSessionAdmission(key, input.AgentsMD, input.Profile, input.AIConnection, input.Model, input.Reasoning)
	if err != nil {
		return controlapi.Session{}, false, err
	}
	admission.CreatedByClientID = clientID
	admission.ClientReference = input.ClientReference
	admission.KeepRunning = input.KeepRunning
	session, created, err := a.directAdmissions.Admit(ctx, admission)
	if errors.Is(err, direct.ErrAdmissionConflict) {
		return controlapi.Session{}, false, controlapi.ErrIdempotencyConflict
	}
	if errors.Is(err, postgres.ErrProfileNotFound) {
		return controlapi.Session{}, false, controlapi.ErrProfileNotFound
	}
	if errors.Is(err, direct.ErrInvalidAdmission) {
		return controlapi.Session{}, false, fmt.Errorf("%w: %v", controlapi.ErrInvalidInput, err)
	}
	if err != nil {
		return controlapi.Session{}, false, err
	}
	view, err := a.projectSession(ctx, session)
	return view, created, err
}

func validControlAdmissionKey(key string) bool {
	return key != "" && key == strings.TrimSpace(key) && len(key) <= 255 && !strings.ContainsRune(key, 0)
}

func newControlSessionAdmission(key, agentsMD, profile, connection, model, reasoning string) (direct.AdmissionRequest, error) {
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

func (a controlAPISessions) Get(ctx context.Context, sessionID string) (controlapi.Session, error) {
	session, err := a.loadSession(ctx, sessionID)
	if err != nil {
		return controlapi.Session{}, err
	}
	view, err := a.projectSession(ctx, session)
	if err != nil {
		return controlapi.Session{}, err
	}
	deliveries, err := a.store.Deliveries(ctx, sessionID)
	if err != nil {
		return controlapi.Session{}, err
	}
	view.LatestReplyID = latestReplyID(sessionID, deliveries)
	return view, nil
}

func latestReplyID(sessionID string, deliveries []core.Delivery) string {
	var id string
	var sequence int64
	for _, delivery := range deliveries {
		run, message := delivery.AgentRun, delivery.Message
		if run.SandboxID != core.MainSandboxName(sessionID) || message.Sequence <= sequence {
			continue
		}
		if run.State == core.AgentRunCompleted && run.TurnOutcome != "" || run.State == core.AgentRunFailed || run.State == core.AgentRunInterrupted {
			id, sequence = message.ID, message.Sequence
		}
	}
	return id
}

func (a controlAPISessions) SendMessage(ctx context.Context, sessionID, key string, input controlapi.SendMessageRequest) (controlapi.Message, bool, error) {
	session, err := a.loadSession(ctx, sessionID)
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
	attachments, err := a.retainMessageAttachments(ctx, session.ProfileRef(), input.Attachments)
	if err != nil {
		return controlapi.Message{}, false, err
	}
	if input.RefreshSkills {
		options = append(options, core.RefreshSkills())
	}
	handle, err := a.application().OpenSession(ctx, session.ID)
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
	message, err := a.GetMessage(ctx, session.ID, receipt.MessageID)
	return message, receipt.Created, err
}

func (a controlAPISessions) GetMessage(ctx context.Context, sessionID, messageID string) (controlapi.Message, error) {
	session, err := a.loadSession(ctx, sessionID)
	if err != nil {
		return controlapi.Message{}, err
	}
	deliveries, err := a.store.Deliveries(ctx, session.ID)
	if err != nil {
		return controlapi.Message{}, err
	}
	if messageID == "latest" {
		messageID = latestReplyID(session.ID, deliveries)
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
	result, err := a.messageResult(ctx, session, delivery)
	if err != nil {
		return controlapi.Message{}, err
	}
	attention := (*controlapi.Attention)(nil)
	if run.Attention != "" || run.State == core.AgentRunUncertain {
		attention = &controlapi.Attention{Code: "agent_delivery_attention", Detail: "Message delivery needs operator attention; inspect the deployment service logs."}
	}
	return controlapi.Message{
		ID: message.ID, SessionID: session.ID, Sequence: message.Sequence, Intent: string(message.Intent),
		InterruptRequested: run.InterruptRequested,
		WaitReason:         waitReason,
		Delivery:           controlapi.State{State: deliveryState}, Result: result, Attention: attention, AdmittedAt: message.AdmittedAt,
	}, nil
}

func (a controlAPISessions) messageResult(ctx context.Context, session core.Session, delivery core.Delivery) (*controlapi.MessageResult, error) {
	message, run := delivery.Message, delivery.AgentRun
	result := (*controlapi.MessageResult)(nil)
	if session.CleanupState == core.CleanupPending {
		switch run.State {
		case core.AgentRunCompleted:
			if run.TurnOutcome == "" {
				break
			}
			if a.reader == nil {
				return nil, fmt.Errorf("control reader is not configured")
			}
			observed, observeErr := a.reader.ObserveMessage(ctx, session.ID, message.ID)
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

func (a controlAPISessions) InterruptMessage(ctx context.Context, sessionID, messageID string) (controlapi.Message, error) {
	session, err := a.loadSession(ctx, sessionID)
	if err != nil {
		return controlapi.Message{}, err
	}
	execution, err := a.store.AgentMessageExecution(ctx, messageID)
	if errors.Is(err, postgres.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return controlapi.Message{}, controlapi.ErrMessageNotFound
	}
	if err != nil {
		return controlapi.Message{}, err
	}
	if execution.Session.ID != session.ID {
		return controlapi.Message{}, controlapi.ErrMessageNotFound
	}
	if execution.AgentRun.Harness != codex.Harness {
		return controlapi.Message{}, controlapi.ErrInterruptUnavailable
	}
	if _, err := a.application().RequestMessageInterrupt(ctx, session.ID, messageID); err != nil {
		if errors.Is(err, core.ErrMessageInterruptUnavailable) || errors.Is(err, core.ErrMessageAdmissionClosed) {
			return controlapi.Message{}, controlapi.ErrInterruptUnavailable
		}
		return controlapi.Message{}, err
	}
	return a.GetMessage(ctx, session.ID, messageID)
}

func remoteMessageInterrupt(ctx context.Context, cfg clientconfig.Config, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("session message interrupt", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 2 {
		return fmt.Errorf("session message interrupt requires one Session ID and Message ID")
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

func (a controlAPISessions) Retry(ctx context.Context, sessionID, key string) (controlapi.Retry, bool, error) {
	session, err := a.loadSession(ctx, sessionID)
	if err != nil {
		return controlapi.Retry{}, false, err
	}
	receipt, err := a.application().RetryFailedSession(ctx, session.ID, key)
	if err != nil {
		return controlapi.Retry{}, false, controlRetryError(err)
	}
	return controlapi.Retry{SessionID: receipt.SessionID, State: receipt.Retry}, receipt.Created, nil
}

func (a controlAPISessions) ReadSandboxFile(ctx context.Context, sandboxID, relativePath string) ([]byte, error) {
	owned, err := a.store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return nil, controlapi.ErrSandboxNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := a.loadSession(ctx, owned.SessionID); err != nil {
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

func (a controlAPISessions) WriteSandboxFile(ctx context.Context, sandboxID, relativePath string, contents []byte, ifAbsent bool) error {
	owned, err := a.store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return controlapi.ErrSandboxNotFound
	}
	if err != nil {
		return err
	}
	if _, err := a.loadSession(ctx, owned.SessionID); err != nil {
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

func (a controlAPISessions) RequestCleanup(ctx context.Context, sessionID string) (controlapi.Session, error) {
	session, err := a.loadSession(ctx, sessionID)
	if err != nil {
		return controlapi.Session{}, err
	}
	handle, err := a.application().OpenSession(ctx, session.ID)
	if err != nil {
		return controlapi.Session{}, err
	}
	if err := handle.RequestCleanup(ctx); err != nil {
		return controlapi.Session{}, err
	}
	return a.Get(ctx, session.ID)
}

func (a controlAPISessions) loadSession(ctx context.Context, sessionID string) (core.Session, error) {
	session, err := a.store.Session(ctx, sessionID)
	if errors.Is(err, postgres.ErrNotFound) {
		return core.Session{}, controlapi.ErrSessionNotFound
	}
	if err != nil {
		return core.Session{}, err
	}
	return session, nil
}

func (a controlAPISessions) projectSession(ctx context.Context, session core.Session) (controlapi.Session, error) {
	snapshot, err := direct.LoadSnapshot(ctx, a.store, session)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return controlapi.Session{}, controlapi.ErrSessionNotFound
		}
		return controlapi.Session{}, err
	}
	projection := snapshot.Project()
	task, err := fetchTaskResult(ctx, a.tasks, session.CurrentTaskID)
	if err != nil {
		return controlapi.Session{}, err
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
		return controlapi.Session{}, fmt.Errorf("Session %s has unknown direct execution state %q", session.ID, projection.State)
	}
	var attention *controlapi.Attention
	if projection.State == direct.ExecutionAttention {
		code := "agent_attention"
		if session.ExecutionAttention != "" {
			code = "session_attention"
		}
		attention = &controlapi.Attention{Code: code, Detail: "Session execution needs operator attention; inspect the deployment service logs."}
	}
	return a.projectSessionResources(ctx, session, executionState, attention, task, snapshot.Sandboxes)
}

func publicSessionCreator(id, name string) *controlapi.SessionCreator {
	if id == "" {
		return nil
	}
	return &controlapi.SessionCreator{ID: id, Name: name}
}

func publicSession(session core.Session, executionState string, attention *controlapi.Attention, task taskResultView, owned []core.Sandbox) (controlapi.Session, error) {
	if executionState == "" {
		return controlapi.Session{}, fmt.Errorf("Session %s has an incomplete public projection", session.ID)
	}
	cleanupState := map[core.CleanupState]string{
		core.CleanupPending: "not_requested", core.CleanupRequested: "requested",
		core.CleanupScheduled: "running", core.CleanupComplete: "complete",
	}[session.CleanupState]
	if cleanupState == "" {
		return controlapi.Session{}, fmt.Errorf("Session %s has unknown cleanup state %q", session.ID, session.CleanupState)
	}
	if (session.CleanupState == core.CleanupPending && failedExecutionTask(task.State)) ||
		(session.CleanupState == core.CleanupRequested && task.State == absurd.TaskFailed) {
		executionState = "failed"
		attention = publicExecutionFailure(task)
	}
	if session.CleanupState != core.CleanupPending {
		if executionState == "provisioning_sandbox" || executionState == "connecting_model_access" || executionState == "awaiting_agent" || executionState == "running" {
			executionState = "stopped"
		}
		if session.CleanupState == core.CleanupScheduled && failedExecutionTask(task.State) {
			cleanupState = "failed"
			attention = &controlapi.Attention{Code: "cleanup_failed", Detail: "Cleanup stopped before all resources were released; inspect the deployment service logs."}
		}
	}
	sandboxes := make([]controlapi.Sandbox, 0, len(owned))
	for _, sandbox := range owned {
		if sandbox.ID == "" || sandbox.SessionID != session.ID {
			return controlapi.Session{}, fmt.Errorf("Session %s has a mismatched Sandbox projection", session.ID)
		}
		sandboxes = append(sandboxes, controlapi.Sandbox{ID: sandbox.ID, Name: sandbox.Name, ResourceID: sandbox.ResourceID, ProviderID: sandbox.ProviderID})
	}
	return controlapi.Session{
		CreatedByClient: publicSessionCreator(session.CreatedByClientID, session.CreatedByClientName), ClientReference: session.ClientReference,
		ID: session.ID, Profile: session.SandboxProfile,
		KeepRunning: session.KeepRunning, Model: session.Model, Reasoning: session.ReasoningEffort,
		Admission: controlapi.Admission{Open: session.AdmissionOpen}, Execution: controlapi.State{State: executionState},
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
	return &controlapi.Attention{Code: "execution_failed", Detail: "Session execution stopped; inspect the deployment service logs, repair the cause, then retry."}
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
	sessions := controlAPISessions{
		store: store, tasks: tasks,
		directAdmissions: direct.NewAdmissionService(store, config.QueueName, reader),
		reader:           reader, blobs: blob.Store{Root: cfg.BlobRoot}, messageImages: runtimes,
	}
	server := controlapi.NewServer(controlapi.Discovery{
		Product: "dorf", Version: version.Version,
		Capabilities: []string{"direct_sessions", "session_list", "profile_list", "session_watch", "session_timeline", "messages", "message_interrupt", "session_retry", "sandbox_files", "sandbox_exec", "sandbox_status", "latest_reply"},
	}, auth, sessions, controlAPIProfiles{store: store})
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

func renderSessionAttribution(output io.Writer, creator *controlapi.SessionCreator, reference string) {
	if creator == nil {
		fmt.Fprintln(output, "  created by: unknown")
	} else {
		fmt.Fprintf(output, "  created by: %s (%s)\n", creator.Name, creator.ID)
	}
	if reference != "" {
		fmt.Fprintf(output, "  client reference: %q\n", reference)
	}
}

func (a controlAPISessions) ExecSandbox(ctx context.Context, sandboxID string, command provider.Command) (provider.CommandResult, error) {
	owned, err := a.store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return provider.CommandResult{}, controlapi.ErrSandboxNotFound
	}
	if err != nil {
		return provider.CommandResult{}, err
	}
	if _, err := a.loadSession(ctx, owned.SessionID); err != nil {
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

func (a controlAPISessions) ReadSandboxStatus(ctx context.Context, sandboxID string) (provider.Status, error) {
	owned, err := a.store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return provider.Status{}, controlapi.ErrSandboxNotFound
	}
	if err != nil {
		return provider.Status{}, err
	}
	if _, err := a.loadSession(ctx, owned.SessionID); err != nil {
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
