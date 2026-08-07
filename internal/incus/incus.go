package incus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

type Config struct {
	Image     string
	Network   string
	DiskSize  string
	Workspace string
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Runner interface {
	Run(context.Context, string, []byte, ...string) (Result, error)
}

type CommandRunner struct{}

func (CommandRunner) Run(ctx context.Context, command string, input []byte, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

type Sandbox struct {
	Config Config
	Runner Runner
	Sleep  func(time.Duration)
}

type ReviewMetadata struct {
	JobID          string
	AgentRunID     string
	Revision       string
	OwnershipNonce string
}

type OwnershipError struct{ Reason string }

func (e *OwnershipError) Error() string         { return e.Reason }
func (e *OwnershipError) AttentionNeeded() bool { return true }

func ownershipErrorf(format string, args ...any) error {
	return &OwnershipError{Reason: fmt.Sprintf(format, args...)}
}

func (s Sandbox) Name(jobID string) string {
	sum := sha256.Sum256([]byte(jobID))
	return "dorf-" + hex.EncodeToString(sum[:])[:20]
}

func (s Sandbox) ReconcileReviewCreate(ctx context.Context, name string, metadata ReviewMetadata) (string, error) {
	if name == "" || metadata.JobID == "" || metadata.AgentRunID == "" || metadata.Revision == "" || len(metadata.OwnershipNonce) != 64 {
		return "", fmt.Errorf("review Sandbox requires complete host-owned identity metadata")
	}
	matches, err := s.reviewMatches(ctx, metadata.AgentRunID)
	if err != nil {
		return "", err
	}
	if len(matches) > 1 || len(matches) == 1 && matches[0].Name != name {
		return "", ownershipErrorf("review Sandbox ownership is ambiguous for AgentRun %s", metadata.AgentRunID)
	}
	info, err := s.run(ctx, nil, "info", name)
	if err != nil {
		return "", err
	}
	if info.ExitCode != 0 {
		if !absent(info) {
			return "", failure("inspect review Sandbox", info)
		}
		created, err := s.run(ctx, nil, "init", s.Config.Image, name, "--vm", "--network", s.Config.Network, "-d", "root,size="+s.Config.DiskSize,
			"-c", "user.dorf.owner=review", "-c", "user.dorf.job="+metadata.JobID, "-c", "user.dorf.agent_run="+metadata.AgentRunID,
			"-c", "user.dorf.revision="+metadata.Revision, "-c", "user.dorf.ownership_nonce="+metadata.OwnershipNonce)
		if err != nil {
			return "", err
		}
		if created.ExitCode != 0 {
			return "", failure("create review Sandbox", created)
		}
	}
	if err := s.AttestReview(ctx, name, metadata); err != nil {
		return "", err
	}
	start, err := s.run(ctx, nil, "start", name)
	if err != nil {
		return "", err
	}
	if start.ExitCode != 0 && !strings.Contains(strings.ToLower(start.Stderr+start.Stdout), "already running") {
		return "", failure("start review Sandbox", start)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		ready, readyErr := s.Exec(ctx, name, nil, "true")
		if readyErr == nil && ready.ExitCode == 0 {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("Incus guest agent did not become ready for review Sandbox %s", name)
		}
		s.sleep(250 * time.Millisecond)
	}
	credentialCheck, err := s.Exec(ctx, name, nil, "bash", "-lc", "test ! -e /root/.codex/auth.json && test ! -e /root/.config/dorf/provider-route.key && test ! -e /root/.codex/config.toml")
	if err != nil {
		return "", err
	}
	if credentialCheck.ExitCode != 0 {
		return "", fmt.Errorf("review Sandbox is not credential-free before its scoped route")
	}
	return name, nil
}

func (s Sandbox) AttestReview(ctx context.Context, name string, metadata ReviewMetadata) error {
	matches, err := s.reviewMatches(ctx, metadata.AgentRunID)
	if err != nil {
		return err
	}
	if len(matches) != 1 || matches[0].Name != name {
		return ownershipErrorf("review Sandbox metadata is missing, foreign, stale, or ambiguous")
	}
	want := map[string]string{
		"user.dorf.owner":           "review",
		"user.dorf.job":             metadata.JobID,
		"user.dorf.agent_run":       metadata.AgentRunID,
		"user.dorf.revision":        metadata.Revision,
		"user.dorf.ownership_nonce": metadata.OwnershipNonce,
	}
	for key, value := range want {
		if matches[0].Config[key] != value {
			return ownershipErrorf("review Sandbox metadata %s does not match its durable owner", key)
		}
	}
	return nil
}

type reviewInstance struct {
	Name   string            `json:"name"`
	Config map[string]string `json:"config"`
}

func (s Sandbox) reviewMatches(ctx context.Context, runID string) ([]reviewInstance, error) {
	result, err := s.run(ctx, nil, "list", "--format=json")
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, failure("discover bounded review Sandboxes", result)
	}
	var instances []reviewInstance
	if err := json.Unmarshal([]byte(result.Stdout), &instances); err != nil {
		return nil, fmt.Errorf("decode Incus review Sandbox inventory: %w", err)
	}
	if len(instances) > 1000 {
		return nil, ownershipErrorf("Incus review Sandbox discovery exceeded its bounded inventory")
	}
	var matches []reviewInstance
	for _, instance := range instances {
		if instance.Config["user.dorf.agent_run"] == runID {
			matches = append(matches, instance)
		}
	}
	return matches, nil
}

func (s Sandbox) ReviewPresent(ctx context.Context, name string, metadata ReviewMetadata) (bool, error) {
	result, err := s.run(ctx, nil, "list", "--format=json")
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, failure("discover bounded review Sandboxes", result)
	}
	var instances []reviewInstance
	if err := json.Unmarshal([]byte(result.Stdout), &instances); err != nil {
		return false, fmt.Errorf("decode Incus review Sandbox inventory: %w", err)
	}
	if len(instances) > 1000 {
		return false, ownershipErrorf("Incus review Sandbox discovery exceeded its bounded inventory")
	}
	present := false
	for _, instance := range instances {
		if instance.Name == name {
			present = true
		}
	}
	if !present {
		matches := 0
		for _, instance := range instances {
			if instance.Config["user.dorf.agent_run"] == metadata.AgentRunID {
				matches++
			}
		}
		if matches != 0 {
			return false, ownershipErrorf("expected review Sandbox is absent but another owned resource remains")
		}
		return false, nil
	}
	if err := s.AttestReview(ctx, name, metadata); err != nil {
		return false, err
	}
	return true, nil
}

func (s Sandbox) DeleteReview(ctx context.Context, name string, metadata ReviewMetadata) error {
	info, err := s.run(ctx, nil, "info", name)
	if err != nil {
		return err
	}
	if info.ExitCode != 0 {
		if absent(info) {
			matches, matchErr := s.reviewMatches(ctx, metadata.AgentRunID)
			if matchErr != nil {
				return matchErr
			}
			if len(matches) != 0 {
				return ownershipErrorf("expected review Sandbox is absent but another owned resource remains")
			}
			return nil
		}
		return failure("inspect review Sandbox for deletion", info)
	}
	if err := s.AttestReview(ctx, name, metadata); err != nil {
		return err
	}
	return s.Delete(ctx, name)
}

func (s Sandbox) ReconcileCreate(ctx context.Context, jobID string) (string, error) {
	name := s.Name(jobID)
	info, err := s.run(ctx, nil, "info", name)
	if err != nil {
		return "", err
	}
	if info.ExitCode != 0 {
		if !absent(info) {
			return "", failure("inspect Sandbox", info)
		}
		created, err := s.run(ctx, nil, "init", s.Config.Image, name, "--vm", "--network", s.Config.Network, "-d", "root,size="+s.Config.DiskSize)
		if err != nil {
			return "", err
		}
		if created.ExitCode != 0 {
			return "", failure("create Sandbox", created)
		}
	}
	start, err := s.run(ctx, nil, "start", name)
	if err != nil {
		return "", err
	}
	if start.ExitCode != 0 && !strings.Contains(strings.ToLower(start.Stderr+start.Stdout), "already running") {
		return "", failure("start Sandbox", start)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		ready, err := s.Exec(ctx, name, nil, "true")
		if err == nil && ready.ExitCode == 0 {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("Incus guest agent did not become ready for Sandbox %s", name)
		}
		s.sleep(250 * time.Millisecond)
	}
	credentialCheck, err := s.Exec(ctx, name, nil, "bash", "-lc", "test ! -e /root/.codex/auth.json && test ! -e /root/.config/dorf/provider-route.key")
	if err != nil {
		return "", err
	}
	if credentialCheck.ExitCode != 0 {
		return "", fmt.Errorf("Sandbox image is not credential-free; refusing to install a scoped route")
	}
	result, err := s.Exec(ctx, name, nil, "mkdir", "-p", s.Config.Workspace)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", failure("prepare Sandbox workspace", result)
	}
	return name, nil
}

func (s Sandbox) ReconcileClone(ctx context.Context, name, repository, revision, branch string) error {
	gitDir, err := s.Exec(ctx, name, nil, "git", "-C", s.Config.Workspace, "rev-parse", "--git-dir")
	if err != nil {
		return err
	}
	if gitDir.ExitCode != 0 {
		clone, err := s.Exec(ctx, name, nil, "git", "clone", "--no-checkout", repository, s.Config.Workspace)
		if err != nil {
			return err
		}
		if clone.ExitCode != 0 {
			return failure("clone repository", clone)
		}
	} else {
		remote, err := s.Exec(ctx, name, nil, "git", "-C", s.Config.Workspace, "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		if remote.ExitCode != 0 || strings.TrimSpace(remote.Stdout) != repository {
			return fmt.Errorf("existing Sandbox clone origin does not match admitted repository")
		}
	}
	checkout, err := s.Exec(ctx, name, nil, "git", "-C", s.Config.Workspace, "checkout", "-B", branch, revision)
	if err != nil {
		return err
	}
	if checkout.ExitCode != 0 {
		return failure("checkout admitted Revision", checkout)
	}
	head, err := s.Exec(ctx, name, nil, "git", "-C", s.Config.Workspace, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head.ExitCode != 0 {
		return failure("observe Sandbox HEAD", head)
	}
	if strings.TrimSpace(head.Stdout) != revision {
		return fmt.Errorf("Sandbox HEAD %q does not match admitted Revision %q", strings.TrimSpace(head.Stdout), revision)
	}
	return nil
}

func (s Sandbox) BridgeIPv4(ctx context.Context) (string, error) {
	result, err := s.run(ctx, nil, "network", "get", s.Config.Network, "ipv4.address")
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", failure("resolve configured Incus bridge IPv4", result)
	}
	raw := strings.TrimSpace(result.Stdout)
	address, _, err := net.ParseCIDR(raw)
	if err != nil || address.To4() == nil || !address.IsPrivate() || address.IsLoopback() {
		return "", fmt.Errorf("configured Incus network %s has invalid private ipv4.address %q", s.Config.Network, raw)
	}
	return address.String(), nil
}

func (s Sandbox) InstallRoute(ctx context.Context, name, baseURL, key string) error {
	config := fmt.Sprintf("model_provider = \"dorf\"\n\n[model_providers.dorf]\nname = \"Dorf Provider Gateway\"\nbase_url = %q\nenv_key = \"DORF_PROVIDER_ROUTE_KEY\"\nwire_api = \"responses\"\nsupports_websockets = true\nrequires_openai_auth = false\n", baseURL)
	script := "umask 077; mkdir -p /root/.codex /root/.config/dorf; IFS= read -r config; printf '%s' \"$config\" > /root/.codex/config.toml; IFS= read -r key; printf '%s\\n' \"$key\" > /root/.config/dorf/provider-route.key"
	input := []byte(strings.ReplaceAll(config, "\n", "\\n") + "\n" + key + "\n")
	// printf %b decodes the newline escapes without interpolating either value.
	script = "umask 077; mkdir -p /root/.codex /root/.config/dorf; IFS= read -r config; printf '%b' \"$config\" > /root/.codex/config.toml; IFS= read -r key; printf '%s\\n' \"$key\" > /root/.config/dorf/provider-route.key"
	result, err := s.Exec(ctx, name, input, "bash", "-lc", script)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return failure("install scoped provider route", result)
	}
	return nil
}

func (s Sandbox) RemoveRoute(ctx context.Context, name string) error {
	result, err := s.Exec(ctx, name, nil, "rm", "-f", "/root/.config/dorf/provider-route.key", "/root/.codex/config.toml")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 && !absent(result) {
		return failure("remove scoped provider route", result)
	}
	return nil
}

func (s Sandbox) Delete(ctx context.Context, name string) error {
	info, err := s.run(ctx, nil, "info", name)
	if err != nil {
		return err
	}
	if info.ExitCode != 0 {
		if absent(info) {
			return nil
		}
		return failure("inspect Sandbox for deletion", info)
	}
	result, err := s.run(ctx, nil, "delete", name, "--force")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 && !absent(result) {
		return failure("delete Sandbox", result)
	}
	return nil
}

func (s Sandbox) PrivateIPv4(ctx context.Context, name string) (string, error) {
	result, err := s.Exec(ctx, name, nil, "ip", "-4", "route", "get", "1.1.1.1")
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", failure("resolve Sandbox private address", result)
	}
	fields := strings.Fields(result.Stdout)
	for i := range fields[:max(0, len(fields)-1)] {
		if fields[i] == "src" {
			address := net.ParseIP(fields[i+1])
			if address == nil || address.To4() == nil || !address.IsPrivate() {
				return "", fmt.Errorf("Sandbox default route source is not a private IPv4 address")
			}
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("Sandbox default route did not report an IPv4 source address")
}

func (s Sandbox) Exec(ctx context.Context, name string, input []byte, args ...string) (Result, error) {
	incusArgs := []string{"exec", name, "--"}
	incusArgs = append(incusArgs, args...)
	return s.run(ctx, input, incusArgs...)
}

func (s Sandbox) run(ctx context.Context, input []byte, args ...string) (Result, error) {
	runner := s.Runner
	if runner == nil {
		runner = CommandRunner{}
	}
	return runner.Run(ctx, "incus", input, args...)
}

func (s Sandbox) sleep(duration time.Duration) {
	if s.Sleep != nil {
		s.Sleep(duration)
		return
	}
	time.Sleep(duration)
}

func absent(result Result) bool {
	message := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(message, "not found") || strings.Contains(message, "doesn't exist") || strings.Contains(message, "does not exist")
}

func failure(operation string, result Result) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		detail = fmt.Sprintf("exit %d", result.ExitCode)
	}
	return fmt.Errorf("%s: %s", operation, detail)
}
