package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/spine"
)

type Config struct {
	Image     string
	Network   string
	DiskSize  string
	Workspace string
}

type Result = provider.Result

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

type ReviewMetadata = provider.ReviewMetadata

// ResolveImageFingerprint turns an operator-friendly Incus alias or prefix
// into the immutable image identity stored by a Sandbox profile.
var imageFingerprintLine = regexp.MustCompile(`(?m)^Fingerprint:\s*([0-9a-fA-F]{64})\s*$`)

func ResolveImageFingerprint(ctx context.Context, reference string, runner Runner) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", fmt.Errorf("Incus image reference is required")
	}
	if runner == nil {
		runner = CommandRunner{}
	}
	result, err := runner.Run(ctx, "incus", nil, "image", "info", reference)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", failure("resolve Incus image", result)
	}
	match := imageFingerprintLine.FindStringSubmatch(result.Stdout)
	if len(match) != 2 {
		return "", fmt.Errorf("Incus image reference %q did not resolve to an exact fingerprint", reference)
	}
	return strings.ToLower(match[1]), nil
}

// OwnershipMetadata is the host-owned identity of every Dorf Sandbox. The
// SandboxID is also the exact Incus instance name; callers must never discover
// or delete a Sandbox by a mutable workflow role.
type OwnershipMetadata = provider.Ownership

type OwnershipError = provider.OwnershipError

func ownershipErrorf(format string, args ...any) error {
	return provider.OwnershipErrorf(format, args...)
}

func (s Sandbox) Name(jobID string) string {
	return spine.MainSandboxName(jobID)
}

func validateOwnership(metadata OwnershipMetadata) error {
	if metadata.JobID == "" || metadata.SandboxID == "" || len(metadata.OwnershipNonce) != 64 {
		return fmt.Errorf("Sandbox requires complete host-owned identity metadata")
	}
	return nil
}

// ReconcileOwnedCreate creates or recovers the exact Sandbox recorded by the
// durable core. Workflow-specific labels are attached only after ownership is
// attested and are never part of cleanup identity.
func (s Sandbox) ReconcileOwnedCreate(ctx context.Context, metadata OwnershipMetadata) error {
	if err := validateOwnership(metadata); err != nil {
		return err
	}
	info, err := s.run(ctx, nil, "info", metadata.SandboxID)
	if err != nil {
		return err
	}
	if info.ExitCode != 0 {
		if !absent(info) {
			return failure("inspect Sandbox", info)
		}
		args := []string{"init", s.Config.Image, metadata.SandboxID, "--vm", "--network", s.Config.Network, "-d", "root,size=" + s.Config.DiskSize,
			"-c", "user.dorf.owner=sandbox", "-c", "user.dorf.job=" + metadata.JobID, "-c", "user.dorf.sandbox=" + metadata.SandboxID,
			"-c", "user.dorf.ownership_nonce=" + metadata.OwnershipNonce}
		created, createErr := s.run(ctx, nil, args...)
		if createErr != nil {
			return createErr
		}
		if created.ExitCode != 0 {
			return failure("create Sandbox", created)
		}
	}
	if err := s.AttestOwnership(ctx, metadata); err != nil {
		return err
	}
	start, err := s.run(ctx, nil, "start", metadata.SandboxID)
	if err != nil {
		return err
	}
	if start.ExitCode != 0 && !strings.Contains(strings.ToLower(start.Stderr+start.Stdout), "already running") {
		return failure("start Sandbox", start)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		ready, readyErr := s.Exec(ctx, metadata.SandboxID, nil, "true")
		if readyErr == nil && ready.ExitCode == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Incus guest agent did not become ready for Sandbox %s", metadata.SandboxID)
		}
		s.sleep(250 * time.Millisecond)
	}
	credentialCheck, err := s.Exec(ctx, metadata.SandboxID, nil, "bash", "-lc", "test ! -e /root/.codex/auth.json && test ! -e /root/.pi/agent/auth.json && test ! -e /root/.config/dorf/provider-route.key && test ! -e /root/.codex/config.toml && test ! -e /root/.pi/agent/models.json")
	if err != nil {
		return err
	}
	if credentialCheck.ExitCode != 0 {
		return fmt.Errorf("Sandbox is not credential-free before its scoped route")
	}
	workspace, err := s.Exec(ctx, metadata.SandboxID, nil, "mkdir", "-p", s.Config.Workspace)
	if err != nil {
		return err
	}
	if workspace.ExitCode != 0 {
		return failure("prepare Sandbox workspace", workspace)
	}
	return nil
}

func (s Sandbox) AttestOwnership(ctx context.Context, metadata OwnershipMetadata) error {
	if err := validateOwnership(metadata); err != nil {
		return err
	}
	instances, err := s.instances(ctx)
	if err != nil {
		return err
	}
	matches := 0
	for _, instance := range instances {
		if instance.Config["user.dorf.sandbox"] == metadata.SandboxID {
			matches++
			if instance.Name != metadata.SandboxID || instance.Config["user.dorf.owner"] != "sandbox" || instance.Config["user.dorf.job"] != metadata.JobID || instance.Config["user.dorf.ownership_nonce"] != metadata.OwnershipNonce {
				return ownershipErrorf("Sandbox metadata does not match its durable owner")
			}
		}
	}
	if matches != 1 {
		return ownershipErrorf("Sandbox metadata is missing, foreign, stale, or ambiguous")
	}
	return nil
}

// AttachReviewMetadata adds harness attestation labels only after the generic
// Job/Sandbox ownership has been verified. These labels are not consulted by
// cleanup.
func (s Sandbox) AttachReviewMetadata(ctx context.Context, ownership OwnershipMetadata, review ReviewMetadata) error {
	if review.JobID != ownership.JobID || review.OwnershipNonce != ownership.OwnershipNonce || review.AgentRunID == "" || review.Revision == "" {
		return fmt.Errorf("review Sandbox requires complete host-owned identity metadata")
	}
	if err := s.AttestOwnership(ctx, ownership); err != nil {
		return err
	}
	for _, label := range [][2]string{
		{"user.dorf.agent_run", review.AgentRunID},
		{"user.dorf.revision", review.Revision},
	} {
		key, value := label[0], label[1]
		result, err := s.run(ctx, nil, "config", "set", ownership.SandboxID, key, value)
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return failure("attach review Sandbox metadata", result)
		}
	}
	return s.AttestReview(ctx, ownership.SandboxID, review)
}

func (s Sandbox) OwnedPresent(ctx context.Context, metadata OwnershipMetadata) (bool, error) {
	if err := validateOwnership(metadata); err != nil {
		return false, err
	}
	instances, err := s.instances(ctx)
	if err != nil {
		return false, err
	}
	present := false
	ownedElsewhere := false
	for _, instance := range instances {
		if instance.Name == metadata.SandboxID {
			present = true
		}
		if instance.Config["user.dorf.sandbox"] == metadata.SandboxID && instance.Name != metadata.SandboxID {
			ownedElsewhere = true
		}
	}
	if !present {
		if ownedElsewhere {
			return false, ownershipErrorf("expected Sandbox is absent but another owned resource remains")
		}
		return false, nil
	}
	if err := s.AttestOwnership(ctx, metadata); err != nil {
		return false, err
	}
	return true, nil
}

func (s Sandbox) DeleteOwned(ctx context.Context, metadata OwnershipMetadata) error {
	present, err := s.OwnedPresent(ctx, metadata)
	if err != nil || !present {
		return err
	}
	return s.delete(ctx, metadata.SandboxID)
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
		"user.dorf.job":             metadata.JobID,
		"user.dorf.agent_run":       metadata.AgentRunID,
		"user.dorf.revision":        metadata.Revision,
		"user.dorf.ownership_nonce": metadata.OwnershipNonce,
	}
	owner := matches[0].Config["user.dorf.owner"]
	if owner != "sandbox" && owner != "review" {
		return ownershipErrorf("review Sandbox metadata user.dorf.owner does not match its durable owner")
	}
	if owner == "sandbox" && matches[0].Config["user.dorf.sandbox"] != name {
		return ownershipErrorf("review Sandbox metadata user.dorf.sandbox does not match its durable owner")
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

func (s Sandbox) instances(ctx context.Context) ([]reviewInstance, error) {
	result, err := s.run(ctx, nil, "list", "--format=json")
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, failure("discover bounded Sandboxes", result)
	}
	var instances []reviewInstance
	if err := json.Unmarshal([]byte(result.Stdout), &instances); err != nil {
		return nil, fmt.Errorf("decode Incus Sandbox inventory: %w", err)
	}
	if len(instances) > 1000 {
		return nil, ownershipErrorf("Incus Sandbox discovery exceeded its bounded inventory")
	}
	return instances, nil
}

func (s Sandbox) reviewMatches(ctx context.Context, runID string) ([]reviewInstance, error) {
	instances, err := s.instances(ctx)
	if err != nil {
		return nil, err
	}
	var matches []reviewInstance
	for _, instance := range instances {
		if instance.Config["user.dorf.agent_run"] == runID {
			matches = append(matches, instance)
		}
	}
	return matches, nil
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

func (s Sandbox) delete(ctx context.Context, name string) error {
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
