package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestDirectAdmissionKeyPreservesExplicitIdentityAndGeneratesBeforeAdmission(t *testing.T) {
	explicit, generated, err := directAdmissionKey("  caller-stable  ", nil)
	if err != nil || generated || explicit != "caller-stable" {
		t.Fatalf("explicit key=%q generated=%t err=%v", explicit, generated, err)
	}
	random := strings.NewReader(strings.Repeat("\xab", 16))
	key, generated, err := directAdmissionKey("", random)
	if err != nil || !generated || key != "direct-abababababababababababababababab" {
		t.Fatalf("generated key=%q generated=%t err=%v", key, generated, err)
	}
	if _, _, err := directAdmissionKey("", strings.NewReader("short")); err == nil {
		t.Fatal("short randomness generated a partial admission key")
	}
}

func TestOpenAIConnectionReadsAProtectedFileOrStandardInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openai.key")
	if err := os.WriteFile(path, []byte("  sk-file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := readSecretFile(path, strings.NewReader("unused"))
	if err != nil || fromFile != "sk-file-secret" {
		t.Fatalf("from file=%q err=%v", fromFile, err)
	}
	fromStdin, err := readSecretFile("-", strings.NewReader("sk-stdin-secret\n"))
	if err != nil || fromStdin != "sk-stdin-secret" {
		t.Fatalf("from stdin=%q err=%v", fromStdin, err)
	}
	if _, err := readSecretFile("-", strings.NewReader(" \n")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty secret error=%v", err)
	}
}

func TestProviderConnectionBecomesReadyOnlyAfterComposeAndLiveVerification(t *testing.T) {
	events := []string{}
	defaultConnection := "healthy"
	err := makeProviderConnectionReady(context.Background(), "openai-api",
		func(context.Context) error {
			events = append(events, "compose")
			return nil
		},
		func(_ context.Context, name string) error {
			events = append(events, "finalize:"+name)
			return nil
		},
		func(name string) error {
			events = append(events, "default:"+name)
			defaultConnection = name
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(events, ","), "compose,finalize:openai-api,default:openai-api"; got != want {
		t.Fatalf("provider readiness order=%q want %q", got, want)
	}
	if defaultConnection != "openai-api" {
		t.Fatalf("default connection=%q", defaultConnection)
	}
}

func TestProviderConnectionReadinessStopsAtEachFailedStage(t *testing.T) {
	for _, failure := range []string{"compose", "finalize", "default"} {
		t.Run(failure, func(t *testing.T) {
			events := []string{}
			defaultConnection := "healthy"
			failed := errors.New("stage failed")
			err := makeProviderConnectionReady(context.Background(), "personal-chatgpt",
				func(context.Context) error {
					events = append(events, "compose")
					if failure == "compose" {
						return failed
					}
					return nil
				},
				func(context.Context, string) error {
					events = append(events, "finalize")
					if failure == "finalize" {
						return failed
					}
					return nil
				},
				func(name string) error {
					events = append(events, "default")
					if failure == "default" {
						return failed
					}
					defaultConnection = name
					return nil
				},
			)
			if !errors.Is(err, failed) {
				t.Fatalf("error=%v", err)
			}
			want := map[string]string{
				"compose":  "compose",
				"finalize": "compose,finalize",
				"default":  "compose,finalize,default",
			}[failure]
			if got := strings.Join(events, ","); got != want {
				t.Fatalf("events=%q want %q", got, want)
			}
			if defaultConnection != "healthy" {
				t.Fatalf("failed %s changed default to %q", failure, defaultConnection)
			}
		})
	}
}

func TestGuidedSetupResumesRetainedOpenAICandidateWithoutEarlyDefaultCommit(t *testing.T) {
	configured := func(name string) (bool, error) { return name == "openai-api", nil }
	missingDefault := func() (string, error) { return "", errors.New("no default") }
	for _, options := range []setupOptions{{}, {ConnectionMode: setupConnectionOpenAI}} {
		name, retained, err := retainedSetupConnection(options, missingDefault, configured)
		if err != nil || !retained || name != "openai-api" {
			t.Fatalf("options=%#v name=%q retained=%t error=%v", options, name, retained, err)
		}
	}

	name, retained, err := retainedSetupConnection(setupOptions{}, missingDefault, func(string) (bool, error) { return true, nil })
	if err != nil || retained || name != "" {
		t.Fatalf("ambiguous candidates name=%q retained=%t error=%v", name, retained, err)
	}
}

func TestProviderGatewayRejectsUnscopedNonLoopbackBindBeforeStateMutation(t *testing.T) {
	_, _, err := providerGatewayForBind(context.Background(), postgres.Store{}, config.Config{}, "10.20.30.1", "")
	if err == nil || !strings.Contains(err.Error(), "matching Incus --profile") {
		t.Fatalf("unscoped bind error=%v", err)
	}
}

func TestProviderConnectReusesPersistedGatewayPublication(t *testing.T) {
	authority := &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	hash, err := authority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	profile := core.SandboxProfile{
		Name: "local", Provider: core.SandboxProviderIncus, IncusEndpointAuthorityHash: hash,
		IncusNetwork: "incusbr0", IncusGatewayURL: "http://10.44.0.1:8317/v1",
	}
	cfg := config.Config{Incus: authority}
	for _, prepared := range []string{"", "10.44.0.1"} {
		address, err := selectProviderGatewayBind(cfg, []core.SandboxProfile{profile}, &profile, "", prepared, prepared != "")
		if err != nil || address != "10.44.0.1" {
			t.Fatalf("prepared=%q address=%q error=%v", prepared, address, err)
		}
	}
	if _, err := selectProviderGatewayBind(cfg, []core.SandboxProfile{profile}, &profile, "", "10.44.0.2", true); err == nil || !strings.Contains(err.Error(), "conflicts with Sandbox Profile") {
		t.Fatalf("publication mismatch error=%v", err)
	}
	if _, err := selectProviderGatewayBind(cfg, []core.SandboxProfile{profile}, &profile, "10.44.0.2", "", false); err == nil || !strings.Contains(err.Error(), "exact Gateway URL") {
		t.Fatalf("explicit mismatch error=%v", err)
	}
	if address, err := selectProviderGatewayBind(cfg, []core.SandboxProfile{profile}, &profile, "10.44.0.1", "", false); err != nil || address != "10.44.0.1" {
		t.Fatalf("explicit matching address=%q error=%v", address, err)
	}
}

func TestProviderConnectPreservesOperatorRoutedPublication(t *testing.T) {
	authority := &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	hash, err := authority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	profile := core.SandboxProfile{
		Name: "remote", Provider: core.SandboxProviderIncus, IncusEndpointAuthorityHash: hash,
		IncusNetwork: "remote0", IncusGatewayURL: "https://gateway.example/v1",
	}
	address, err := selectProviderGatewayBind(config.Config{Incus: authority}, []core.SandboxProfile{profile}, &profile, "", "127.0.0.1", true)
	if err != nil || address != "127.0.0.1" {
		t.Fatalf("operator publication=%q error=%v", address, err)
	}
	other := &deployment.Incus{Endpoint: "unix:///run/incus/unix.socket"}
	if _, err := selectProviderGatewayBind(config.Config{Incus: other}, []core.SandboxProfile{profile}, &profile, "", "127.0.0.1", true); err == nil || !strings.Contains(err.Error(), "different Incus endpoint authority") {
		t.Fatalf("remote authority mismatch error=%v", err)
	}
}

func TestProviderConnectNeedsNoDefaultProfileForRetainedLoopbackPublication(t *testing.T) {
	selected, err := providerGatewayProfile(nil, "")
	if err != nil || selected != nil {
		t.Fatalf("selected profile=%#v error=%v", selected, err)
	}
	address, err := selectProviderGatewayBind(config.Config{}, nil, selected, "", "127.0.0.2", true)
	if err != nil || address != "127.0.0.2" {
		t.Fatalf("retained publication=%q error=%v", address, err)
	}
}

func TestGuidedGatewayBindPreservesPreparedAddressWithoutDirectProfile(t *testing.T) {
	address, privateBridge, resolve, err := selectGuidedGatewayBind(
		nil,
		[]guidedProfilePlan{{Provider: core.SandboxProviderE2B, Name: "cloud-codex"}},
		nil, "127.0.0.2", true,
	)
	if err != nil || resolve || address != "127.0.0.2" || privateBridge != "" {
		t.Fatalf("replayed publication=%q private bridge=%q resolve=%t error=%v", address, privateBridge, resolve, err)
	}
}

func TestSetupRetainsVerifiedE2BCredentialForComposeServices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "deployment.json")
	durable := deployment.Config{Database: deployment.Database{
		Host: "127.0.0.1", Port: 5432, Name: "dorf", User: "dorf", Password: "secret",
	}}
	if err := deployment.Save(path, durable); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DeploymentPath: path, E2BAPIKey: "verified-environment-key"}
	if err := retainSetupE2BCredential(&cfg, cfg.E2BAPIKey, false); err != nil {
		t.Fatal(err)
	}
	stored, found, err := deployment.Load(path)
	if err != nil || !found || stored.E2B == nil || stored.E2B.APIKey != cfg.E2BAPIKey {
		t.Fatalf("retained E2B credential: found=%t config=%#v err=%v", found, stored, err)
	}
	if err := retainSetupE2BCredential(&cfg, "rotated-verified-key", true); err != nil {
		t.Fatal(err)
	}
	stored, found, err = deployment.Load(path)
	if err != nil || !found || stored.E2B == nil || stored.E2B.APIKey != "rotated-verified-key" || cfg.E2BAPIKey != "rotated-verified-key" {
		t.Fatalf("rotated E2B credential: found=%t config=%#v runtime=%q err=%v", found, stored, cfg.E2BAPIKey, err)
	}

	external := config.Config{DatabaseExternal: true, E2BAPIKey: "environment-key"}
	if err := retainSetupE2BCredential(&external, external.E2BAPIKey, false); err != nil {
		t.Fatalf("existing external credential: %v", err)
	}
	if err := retainSetupE2BCredential(&external, "new-key", true); err == nil {
		t.Fatal("setup accepted an E2B credential it could not retain for an external deployment")
	}
}

func TestAIConnectionUsesTheDeploymentDefaultUnlessOverridden(t *testing.T) {
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "auth", "codex-account.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "connections.json"), []byte(`[{"name":"personal-chatgpt","provider":"chatgpt","auth_mode":"subscription","credential_ref":"codex-account.json","default":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	g := gateway.Gateway{StatePath: state}
	if got, err := selectedAIConnection(g, ""); err != nil || got != "personal-chatgpt" {
		t.Fatalf("default=%q err=%v", got, err)
	}
	if got, err := selectedAIConnection(g, " work-openai "); err != nil || got != "work-openai" {
		t.Fatalf("override=%q err=%v", got, err)
	}
}

func TestProfileUpdateIsTheOnlyDefinitionMutationCommand(t *testing.T) {
	var stdout, stderr strings.Builder
	if err := profileCommand(context.Background(), postgres.Store{}, config.Config{}, []string{"replace"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), `unsupported profile command "replace"`) {
		t.Fatalf("legacy mutation command error=%v", err)
	}
	if err := profileCommand(context.Background(), postgres.Store{}, config.Config{}, []string{"update"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "profile update requires NAME") {
		t.Fatalf("update command error=%v", err)
	}
}

func TestApplicationUpdateRejectsArgumentsBeforeHostConfiguration(t *testing.T) {
	var stdout, stderr strings.Builder
	err := run(context.Background(), []string{"update", "v9.9.9"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "does not accept arguments") {
		t.Fatalf("update argument error=%v", err)
	}
}

func TestProfileUpdatePatchContainsOnlyExplicitFlags(t *testing.T) {
	var stderr strings.Builder
	patch, err := parseSandboxProfilePatch(context.Background(), "update managed", core.SandboxProviderE2B, nil, "", "", []string{
		"--gateway-url", "https://replacement.example/v1", "--allow-internet=false",
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if patch.E2BGatewayURL == nil || *patch.E2BGatewayURL != "https://replacement.example/v1" ||
		patch.E2BAllowInternet == nil || *patch.E2BAllowInternet || patch.E2BArtifact != nil ||
		patch.E2BSandboxTimeout != nil || patch.Harness != nil ||
		patch.IncusArtifact != nil || patch.IncusEndpointAuthorityHash != nil || patch.IncusProject != nil ||
		patch.IncusStoragePool != nil || patch.IncusNetwork != nil || patch.IncusDiskSize != nil || patch.IncusGatewayURL != nil {
		t.Fatalf("patch contains omitted or incorrect fields: %#v", patch)
	}
	if _, err := parseSandboxProfilePatch(context.Background(), "update managed", core.SandboxProviderE2B, nil, "", "", nil, &stderr); err == nil || !strings.Contains(err.Error(), "at least one field flag") {
		t.Fatalf("empty patch error=%v", err)
	}
	if _, err := parseSandboxProfilePatch(context.Background(), "update managed", core.SandboxProviderE2B, nil, "", "", []string{"--sandbox-provider", "e2b"}, &stderr); err == nil {
		t.Fatal("profile update accepted a provider change")
	}
	if _, err := parseSandboxProfilePatch(context.Background(), "update managed", core.SandboxProviderE2B, nil, "", "", []string{"--image", "dorf"}, &stderr); err == nil || !strings.Contains(err.Error(), "does not accept Incus fields") {
		t.Fatalf("E2B Incus-field error=%v", err)
	}
}

func TestIncusProfileUpdateAcceptsOnlyExplicitDefinitionFields(t *testing.T) {
	var stderr strings.Builder
	patch, err := parseSandboxProfilePatch(context.Background(), "update local", core.SandboxProviderIncus, nil, "", "", []string{
		"--project", "dorf", "--storage-pool", "default", "--network", "incusbr0",
		"--disk-size", "80GiB", "--gateway-url", "http://10.20.30.1:8317/v1",
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if patch.IncusProject == nil || *patch.IncusProject != "dorf" ||
		patch.IncusStoragePool == nil || *patch.IncusStoragePool != "default" ||
		patch.IncusNetwork == nil || *patch.IncusNetwork != "incusbr0" ||
		patch.IncusDiskSize == nil || *patch.IncusDiskSize != "80GiB" ||
		patch.IncusGatewayURL == nil || *patch.IncusGatewayURL != "http://10.20.30.1:8317/v1" ||
		patch.E2BGatewayURL != nil {
		t.Fatalf("Incus patch=%#v", patch)
	}
	if _, err := parseSandboxProfilePatch(context.Background(), "update local", core.SandboxProviderIncus, nil, "", "",
		[]string{"--sandbox-timeout", "1h"}, &stderr); err == nil || !strings.Contains(err.Error(), "does not accept E2B fields") {
		t.Fatalf("Incus accepted E2B field: %v", err)
	}
}

func TestIncusProfileImageOperationsRequireDeploymentAuthorityAndUseProfileScope(t *testing.T) {
	var stderr strings.Builder
	_, err := parseSandboxProfile(context.Background(), "create local", "local", nil, []string{
		"--sandbox-provider", "incus", "--harness", "codex", "--image", "dorf",
	}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not configured in the Dorf Deployment") {
		t.Fatalf("missing create authority error=%v", err)
	}
	_, err = parseSandboxProfilePatch(context.Background(), "update local", core.SandboxProviderIncus, nil, "dorf", "default", []string{"--image", "dorf"}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not configured in the Dorf Deployment") {
		t.Fatalf("missing update authority error=%v", err)
	}

	authority := &deployment.Incus{Endpoint: "unix:///run/incus/dorf.socket"}
	connection, hash, err := incusProfileConnection(authority, "restricted-project", "dorf-pool")
	if err != nil {
		t.Fatal(err)
	}
	wantHash, err := authority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	if connection.Endpoint != authority.Endpoint || connection.Project != "restricted-project" || connection.StoragePool != "dorf-pool" || hash != wantHash {
		t.Fatalf("profile-scoped Incus connection=%#v hash=%q", connection, hash)
	}
}

func TestProviderGatewayStatusDistinguishesHistoricalVerificationFromCurrentReachability(t *testing.T) {
	verifiedAt := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	profile := core.SandboxProfile{
		Name: "managed", Provider: core.SandboxProviderE2B, E2BGatewayURL: "https://gateway.example/v1",
	}
	profile.DefinitionHash = profile.CurrentDefinitionHash()
	profile.Verification = &core.ProfileVerification{
		ContractVersion: core.BaseProfileContract, DefinitionHash: profile.DefinitionHash,
		ProbeCompletedAt: verifiedAt, CleanedAt: verifiedAt.Add(time.Second),
	}
	view := newProviderGatewayStatusView(profile, "personal-chatgpt", nil, context.DeadlineExceeded)
	if view.Ready || !view.ProfileVerified || view.ProfileVerifiedAt == nil || view.SandboxPath.Status != "failed" {
		t.Fatalf("status=%#v", view)
	}
	if !strings.Contains(view.Impact, "remote Sandboxes") || !strings.Contains(view.Next, "update and reverify") {
		t.Fatalf("unhelpful status impact=%q next=%q", view.Impact, view.Next)
	}
	var output strings.Builder
	renderProviderGatewayStatus(&output, view)
	for _, want := range []string{
		"managed · E2B", "verified previously ·", "Authority     ready",
		"Sandbox path  failed · https://gateway.example/v1", "remote Sandboxes using this profile cannot reach inference",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("status output omitted %q:\n%s", want, output.String())
		}
	}

	ready := newProviderGatewayStatusView(profile, "personal-chatgpt", nil, nil)
	if !ready.Ready || ready.Impact != "none" || ready.Next != "" {
		t.Fatalf("ready status=%#v", ready)
	}
	profile.Verification = nil
	unverified := newProviderGatewayStatusView(profile, "personal-chatgpt", nil, nil)
	if unverified.Ready || !strings.Contains(unverified.Next, "profile verify managed") {
		t.Fatalf("unverified status=%#v", unverified)
	}
	profile.Verification = &core.ProfileVerification{LastError: "E2B template not found"}
	unavailable := newProviderGatewayStatusView(profile, "personal-chatgpt", nil, nil)
	if unavailable.Ready || !strings.Contains(unavailable.Impact, "E2B template not found") || !strings.Contains(unavailable.Next, "repair or update") {
		t.Fatalf("unavailable status=%#v", unavailable)
	}
	localProfile := core.SandboxProfile{
		Name: "local", Provider: core.SandboxProviderIncus, IncusNetwork: "incusbr0",
		IncusGatewayURL: "http://10.44.0.1:8317/v1",
	}
	localProfile.DefinitionHash = localProfile.CurrentDefinitionHash()
	localProfile.Verification = &core.ProfileVerification{
		ContractVersion: core.BaseProfileContract, DefinitionHash: localProfile.DefinitionHash,
		ProbeCompletedAt: verifiedAt, CleanedAt: verifiedAt.Add(time.Second),
	}
	local := newProviderGatewayStatusView(localProfile, "personal-chatgpt", nil, nil)
	if !local.Ready || local.SandboxPath.Status != "ready" || !strings.Contains(local.SandboxPath.Detail, "anonymous access rejected") {
		t.Fatalf("local status omitted current Gateway reachability: %#v", local)
	}
	localFailed := newProviderGatewayStatusView(localProfile, "personal-chatgpt", nil, errors.New("Compose publication mismatch"))
	if localFailed.Ready || localFailed.SandboxPath.Status != "failed" || !strings.Contains(localFailed.Next, "update and reverify") {
		t.Fatalf("local mismatch status=%#v", localFailed)
	}
}

func TestProviderStatusFencesIncusProfileAgainstComposePublication(t *testing.T) {
	statePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(statePath, "compose.json"), []byte(`{"publish_address":"10.44.0.2"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := core.SandboxProfile{
		Name: "local", Provider: core.SandboxProviderIncus,
		IncusGatewayURL: "http://10.44.0.1:8317/v1",
	}
	g := gateway.Gateway{StatePath: statePath}
	if err := validateIncusComposePublication(g, profile); err == nil || !strings.Contains(err.Error(), "publishes 10.44.0.2") {
		t.Fatalf("publication mismatch error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "compose.json"), []byte(`{"publish_address":"10.44.0.1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateIncusComposePublication(g, profile); err != nil {
		t.Fatal(err)
	}
}

func TestProviderStatusUsesFreshDNSForTheExactDorfOwnedTunnel(t *testing.T) {
	statePath := t.TempDir()
	cloudflarePath := filepath.Join(statePath, "cloudflare")
	if err := os.MkdirAll(cloudflarePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloudflarePath, "state.json"), []byte(`{
  "schema_version": 1,
  "tunnel_name": "dorf-proof",
  "hostname": "api.dorf.example",
  "model_hostname": "models.dorf.example",
  "origin": "http://127.0.0.1:8317",
  "probe_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := core.SandboxProfile{Provider: core.SandboxProviderE2B, E2BGatewayURL: "https://models.dorf.example/v1"}
	g, err := remoteGatewayForProviderStatus(config.Config{GatewayStatePath: statePath}, profile)
	if err != nil || g.Client == nil || g.DeploymentProbeURL != "https://models.dorf.example/.dorf/probe/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("gateway=%#v err=%v", g, err)
	}
	profile.E2BGatewayURL = "https://operator.example/v1"
	g, err = remoteGatewayForProviderStatus(config.Config{GatewayStatePath: statePath}, profile)
	if err != nil || g.Client != nil || g.DeploymentProbeURL != "" {
		t.Fatalf("foreign gateway=%#v err=%v", g, err)
	}
}

func TestSetupAutomationApprovalAndSelectionsAreExplicit(t *testing.T) {
	var stderr strings.Builder
	options, err := parseSetupOptions([]string{
		"--yes", "--local-image", "dorf-proof:0.5.2", "--ai-connection", "personal-chatgpt",
		"--sandbox-provider", "incus", "--sandbox-provider", "e2b",
		"--harness", "codex", "--e2b-template", "dorf:exact-build",
		"--incus-manifest", "/proof/manifest.json", "--incus-archive", "/proof/image.tar.zst",
		"--gateway-url", "https://gateway.example/v1", "--allow-internet",
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !options.Yes || options.Connection != "personal-chatgpt" || options.ProfileName != "" ||
		options.LocalImage != "dorf-proof:0.5.2" || options.IncusManifest != "/proof/manifest.json" || options.IncusArchive != "/proof/image.tar.zst" ||
		options.Harness != "codex" || options.E2BTemplate != "dorf:exact-build" ||
		options.GatewayURL != "https://gateway.example/v1" || !options.AllowInternet ||
		!containsSandboxProvider(options.SandboxProviders, core.SandboxProviderIncus) ||
		!containsSandboxProvider(options.SandboxProviders, core.SandboxProviderE2B) {
		t.Fatalf("options=%#v", options)
	}
	replacement, err := parseSetupOptions([]string{
		"--sandbox-provider", "e2b", "--cloudflare-domain", "dorf.run",
		"--cloudflare-control-hostname", "control.dorf.run",
		"--cloudflare-model-hostname", "inference.dorf.run",
		"--replace-cloudflare-dns",
	}, &stderr)
	if err != nil || replacement.CloudflareDomain != "dorf.run" || replacement.CloudflareControlHostname != "control.dorf.run" ||
		replacement.CloudflareModelHostname != "inference.dorf.run" || !replacement.ReplaceCloudflareDNS {
		t.Fatalf("replacement options=%#v error=%v", replacement, err)
	}
	if _, err := parseSetupOptions([]string{
		"--sandbox-provider", "e2b", "--replace-cloudflare-dns",
	}, &stderr); err == nil || !strings.Contains(err.Error(), "--cloudflare-domain") {
		t.Fatalf("domain-less replacement error=%v", err)
	}
	if _, err := parseSetupOptions([]string{
		"--sandbox-provider", "e2b", "--cloudflare-control-hostname", "api.dorf.run",
	}, &stderr); err == nil || !strings.Contains(err.Error(), "--cloudflare-domain") {
		t.Fatalf("domain-less endpoint error=%v", err)
	}
	if _, err := parseSetupOptions([]string{
		"--sandbox-provider", "e2b", "--gateway-url", "https://gateway.example/v1", "--cloudflare-domain", "dorf.run",
	}, &stderr); err == nil || !strings.Contains(err.Error(), "either") {
		t.Fatalf("mixed ingress error=%v", err)
	}
	if _, err := parseSetupOptions([]string{
		"--sandbox-provider", "e2b", "--cloudflare-hostname", "dorf.run",
	}, &stderr); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("removed hostname flag error=%v", err)
	}
	if _, err := parseSetupOptions([]string{"--profile", "local-codex", "--sandbox-provider", "incus", "--sandbox-provider", "e2b"}, &stderr); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("multi-provider named profile error=%v", err)
	}
	if _, err := parseSetupOptions([]string{"--sandbox-provider", "incus", "--ai-connection", "personal-chatgpt", "--connection-auth", "chatgpt"}, &stderr); err == nil || !strings.Contains(err.Error(), "either") {
		t.Fatalf("conflicting connection input error=%v", err)
	}
	for _, args := range [][]string{
		{"--sandbox-provider", "incus", "--incus-manifest", "manifest.json"},
		{"--sandbox-provider", "incus", "--incus-archive", "image.tar.zst"},
		{"--sandbox-provider", "e2b", "--incus-manifest", "manifest.json", "--incus-archive", "image.tar.zst"},
	} {
		if _, err := parseSetupOptions(args, &stderr); err == nil {
			t.Fatalf("invalid Incus image transport was accepted: %v", args)
		}
	}
	if _, err := parseSetupOptions([]string{"--sandbox-provider", "unknown"}, &stderr); err == nil {
		t.Fatal("unknown Sandbox provider was accepted")
	}
	if _, err := parseSetupOptions([]string{"unexpected"}, &stderr); err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("setup positional argument error=%v", err)
	}
	if _, err := parseSetupOptions([]string{"--yes", "--e2b-template", "dorf:build"}, &stderr); err == nil || !strings.Contains(err.Error(), "--sandbox-provider") {
		t.Fatalf("provider-less agent setup error=%v", err)
	}
	for _, invalid := range []string{"", "dorf", ":build", "dorf:", "dorf:build id"} {
		if err := validateExactE2BTemplate(invalid); err == nil {
			t.Fatalf("accepted E2B template %q", invalid)
		}
	}
	if template, err := setupE2BTemplate(setupOptions{}); err != nil || template != guidedE2BTemplate {
		t.Fatalf("guided E2B template=%q error=%v", template, err)
	}
	if template, err := setupE2BTemplate(setupOptions{E2BTemplate: "operator/custom:exact-build"}); err != nil || template != "operator/custom:exact-build" {
		t.Fatalf("custom E2B template=%q error=%v", template, err)
	}
	automated, err := selectSetupSandboxProviders(context.Background(), config.Config{}, setupOptions{Yes: true}, newSetupPresenter(&strings.Builder{}))
	if err != nil || len(automated) != 0 {
		t.Fatalf("common-only automation providers=%v error=%v", automated, err)
	}
	inferred, settled := deriveSetupSandboxProviders(config.Config{E2BAPIKey: "configured"}, setupOptions{}, true, false)
	if !settled || len(inferred) != 1 || inferred[0] != core.SandboxProviderE2B {
		t.Fatalf("cloud-only inference providers=%v settled=%t", inferred, settled)
	}
	if _, settled := deriveSetupSandboxProviders(config.Config{E2BAPIKey: "configured"}, setupOptions{}, true, true); settled {
		t.Fatal("setup inferred one provider when local and cloud were both viable")
	}
	for _, test := range []struct {
		ready map[string]bool
		want  string
	}{
		{ready: map[string]bool{"personal-chatgpt": true}, want: "personal-chatgpt"},
		{ready: map[string]bool{"openai-api": true}, want: "openai-api"},
		{ready: map[string]bool{"personal-chatgpt": true, "openai-api": true}},
		{ready: map[string]bool{}},
	} {
		got, err := unambiguousSetupConnection(func(name string) (bool, error) {
			return test.ready[name], nil
		})
		if err != nil || got != test.want {
			t.Fatalf("ready connections=%v got=%q want=%q error=%v", test.ready, got, test.want, err)
		}
	}
}

func TestGuidedSetupPresenterKeepsChoicesAndControlsUsable(t *testing.T) {
	presenter := setupPresenter{}
	providers := []core.SandboxProvider{}
	provider := presenter.ProviderGroup(&providers, true)
	harness := presenter.HarnessGroup(new(string))
	connection := presenter.ConnectionGroup(new(setupConnectionMode))
	controlHostname, modelHostname := "api.dorf.run", "models.dorf.run"
	endpoints := presenter.PublicEndpointsGroup("dorf.run", &controlHostname, &modelHostname)
	for _, group := range []struct{ name, header, content string }{
		{"provider", provider.Header(), provider.Content()},
		{"harness", harness.Header(), harness.Content()},
		{"connection", connection.Header(), connection.Content()},
		{"endpoints", endpoints.Header(), endpoints.Content()},
	} {
		if strings.TrimSpace(group.header) == "" || strings.TrimSpace(group.content) == "" {
			t.Fatalf("%s group header=%q content=%q", group.name, group.header, group.content)
		}
	}
	if content := provider.Content(); !strings.Contains(content, "Local · Incus") || !strings.Contains(content, "Cloud · E2B") {
		t.Fatalf("provider choices=%q", content)
	}
	if content := harness.Content(); !strings.Contains(content, "Codex") || !strings.Contains(content, "Pi") {
		t.Fatalf("Harness choices=%q", content)
	}
	unavailable := presenter.ProviderGroup(&providers, false).Content()
	if !strings.Contains(unavailable, "[—] Local · Incus") || !strings.Contains(unavailable, "Unavailable") || strings.Contains(unavailable, "[ ] Local · Incus") {
		t.Fatalf("KVM-disabled provider choices=%q", unavailable)
	}

	keymap := setupKeyMap()
	if help := keymap.Select.Next.Help(); help.Key != "enter" || help.Desc != "continue" {
		t.Fatalf("select help=%#v", help)
	}
	if help := keymap.MultiSelect.Toggle.Help(); help.Key != "space" || help.Desc != "select" {
		t.Fatalf("toggle help=%#v", help)
	}
	if help := keymap.MultiSelect.Submit.Help(); help.Key != "enter" || help.Desc != "continue" {
		t.Fatalf("submit help=%#v", help)
	}
	styles := setupTheme(true)
	if styles.Focused.SelectSelector.String() == styles.Blurred.SelectSelector.String() ||
		styles.Focused.FocusedButton.Render("Continue") == styles.Focused.BlurredButton.Render("Continue") {
		t.Fatal("focused controls lack a distinct indicator")
	}
	for _, label := range []string{"Control API", "Model Gateway", "api.dorf.run", "models.dorf.run"} {
		if !strings.Contains(endpoints.Content(), label) {
			t.Fatalf("public endpoint inputs omitted %q:\n%s", label, endpoints.Content())
		}
	}
}

func TestManagedSetupRejectsExternalPostgreSQLBeforeHostWork(t *testing.T) {
	var stdout strings.Builder
	err := setupCommand(context.Background(), config.Config{DatabaseExternal: true}, nil, &stdout, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "DORF_DATABASE_URL") {
		t.Fatalf("external setup error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("external setup performed presentation or host work: %q", stdout.String())
	}
}

func TestManagedSetupRejectsRetiredSingleOriginTunnelBeforeHostWork(t *testing.T) {
	statePath := t.TempDir()
	cloudflarePath := filepath.Join(statePath, "cloudflare")
	if err := os.MkdirAll(cloudflarePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloudflarePath, "state.json"), []byte(`{
  "schema_version": 1,
  "tunnel_name": "dorf-retired",
  "hostname": "dorf.example.com",
  "origin": "http://127.0.0.1:8317"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	err := setupCommand(context.Background(), config.Config{GatewayStatePath: statePath}, nil, &stdout, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "retired single-origin") {
		t.Fatalf("retired Tunnel error=%v", err)
	}
	if strings.Contains(stdout.String(), "Host runtime") || strings.Contains(stdout.String(), "Starting Dorf services") {
		t.Fatalf("retired Tunnel reached host work: %q", stdout.String())
	}
}

func TestGuidedGatewayPlanningReusesTheDurableProfileContract(t *testing.T) {
	existing := core.SandboxProfile{
		Name: "cloud-codex", Provider: core.SandboxProviderE2B, Harness: "codex",
		Artifact: "dorf:exact-build", E2BGatewayURL: "https://gateway.example/v1",
	}
	plan, err := planRemoteGateway(context.Background(), setupOptions{}, []guidedProfilePlan{{
		Provider: core.SandboxProviderE2B, Name: existing.Name, Harness: existing.Harness, Existing: &existing,
	}}, setupPresenter{}, fakeDNSResolver{}, ownedCloudflareEndpoints{})
	if err != nil || plan.Mode != setupGatewayExisting || plan.URL != existing.E2BGatewayURL {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	owned := ownedCloudflareEndpoints{ControlHostname: "api.dorf.run", ModelHostname: "models.dorf.run"}
	if _, err := planRemoteGateway(context.Background(), setupOptions{GatewayURL: "https://gateway.example/v1"}, nil, setupPresenter{}, fakeDNSResolver{}, owned); err == nil || !strings.Contains(err.Error(), "remove") {
		t.Fatalf("custom Gateway with retained Tunnel error=%v", err)
	}
	cloudflare := fakeDNSResolver{records: map[string][]*net.NS{
		"dorf.run": {{Host: "cash.ns.cloudflare.com."}, {Host: "lana.ns.cloudflare.com."}},
	}}
	plan, err = planRemoteGateway(context.Background(), setupOptions{CloudflareDomain: "dorf.run"}, []guidedProfilePlan{{
		Provider: core.SandboxProviderE2B, Name: existing.Name, Harness: existing.Harness, Existing: &existing,
	}}, setupPresenter{}, cloudflare, ownedCloudflareEndpoints{})
	if err != nil || plan.URL != "https://models.dorf.run/v1" {
		t.Fatalf("retarget plan=%#v error=%v", plan, err)
	}
	for _, invalid := range []string{
		"http://gateway.example/v1", "https://user@gateway.example/v1", "https://gateway.example/v1/", "https://gateway.example/v1?token=secret",
	} {
		if err := validateExactGatewayURL(invalid); err == nil {
			t.Fatalf("accepted Gateway URL %q", invalid)
		}
	}
}

func TestGuidedGatewayPlanningRequiresCloudflareDNSAndAnUnusedHostname(t *testing.T) {
	cloudflare := fakeDNSResolver{records: map[string][]*net.NS{
		"dorf.run": {{Host: "cash.ns.cloudflare.com."}, {Host: "lana.ns.cloudflare.com."}},
	}}
	plan, err := planRemoteGateway(context.Background(), setupOptions{CloudflareDomain: "dorf.run"}, nil, setupPresenter{}, cloudflare, ownedCloudflareEndpoints{})
	if err != nil || plan.Mode != setupGatewayCloudflare || plan.Domain != "dorf.run" ||
		plan.ControlHostname != "api.dorf.run" || plan.ModelHostname != "models.dorf.run" ||
		plan.ControlURL != "https://api.dorf.run" || plan.URL != "https://models.dorf.run/v1" {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}

	nonCloudflare := fakeDNSResolver{records: map[string][]*net.NS{
		"dorf.run": {{Host: "ns1.other-provider.example."}},
	}}
	if _, err := planRemoteGateway(context.Background(), setupOptions{CloudflareDomain: "dorf.run"}, nil, setupPresenter{}, nonCloudflare, ownedCloudflareEndpoints{}); err == nil || !strings.Contains(err.Error(), "not Cloudflare") {
		t.Fatalf("non-Cloudflare error=%v", err)
	}

	occupied := cloudflare
	occupied.addresses = map[string][]net.IPAddr{"api.dorf.run": {{IP: net.ParseIP("192.0.2.1")}}}
	if _, err := planRemoteGateway(context.Background(), setupOptions{CloudflareDomain: "dorf.run"}, nil, setupPresenter{}, occupied, ownedCloudflareEndpoints{}); err == nil || !strings.Contains(err.Error(), "already resolves") {
		t.Fatalf("occupied Control API hostname error=%v", err)
	}
	occupied.addresses = map[string][]net.IPAddr{"models.dorf.run": {{IP: net.ParseIP("192.0.2.2")}}}
	if _, err := planRemoteGateway(context.Background(), setupOptions{CloudflareDomain: "dorf.run"}, nil, setupPresenter{}, occupied, ownedCloudflareEndpoints{}); err == nil || !strings.Contains(err.Error(), "already resolves") {
		t.Fatalf("occupied model Gateway hostname error=%v", err)
	}
	occupied.addresses = map[string][]net.IPAddr{"api.dorf.run": {{IP: net.ParseIP("192.0.2.1")}}}
	owned := ownedCloudflareEndpoints{ControlHostname: "api.dorf.run", ModelHostname: "models.dorf.run"}
	if plan, err := planRemoteGateway(context.Background(), setupOptions{CloudflareDomain: "dorf.run"}, nil, setupPresenter{}, occupied, owned); err != nil || plan.Mode != setupGatewayCloudflare {
		t.Fatalf("owned hostname replay plan=%#v error=%v", plan, err)
	}
}

func TestGuidedGatewayPlanningCanExplicitlyReplaceCloudflareDNS(t *testing.T) {
	occupied := fakeDNSResolver{
		records: map[string][]*net.NS{
			"dorf.run": {{Host: "cash.ns.cloudflare.com."}, {Host: "lana.ns.cloudflare.com."}},
		},
		addresses: map[string][]net.IPAddr{
			"control.dorf.run":   {{IP: net.ParseIP("192.0.2.1")}},
			"inference.dorf.run": {{IP: net.ParseIP("192.0.2.2")}},
		},
	}
	plan, err := planRemoteGateway(context.Background(), setupOptions{
		CloudflareDomain: "dorf.run", CloudflareControlHostname: "control.dorf.run",
		CloudflareModelHostname: "inference.dorf.run", ReplaceCloudflareDNS: true,
	}, nil, setupPresenter{}, occupied, ownedCloudflareEndpoints{})
	if err != nil || plan.Mode != setupGatewayCloudflare || plan.ControlURL != "https://control.dorf.run" ||
		plan.URL != "https://inference.dorf.run/v1" || !plan.ReplaceExistingDNS {
		t.Fatalf("replacement plan=%#v error=%v", plan, err)
	}
}

func TestGuidedGatewayPlanningRetargetsTheOwnedSingleOriginProfile(t *testing.T) {
	existing := core.SandboxProfile{
		Name: "cloud-codex", Provider: core.SandboxProviderE2B, Harness: "codex",
		Artifact: "dorf:exact-build", E2BGatewayURL: "https://api.dorf.run/v1",
	}
	owned := ownedCloudflareEndpoints{ControlHostname: "api.dorf.run", ModelHostname: "models.dorf.run"}
	plan, err := planRemoteGateway(context.Background(), setupOptions{}, []guidedProfilePlan{{
		Provider: core.SandboxProviderE2B, Name: existing.Name, Harness: existing.Harness, Existing: &existing,
	}}, setupPresenter{}, fakeDNSResolver{}, owned)
	if err != nil || plan.Mode != setupGatewayCloudflare || plan.ControlHostname != owned.ControlHostname ||
		plan.ModelHostname != owned.ModelHostname || plan.ControlURL != "https://api.dorf.run" || plan.URL != "https://models.dorf.run/v1" {
		t.Fatalf("plan=%#v error=%v", plan, err)
	}
}

func TestGuidedGatewayPlanningPreservesExactRetainedEndpointsWithoutAProfile(t *testing.T) {
	owned := ownedCloudflareEndpoints{ControlHostname: "control.dorf.run", ModelHostname: "inference.dorf.run"}
	plan, err := planRemoteGateway(context.Background(), setupOptions{}, nil, setupPresenter{}, fakeDNSResolver{}, owned)
	if err != nil || plan.Domain != "dorf.run" || plan.ControlHostname != owned.ControlHostname ||
		plan.ModelHostname != owned.ModelHostname || plan.ControlURL != "https://control.dorf.run" || plan.URL != "https://inference.dorf.run/v1" {
		t.Fatalf("retained plan=%#v error=%v", plan, err)
	}
}

func TestCloudflareGatewayPlanRequiresDistinctDirectChildren(t *testing.T) {
	controlOverride, err := newCloudflareGatewayPlan("DORF.RUN", "control.dorf.run", "", false)
	if err != nil || controlOverride.ControlHostname != "control.dorf.run" || controlOverride.ModelHostname != "models.dorf.run" {
		t.Fatalf("control override plan=%#v error=%v", controlOverride, err)
	}
	modelOverride, err := newCloudflareGatewayPlan("dorf.run", "", "inference.dorf.run", false)
	if err != nil || modelOverride.ControlHostname != "api.dorf.run" || modelOverride.ModelHostname != "inference.dorf.run" {
		t.Fatalf("model override plan=%#v error=%v", modelOverride, err)
	}
	for _, test := range []struct {
		name, control, model string
	}{
		{name: "equal", control: "api.dorf.run", model: "api.dorf.run"},
		{name: "control apex", control: "dorf.run", model: "models.dorf.run"},
		{name: "model outside", control: "api.dorf.run", model: "models.example.com"},
		{name: "nested control", control: "edge.api.dorf.run", model: "models.dorf.run"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newCloudflareGatewayPlan("dorf.run", test.control, test.model, false); err == nil {
				t.Fatalf("accepted control=%q model=%q", test.control, test.model)
			}
		})
	}
}

func TestGuidedGatewayBindPreservesExistingLocalProfiles(t *testing.T) {
	profiles := []core.SandboxProfile{{
		Name: "local-codex", Provider: core.SandboxProviderIncus, IncusNetwork: "incusbr0",
		IncusProject: "restricted", IncusStoragePool: "dorf-pool", IncusGatewayURL: "http://10.44.0.1:8317/v1",
	}}
	selected := []guidedProfilePlan{{Provider: core.SandboxProviderE2B, Name: "cloud-codex"}}
	network, err := gatewayIncusNetwork(profiles, selected)
	if err != nil || network != "incusbr0" {
		t.Fatalf("network=%q err=%v", network, err)
	}
	project, pool := gatewayIncusScope(profiles, selected, network)
	if project != "restricted" || pool != "dorf-pool" {
		t.Fatalf("Incus Gateway scope=%s/%s", project, pool)
	}
	address, required, err := guidedIncusBridgeAuthority(profiles, selected, network)
	if err != nil || !required || address != "10.44.0.1" {
		t.Fatalf("Incus Gateway address=%q required=%t error=%v", address, required, err)
	}
	_, err = gatewayIncusNetwork([]core.SandboxProfile{{
		Provider: core.SandboxProviderIncus, IncusNetwork: "incusbr0", IncusGatewayURL: "http://10.44.0.1:8317/v1",
	}}, []guidedProfilePlan{{
		Provider: core.SandboxProviderIncus, Name: "local-pi", Existing: &core.SandboxProfile{
			Provider: core.SandboxProviderIncus, IncusNetwork: "otherbr0", IncusGatewayURL: "http://10.55.0.1:8317/v1",
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "multiple networks") {
		t.Fatalf("ambiguous network error=%v", err)
	}
}

func TestGuidedGatewayBindNeverRewritesExistingProfileAuthority(t *testing.T) {
	profiles := []core.SandboxProfile{
		{Name: "first", Provider: core.SandboxProviderIncus, IncusNetwork: "incusbr0", IncusGatewayURL: "http://10.44.0.1:8317/v1"},
		{Name: "same", Provider: core.SandboxProviderIncus, IncusNetwork: "incusbr0", IncusGatewayURL: "http://10.44.0.1:8317/v1"},
	}
	address, required, err := guidedIncusBridgeAuthority(profiles, nil, "incusbr0")
	if err != nil || !required || address != "10.44.0.1" {
		t.Fatalf("shared authority address=%q required=%t error=%v", address, required, err)
	}
	profiles[1].IncusGatewayURL = "http://10.44.0.2:8317/v1"
	if _, _, err := guidedIncusBridgeAuthority(profiles, nil, "incusbr0"); err == nil || !strings.Contains(err.Error(), "different Gateway addresses") {
		t.Fatalf("divergent profile authority error=%v", err)
	}

	httpsProfile := core.SandboxProfile{
		Name: "remote", Provider: core.SandboxProviderIncus, IncusNetwork: "remote0", IncusGatewayURL: "https://gateway.example/v1",
	}
	if network, err := gatewayIncusNetwork([]core.SandboxProfile{httpsProfile}, nil); err != nil || network != "" {
		t.Fatalf("operator-routed HTTPS profile selected guided bridge %q: %v", network, err)
	}
	if address, required, err := guidedIncusBridgeAuthority([]core.SandboxProfile{httpsProfile}, nil, ""); err != nil || required || address != "" {
		t.Fatalf("operator-routed HTTPS authority address=%q required=%t error=%v", address, required, err)
	}

	address, required, err = guidedIncusBridgeAuthority(nil, []guidedProfilePlan{{Provider: core.SandboxProviderIncus, Name: "new"}}, guidedIncusNetwork)
	if err != nil || !required || address != "" {
		t.Fatalf("new Incus profile address=%q required=%t error=%v", address, required, err)
	}
}

func TestGuidedGatewayBindConsumesPersistedPrivateRouteWithoutLocalInference(t *testing.T) {
	authority := testRemoteIncusAuthority(t)
	hash, err := authority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	profile := core.SandboxProfile{
		Name: "remote", Provider: core.SandboxProviderIncus,
		IncusEndpointAuthorityHash: hash, IncusNetwork: "vpn0",
		IncusGatewayURL: "http://100.64.0.10:8317/v1",
	}
	address, privateBridge, resolve, err := selectGuidedGatewayBind(
		[]core.SandboxProfile{profile},
		[]guidedProfilePlan{{Provider: core.SandboxProviderE2B, Name: "cloud-codex"}},
		authority, "100.64.0.10", true,
	)
	if err != nil || resolve || address != "100.64.0.10" || privateBridge != "" {
		t.Fatalf("address=%q private bridge=%q resolve=%t error=%v", address, privateBridge, resolve, err)
	}
}

func TestGuidedGatewayBindRejectsNewIncusProfileOnRemoteAuthorityBeforeRouteReuse(t *testing.T) {
	authority := testRemoteIncusAuthority(t)
	hash, err := authority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	profile := core.SandboxProfile{
		Name: "remote", Provider: core.SandboxProviderIncus,
		IncusEndpointAuthorityHash: hash, IncusNetwork: guidedIncusNetwork,
		IncusGatewayURL: "http://100.64.0.10:8317/v1",
	}
	_, _, _, err = selectGuidedGatewayBind(
		[]core.SandboxProfile{profile},
		[]guidedProfilePlan{{Provider: core.SandboxProviderIncus, Name: "new-local"}},
		authority, "100.64.0.10", true,
	)
	if err == nil || !strings.Contains(err.Error(), "guided remote Incus setup is not supported") {
		t.Fatalf("remote Incus setup error=%v", err)
	}
}

func testRemoteIncusAuthority(t *testing.T) *deployment.Incus {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "incus.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return &deployment.Incus{
		Endpoint: "https://incus.example:8443", ServerCertificate: certificate,
		ClientCertificate: certificate, ClientPrivateKey: privateKey,
	}
}

func TestIncusReadinessIsScopedOnlyToASelectedIncusProfile(t *testing.T) {
	if _, _, selected := guidedIncusReadinessScope([]guidedProfilePlan{{Provider: core.SandboxProviderE2B}}); selected {
		t.Fatal("E2B-only setup inherited a configured Incus prerequisite")
	}
	existing := &core.SandboxProfile{IncusProject: "restricted", IncusStoragePool: "dorf-pool"}
	project, pool, selected := guidedIncusReadinessScope([]guidedProfilePlan{{Provider: core.SandboxProviderIncus, Existing: existing}})
	if !selected || project != "restricted" || pool != "dorf-pool" {
		t.Fatalf("selected Incus readiness scope=%s/%s selected=%t", project, pool, selected)
	}
}

func TestGuidedNewLocalIncusPersistsEndpointAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "deployment.json")
	if err := deployment.Save(path, deployment.Config{Database: deployment.Database{
		Host: "127.0.0.1", Port: 5432, Name: "dorf", User: "dorf", Password: "secret",
	}}); err != nil {
		t.Fatal(err)
	}
	authority := deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	if err := persistGuidedIncusAuthority(&config.Config{DeploymentPath: path, Incus: &authority}, guidedIncusNetwork); err != nil {
		t.Fatal(err)
	}
	stored, found, err := deployment.Load(path)
	if err != nil || !found || stored.Incus == nil || *stored.Incus != authority {
		t.Fatalf("persisted authority found=%t Incus=%#v err=%v", found, stored.Incus, err)
	}
}

func TestGuidedIncusAuthorityDriftStopsBeforeReadinessHandoff(t *testing.T) {
	profileAuthority := &deployment.Incus{Endpoint: "unix:///remote/incus/unix.socket"}
	hash, err := profileAuthority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	existing := &core.SandboxProfile{
		Name: "remote", Provider: core.SandboxProviderIncus,
		IncusEndpointAuthorityHash: hash,
	}
	configured := &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	err = setupGuidedIncusReadiness(context.Background(), []guidedProfilePlan{{
		Provider: core.SandboxProviderIncus, Name: existing.Name, Existing: existing,
	}}, configured)
	if err == nil || !strings.Contains(err.Error(), "different Incus endpoint authority") {
		t.Fatalf("authority drift error=%v", err)
	}
	var readiness incusSetupReadinessError
	if errors.As(err, &readiness) {
		t.Fatalf("authority drift became a local Incus readiness handoff: %v", err)
	}
}

func TestGuidedExistingVerifiedRemoteIncusIsRejectedBeforeAuthorityOrHostWork(t *testing.T) {
	authority := &deployment.Incus{Endpoint: "https://unreachable.example:8443"}
	verifiedAt := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	profile := core.SandboxProfile{
		Name: "existing-remote", Provider: core.SandboxProviderIncus, Harness: "codex",
		Artifact: "dorf:verified", IncusEndpointAuthorityHash: "different-authority",
		IncusProject: "restricted", IncusStoragePool: "dorf-pool", IncusNetwork: "remote0",
		IncusDiskSize: "40GiB", IncusGatewayURL: "https://gateway.example/v1",
	}
	profile.DefinitionHash = profile.CurrentDefinitionHash()
	profile.Verification = &core.ProfileVerification{
		ProfileName: profile.Name, ContractVersion: core.BaseProfileContract, DefinitionHash: profile.DefinitionHash,
		ProbeCompletedAt: verifiedAt, CleanedAt: verifiedAt.Add(time.Second),
	}
	if !profile.BaseVerified() {
		t.Fatal("test profile is not verified")
	}

	probed := false
	err := setupGuidedIncusReadinessWith(context.Background(), []guidedProfilePlan{{
		Provider: core.SandboxProviderIncus, Name: profile.Name, Existing: &profile,
	}}, authority, func(context.Context, *deployment.Incus, string, string) error {
		probed = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "guided remote Incus setup is not supported") {
		t.Fatalf("existing remote guided setup error=%v", err)
	}
	if strings.Contains(err.Error(), "different Incus endpoint authority") {
		t.Fatalf("profile authority validation ran before remote Incus policy: %v", err)
	}
	if probed {
		t.Fatal("existing remote profile reuse reached endpoint readiness")
	}
}

func TestIncusProfilePublicationRequiresTheSameDeploymentEndpointAuthority(t *testing.T) {
	authority := &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	hash, err := authority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	profile := core.SandboxProfile{Name: "local", Provider: core.SandboxProviderIncus, IncusEndpointAuthorityHash: hash}
	if err := validateIncusProfileEndpointAuthority(authority, profile); err != nil {
		t.Fatal(err)
	}
	other := &deployment.Incus{Endpoint: "unix:///run/incus/unix.socket"}
	if err := validateIncusProfileEndpointAuthority(other, profile); err == nil || !strings.Contains(err.Error(), "different Incus endpoint authority") {
		t.Fatalf("endpoint mismatch error=%v", err)
	}
	if err := validateIncusProfileEndpointAuthority(nil, profile); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing endpoint error=%v", err)
	}
}

func TestGuidedIncusGatewayAcceptsPrivateAndSharedIPv4Only(t *testing.T) {
	for raw, want := range map[string]bool{
		"10.20.30.1":      true,
		"172.20.0.1":      true,
		"100.64.0.1":      true,
		"100.127.255.254": true,
		"100.128.0.1":     false,
		"127.0.0.1":       false,
		"192.0.2.1":       false,
	} {
		got := privateOrSharedIPv4(net.ParseIP(raw))
		if got != want {
			t.Fatalf("address %s accepted=%t want %t", raw, got, want)
		}
	}
}

func TestProfileInstallValidatesIdentityBeforeArtifactMutation(t *testing.T) {
	for _, args := range [][]string{
		{"Invalid_Name", "--release", "v1.2.3", "--harness", "codex"},
		{"local", "--release", "v1.2.3", "--harness", "unknown"},
	} {
		var stdout, stderr strings.Builder
		err := installOfficialIncusProfile(context.Background(), postgres.Store{}, args, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "Sandbox profile") {
			t.Fatalf("args=%v error=%v", args, err)
		}
	}
	var stdout, stderr strings.Builder
	err := installOfficialIncusProfile(context.Background(), postgres.Store{}, []string{
		"local", "--release", "v1.2.3", "--harness", "codex", "--gateway-url", "http://10.20.30.1:8317/v1",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not configured in the Dorf Deployment") {
		t.Fatalf("missing Deployment authority error=%v", err)
	}
}

func TestSandboxForProfileSelectsOneConcreteAdapter(t *testing.T) {
	managedProfile := core.SandboxProfile{
		Name: "managed", Provider: core.SandboxProviderE2B, Artifact: "dorf:exact-build",
		E2BGatewayURL: "https://gateway.example/v1", E2BSandboxTimeout: 55 * time.Minute,
	}
	managed, err := sandboxForProfile(config.Config{E2BAPIKey: "test-key", Workspace: "/workspace/job", TurnTimeout: 45 * time.Minute}, managedProfile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := managed.(e2b.Adapter); !ok {
		t.Fatalf("managed adapter = %T", managed)
	}
	if _, err := sandboxForProfile(config.Config{Workspace: "/workspace/job"}, managedProfile); err == nil || !strings.Contains(err.Error(), "E2B_API_KEY") {
		t.Fatalf("missing E2B API key error = %v", err)
	}
	managedProfile.E2BGatewayURL = "http://gateway.example/v1"
	if _, err := sandboxForProfile(config.Config{E2BAPIKey: "test-key", Workspace: "/workspace/job"}, managedProfile); err == nil {
		t.Fatal("invalid remote Gateway URL was admitted")
	}
}

func TestTaskResultProjectionPublishesOnlyBoundedFailureMessage(t *testing.T) {
	view := projectTaskResult("task-1", &absurd.TaskResultSnapshot{
		State:   absurd.TaskFailed,
		Failure: json.RawMessage(`{"name":"*errors.errorString","message":"clone repository:\nCould not resolve host github.com","traceback":"secret stack details"}`),
	})
	if view.LastError != "clone repository: Could not resolve host github.com" {
		t.Fatalf("last error = %q", view.LastError)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"traceback", "secret stack details", `"failure"`} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("public task projection exposed %q: %s", hidden, encoded)
		}
	}

	long := boundedTaskError(json.RawMessage(`{"message":"` + strings.Repeat("x", 400) + `"}`))
	if got := len([]rune(long)); got != 320 || !strings.HasSuffix(long, "…") {
		t.Fatalf("bounded error has %d runes", got)
	}
}

func TestSandboxProfileNotReadyDetailSurfacesUnavailableArtifact(t *testing.T) {
	profile := core.SandboxProfile{Name: "cloud-codex", Verification: &core.ProfileVerification{LastError: "E2B template is unavailable"}}
	detail := sandboxProfileNotReadyDetail(profile)
	if !strings.Contains(detail, `Sandbox profile "cloud-codex" is unavailable: E2B template is unavailable`) ||
		!strings.Contains(detail, "repair or update it, then run dorf profile verify cloud-codex") {
		t.Fatalf("detail = %q", detail)
	}
}
