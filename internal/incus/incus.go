package incus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type Config struct {
	Image              string
	Network            string
	DiskSize           string
	Workspace          string
	ProviderGatewayURL string
	Connection         ConnectionConfig
}

type Result = provider.Result
type ReviewMetadata = provider.ReviewMetadata
type OwnershipMetadata = provider.Ownership
type OwnershipError = provider.OwnershipError

// Sandbox implements Dorf's Incus provider through one official-SDK-backed
// ClientFactory. Tests inject a narrow fake ClientFactory; there is no CLI
// runtime fallback or ambient Incus client configuration.
type Sandbox struct {
	Config        Config
	ClientFactory ClientFactory
	Sleep         func(time.Duration)
}

func ownershipErrorf(format string, args ...any) error {
	return provider.OwnershipErrorf(format, args...)
}

func (s Sandbox) Name(jobID string) string { return core.MainSandboxName(jobID) }

func validateOwnership(metadata OwnershipMetadata) error {
	if metadata.JobID == "" || metadata.SandboxID == "" || len(metadata.OwnershipNonce) != 64 {
		return fmt.Errorf("Sandbox requires complete host-owned identity metadata")
	}
	return nil
}

func (s Sandbox) open(ctx context.Context) (Client, error) {
	factory := s.ClientFactory
	if factory == nil {
		factory = SDKClientFactory{}
	}
	client, err := factory.Open(ctx, s.connection())
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (s Sandbox) connection() ConnectionConfig {
	if s.Config.Connection == (ConnectionConfig{}) {
		return DefaultConnectionConfig()
	}
	return s.Config.Connection
}

// ReconcileOwnedCreate creates or recovers the exact Sandbox recorded by the
// durable core. Workflow-specific labels are attached only after ownership is
// attested and are never part of cleanup identity.
func (s Sandbox) ReconcileOwnedCreate(ctx context.Context, metadata OwnershipMetadata) error {
	if err := validateOwnership(metadata); err != nil {
		return err
	}
	client, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	instance, err := client.Instance(ctx, metadata.SandboxID)
	if errors.Is(err, ErrNotFound) {
		err = client.CreateInstance(ctx, CreateInstanceRequest{
			Name: metadata.SandboxID, Image: s.Config.Image, Network: s.Config.Network,
			StoragePool: s.connection().StoragePool, DiskSize: s.Config.DiskSize,
			Config: ownershipConfig(metadata),
		})
		if err != nil {
			if missingImage(err) {
				return provider.ArtifactUnavailableErrorf("Incus image %q is unavailable: %v", s.Config.Image, err)
			}
			return err
		}
		instance, err = client.Instance(ctx, metadata.SandboxID)
	}
	if err != nil {
		return err
	}
	if err := s.attestOwnership(ctx, client, metadata); err != nil {
		return err
	}
	if !instance.Running {
		if err := client.StartInstance(ctx, metadata.SandboxID); err != nil {
			return err
		}
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		ready, readyErr := client.Exec(ctx, metadata.SandboxID, nil, "true")
		if readyErr == nil && ready.ExitCode == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Incus guest agent did not become ready for Sandbox %s", metadata.SandboxID)
		}
		s.sleep(250 * time.Millisecond)
	}
	credentialCheck, err := client.Exec(ctx, metadata.SandboxID, nil, "bash", "-lc", "test ! -e /root/.codex/auth.json && test ! -e /root/.pi/agent/auth.json && test ! -e /root/.config/dorf/provider-route.key && test ! -e /root/.codex/config.toml && test ! -e /root/.pi/agent/models.json")
	if err != nil {
		return err
	}
	if credentialCheck.ExitCode != 0 {
		return fmt.Errorf("Sandbox is not credential-free before its scoped route")
	}
	workspace, err := client.Exec(ctx, metadata.SandboxID, nil, "mkdir", "-p", s.Config.Workspace)
	if err != nil {
		return err
	}
	if workspace.ExitCode != 0 {
		return resultFailure("prepare Sandbox workspace", workspace)
	}
	return nil
}

func missingImage(err error) bool {
	detail := strings.ToLower(err.Error())
	return strings.Contains(detail, "image") && strings.Contains(detail, "not found")
}

func ownershipConfig(metadata OwnershipMetadata) map[string]string {
	return map[string]string{
		"user.dorf.owner":           "sandbox",
		"user.dorf.job":             metadata.JobID,
		"user.dorf.sandbox":         metadata.SandboxID,
		"user.dorf.ownership_nonce": metadata.OwnershipNonce,
	}
}

func (s Sandbox) AttestOwnership(ctx context.Context, metadata OwnershipMetadata) error {
	if err := validateOwnership(metadata); err != nil {
		return err
	}
	client, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	return s.attestOwnership(ctx, client, metadata)
}

func (s Sandbox) attestOwnership(ctx context.Context, client Client, metadata OwnershipMetadata) error {
	instances, err := boundedInstances(ctx, client)
	if err != nil {
		return err
	}
	return attestOwnershipIn(instances, metadata)
}

func boundedInstances(ctx context.Context, client Client) ([]Instance, error) {
	instances, err := client.Instances(ctx)
	if err != nil {
		return nil, err
	}
	if len(instances) > 1000 {
		return nil, ownershipErrorf("Incus Sandbox discovery exceeded its bounded inventory")
	}
	return instances, nil
}

func attestOwnershipIn(instances []Instance, metadata OwnershipMetadata) error {
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
	client, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := s.attestOwnership(ctx, client, ownership); err != nil {
		return err
	}
	if err := client.PatchInstanceConfig(ctx, ownership.SandboxID, ownershipConfig(ownership), map[string]string{
		"user.dorf.agent_run": review.AgentRunID,
		"user.dorf.revision":  review.Revision,
	}); err != nil {
		return err
	}
	return s.attestReview(ctx, client, ownership.SandboxID, review)
}

func (s Sandbox) OwnedPresent(ctx context.Context, metadata OwnershipMetadata) (bool, error) {
	if err := validateOwnership(metadata); err != nil {
		return false, err
	}
	client, err := s.open(ctx)
	if err != nil {
		return false, err
	}
	defer client.Close()
	return s.ownedPresent(ctx, client, metadata)
}

func (s Sandbox) ownedPresent(ctx context.Context, client Client, metadata OwnershipMetadata) (bool, error) {
	instances, err := boundedInstances(ctx, client)
	if err != nil {
		return false, err
	}
	present, ownedElsewhere := false, false
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
	if err := attestOwnershipIn(instances, metadata); err != nil {
		return false, err
	}
	return true, nil
}

func (s Sandbox) DeleteOwned(ctx context.Context, metadata OwnershipMetadata) error {
	if err := validateOwnership(metadata); err != nil {
		return err
	}
	client, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	present, err := s.ownedPresent(ctx, client, metadata)
	if err != nil || !present {
		return err
	}
	return client.DeleteInstance(ctx, metadata.SandboxID, ownershipConfig(metadata))
}

func (s Sandbox) AttestReview(ctx context.Context, name string, metadata ReviewMetadata) error {
	client, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	return s.attestReview(ctx, client, name, metadata)
}

func (s Sandbox) attestReview(ctx context.Context, client Client, name string, metadata ReviewMetadata) error {
	instances, err := boundedInstances(ctx, client)
	if err != nil {
		return err
	}
	matches := make([]Instance, 0, 1)
	for _, instance := range instances {
		if instance.Config["user.dorf.agent_run"] == metadata.AgentRunID {
			matches = append(matches, instance)
		}
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

func (s Sandbox) BridgeIPv4(ctx context.Context) (string, error) {
	client, err := s.open(ctx)
	if err != nil {
		return "", err
	}
	defer client.Close()
	raw, err := client.NetworkIPv4(ctx, s.Config.Network)
	if err != nil {
		return "", err
	}
	address, _, err := net.ParseCIDR(strings.TrimSpace(raw))
	if err != nil || address.To4() == nil || !address.IsPrivate() || address.IsLoopback() {
		return "", fmt.Errorf("configured Incus network %s has invalid private ipv4.address %q", s.Config.Network, strings.TrimSpace(raw))
	}
	return address.String(), nil
}

func (s Sandbox) PrivateIPv4(ctx context.Context, name string) (string, error) {
	client, err := s.open(ctx)
	if err != nil {
		return "", err
	}
	defer client.Close()
	result, err := client.Exec(ctx, name, nil, "ip", "-4", "route", "get", "1.1.1.1")
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", resultFailure("resolve Sandbox private address", result)
	}
	fields := strings.Fields(result.Stdout)
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == "src" {
			address := net.ParseIP(fields[index+1])
			if address == nil || address.To4() == nil || !address.IsPrivate() {
				return "", fmt.Errorf("Sandbox default route source is not a private IPv4 address")
			}
			return fields[index+1], nil
		}
	}
	return "", fmt.Errorf("Sandbox default route did not report an IPv4 source address")
}

func (s Sandbox) Exec(ctx context.Context, name string, input []byte, args ...string) (Result, error) {
	client, err := s.open(ctx)
	if err != nil {
		return Result{}, err
	}
	defer client.Close()
	return client.Exec(ctx, name, input, args...)
}

func (s Sandbox) PortForwardEndpoint(ctx context.Context, metadata OwnershipMetadata, port int) (provider.Endpoint, error) {
	if port < 1 || port > 65535 {
		return provider.Endpoint{}, fmt.Errorf("Incus endpoint port must be between 1 and 65535")
	}
	if err := validateOwnership(metadata); err != nil {
		return provider.Endpoint{}, err
	}
	client, err := s.open(ctx)
	if err != nil {
		return provider.Endpoint{}, err
	}
	if err := s.attestOwnership(ctx, client, metadata); err != nil {
		client.Close()
		return provider.Endpoint{}, err
	}
	client.Close()

	target := net.JoinHostPort("incus.invalid", fmt.Sprintf("%d", port))
	dial := func(dialCtx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != target {
			return nil, fmt.Errorf("Incus endpoint refused unexpected dial target %s %s", network, address)
		}
		attempt, err := s.open(dialCtx)
		if err != nil {
			return nil, err
		}
		if err := s.attestOwnership(dialCtx, attempt, metadata); err != nil {
			attempt.Close()
			return nil, err
		}
		connection, err := attempt.OpenPortForward(dialCtx, metadata.SandboxID, "127.0.0.1", port)
		if err != nil {
			attempt.Close()
			return nil, err
		}
		return &ownedConnection{Conn: connection, closeClient: attempt.Close}, nil
	}
	listenURL := "ws://" + net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
	dialURL := "ws://" + target
	return provider.NewDialEndpoint(listenURL, dialURL, nil, dial), nil
}

type ownedConnection struct {
	net.Conn
	once        sync.Once
	closeClient func()
}

func (c *ownedConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.closeClient)
	return err
}

func (s Sandbox) sleep(duration time.Duration) {
	if s.Sleep != nil {
		s.Sleep(duration)
		return
	}
	time.Sleep(duration)
}

func resultFailure(operation string, result Result) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		detail = fmt.Sprintf("exit %d", result.ExitCode)
	}
	return fmt.Errorf("%s: %s", operation, detail)
}
