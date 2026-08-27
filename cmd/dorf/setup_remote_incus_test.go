package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/incus"
)

func TestSetupIncusTrustOfferInputIsBoundedAndProtected(t *testing.T) {
	if got, err := readSetupIncusTrustOffer("-", strings.NewReader("  retained-offer\n")); err != nil || got != "retained-offer" {
		t.Fatalf("standard input offer=%q error=%v", got, err)
	}
	for _, input := range []string{"", "bad\x00offer", strings.Repeat("x", (16<<10)+1)} {
		if got, err := readSetupIncusTrustOffer("-", strings.NewReader(input)); err == nil || got != "" {
			t.Fatalf("invalid standard input returned offer length=%d error=%v", len(got), err)
		}
	}

	offerDir := filepath.Join(t.TempDir(), "offers")
	if err := os.Mkdir(offerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(offerDir, "offer.txt")
	if err := os.WriteFile(path, []byte("protected-offer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readSetupIncusTrustOffer(path, nil); err != nil || got != "protected-offer" {
		t.Fatalf("protected file offer=%q error=%v", got, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSetupIncusTrustOffer(path, nil); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("unprotected offer error=%v", err)
	}
}

func TestSetupInteractiveRemoteIncusOfferAndProfileNaming(t *testing.T) {
	providers := []core.SandboxProvider{core.SandboxProviderIncus}
	cfg := &config.Config{}
	if !setupNeedsInteractiveIncusTrustOffer(cfg, setupOptions{}, providers, false, true) {
		t.Fatal("interactive KVM-free Incus selection did not request a trust offer")
	}
	for _, test := range []struct {
		name        string
		cfg         *config.Config
		options     setupOptions
		providers   []core.SandboxProvider
		kvm         bool
		interactive bool
	}{
		{name: "automated", cfg: cfg, options: setupOptions{Yes: true}, providers: providers, interactive: true},
		{name: "file", cfg: cfg, options: setupOptions{IncusTrustOfferFile: "offer.txt"}, providers: providers, interactive: true},
		{name: "local KVM", cfg: cfg, providers: providers, kvm: true, interactive: true},
		{name: "noninteractive", cfg: cfg, providers: providers},
		{name: "E2B", cfg: cfg, providers: []core.SandboxProvider{core.SandboxProviderE2B}, interactive: true},
		{name: "accepted", cfg: &config.Config{Incus: testRemoteIncusAuthority(t)}, providers: providers, interactive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if setupNeedsInteractiveIncusTrustOffer(test.cfg, test.options, test.providers, test.kvm, test.interactive) {
				t.Fatal("unexpected interactive trust-offer prompt")
			}
		})
	}
	if got, err := selectedSetupIncusTrustOffer("", "  pasted-offer\n", nil); err != nil || got != "pasted-offer" {
		t.Fatalf("pasted offer=%q error=%v", got, err)
	}
	if _, err := selectedSetupIncusTrustOffer("", "", nil); err == nil || !strings.Contains(err.Error(), "--incus-trust-offer-file") {
		t.Fatalf("automated missing offer error=%v", err)
	}
	if got := guidedIncusProfileName("codex", testRemoteIncusAuthority(t)); got != "incus-codex" {
		t.Fatalf("remote Incus profile name=%q", got)
	}
	if got := guidedIncusProfileName("pi", &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}); got != "local-pi" {
		t.Fatalf("local Incus profile name=%q", got)
	}
}

func TestGuidedIncusProfileTargetSelectsTheFixedEndpointBoundary(t *testing.T) {
	local := &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	remote := testRemoteIncusAuthority(t)
	for _, test := range []struct {
		name                      string
		authority                 *deployment.Incus
		project, storage, network string
	}{
		{
			name: "local Unix", authority: local,
			project: incus.DefaultProject, storage: incus.DefaultStoragePool, network: guidedIncusNetwork,
		},
		{
			name: "remote HTTPS", authority: remote,
			project: incus.RemoteProjectName, storage: incus.DefaultStoragePool, network: incus.RemoteNetworkName,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, storage, network, err := guidedIncusProfileTarget(test.authority)
			if err != nil || project != test.project || storage != test.storage || network != test.network {
				t.Fatalf("target=%s/%s/%s want=%s/%s/%s error=%v", project, storage, network, test.project, test.storage, test.network, err)
			}
		})
	}
	if _, _, _, err := guidedIncusProfileTarget(nil); err == nil {
		t.Fatal("guided Incus target accepted no deployment authority")
	}
}

func TestSetupKeepsFreshLocalIncusInMemoryUntilReadiness(t *testing.T) {
	path := setupTestDeploymentPath(t)
	cfg := config.Config{DeploymentPath: path}
	err := establishSetupIncusAuthority(
		context.Background(), &cfg, setupOptions{}, []core.SandboxProvider{core.SandboxProviderIncus},
		filepath.Join(t.TempDir(), "pending.json"), true, "", nil, nil,
	)
	if err != nil || cfg.Incus == nil || cfg.Incus.Endpoint != "unix://"+incus.DefaultUnixSocket {
		t.Fatalf("in-memory authority=%#v error=%v", cfg.Incus, err)
	}
	stored, found, err := deployment.Load(path)
	if err != nil || !found || stored.Incus != nil {
		t.Fatalf("pre-readiness stored authority=%#v found=%t error=%v", stored.Incus, found, err)
	}
	if err := persistGuidedIncusAuthority(&cfg, guidedIncusNetwork); err != nil {
		t.Fatal(err)
	}
	stored, found, err = deployment.Load(path)
	if err != nil || !found || stored.Incus == nil || *stored.Incus != *cfg.Incus {
		t.Fatalf("post-readiness stored authority=%#v found=%t error=%v", stored.Incus, found, err)
	}
}

func TestSetupRemoteIncusEnrollmentRequiresThePersistenceCommitMarker(t *testing.T) {
	path := setupTestDeploymentPath(t)
	authority := *testRemoteIncusAuthority(t)
	cfg := config.Config{DeploymentPath: path}
	pendingPath := filepath.Join(t.TempDir(), "incus-enrollment.json")
	enrolled := func(_ context.Context, request incus.EnrollmentRequest) (deployment.Incus, error) {
		if request.DeploymentPath != path || request.PendingPath != pendingPath || request.TrustToken != "one-use-offer" {
			t.Fatalf("enrollment request=%#v", request)
		}
		if err := deployment.RetainIncus(path, authority); err != nil {
			t.Fatal(err)
		}
		return authority, nil
	}
	err := establishSetupIncusAuthority(
		context.Background(), &cfg,
		setupOptions{IncusTrustOfferFile: "-"}, []core.SandboxProvider{core.SandboxProviderIncus},
		pendingPath, false, "", strings.NewReader("one-use-offer\n"), enrolled,
	)
	if err != nil || cfg.Incus == nil || *cfg.Incus != authority {
		t.Fatalf("accepted authority=%#v error=%v", cfg.Incus, err)
	}

	summary, err := setupIncusAuthoritySummary(authority)
	if err != nil || !strings.Contains(summary, authority.Endpoint) || !strings.Contains(summary, "authority ") || !strings.Contains(summary, "client ") {
		t.Fatalf("summary=%q error=%v", summary, err)
	}
	for _, secret := range []string{"one-use-offer", strings.TrimSpace(authority.ServerCertificate), strings.TrimSpace(authority.ClientCertificate), strings.TrimSpace(authority.ClientPrivateKey)} {
		if strings.Contains(summary, secret) {
			t.Fatalf("safe summary disclosed retained secret or PEM: %q", summary)
		}
	}

	replay := config.Config{DeploymentPath: path, Incus: &authority}
	called := false
	err = establishSetupIncusAuthority(
		context.Background(), &replay,
		setupOptions{IncusTrustOfferFile: filepath.Join(t.TempDir(), "removed-offer")}, []core.SandboxProvider{core.SandboxProviderIncus},
		pendingPath, false, "", nil, func(_ context.Context, request incus.EnrollmentRequest) (deployment.Incus, error) {
			called = true
			if request.DeploymentPath != path || request.PendingPath != pendingPath || request.TrustToken != "" {
				t.Fatalf("accepted reconciliation request=%#v", request)
			}
			return authority, nil
		},
	)
	if err != nil || !called {
		t.Fatalf("accepted replay called enrollment=%t error=%v", called, err)
	}
	conflict := authority
	conflict.Endpoint = "https://100.100.10.21:8443"
	err = establishSetupIncusAuthority(
		context.Background(), &replay, setupOptions{}, []core.SandboxProvider{core.SandboxProviderIncus},
		pendingPath, false, "", nil, func(_ context.Context, request incus.EnrollmentRequest) (deployment.Incus, error) {
			if request.TrustToken != "" {
				t.Fatalf("accepted replay included trust offer %q", request.TrustToken)
			}
			return conflict, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "different retained authority") {
		t.Fatalf("conflicting reconciliation error=%v", err)
	}

	localPath := setupTestDeploymentPath(t)
	local := deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	if err := deployment.RetainIncus(localPath, local); err != nil {
		t.Fatal(err)
	}
	localConfig := config.Config{DeploymentPath: localPath, Incus: &local}
	called = false
	err = establishSetupIncusAuthority(
		context.Background(), &localConfig, setupOptions{}, []core.SandboxProvider{core.SandboxProviderIncus},
		pendingPath, false, "", nil, func(_ context.Context, request incus.EnrollmentRequest) (deployment.Incus, error) {
			called = true
			if request.TrustToken != "" {
				t.Fatalf("local reconciliation included trust offer %q", request.TrustToken)
			}
			return authority, nil
		},
	)
	if !called || err == nil || !strings.Contains(err.Error(), "different retained authority") {
		t.Fatalf("cross-shape reconciliation called=%t error=%v", called, err)
	}

	missingPath := setupTestDeploymentPath(t)
	missing := config.Config{DeploymentPath: missingPath}
	err = establishSetupIncusAuthority(
		context.Background(), &missing,
		setupOptions{IncusTrustOfferFile: "-"}, []core.SandboxProvider{core.SandboxProviderIncus},
		filepath.Join(t.TempDir(), "pending.json"), false, "", strings.NewReader("one-use-offer"),
		func(context.Context, incus.EnrollmentRequest) (deployment.Incus, error) { return authority, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "not committed") {
		t.Fatalf("missing commit marker error=%v", err)
	}
}

func TestSetupRemoteGatewayRouteMatrix(t *testing.T) {
	local := &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	remote := testRemoteIncusAuthority(t)
	incusPlan := guidedProfilePlan{Provider: core.SandboxProviderIncus}
	e2bPlan := guidedProfilePlan{Provider: core.SandboxProviderE2B}
	for _, test := range []struct {
		name      string
		plans     []guidedProfilePlan
		authority *deployment.Incus
		want      bool
	}{
		{name: "local Incus", plans: []guidedProfilePlan{incusPlan}, authority: local},
		{name: "remote Incus", plans: []guidedProfilePlan{incusPlan}, authority: remote, want: true},
		{name: "E2B", plans: []guidedProfilePlan{e2bPlan}, want: true},
		{name: "local Incus and E2B", plans: []guidedProfilePlan{incusPlan, e2bPlan}, authority: local, want: true},
		{name: "remote Incus and E2B", plans: []guidedProfilePlan{incusPlan, e2bPlan}, authority: remote, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := setupNeedsStableHTTPS(test.plans, test.authority)
			if err != nil || got != test.want {
				t.Fatalf("stable HTTPS=%t want=%t error=%v", got, test.want, err)
			}
		})
	}
	if _, err := setupNeedsStableHTTPS([]guidedProfilePlan{incusPlan}, nil); err == nil {
		t.Fatal("Incus route planning accepted no deployment authority")
	}
}

func TestSetupRemoteIncusUsesThePlannedGatewayWithoutRetargetingExistingProfiles(t *testing.T) {
	remote := testRemoteIncusAuthority(t)
	const gatewayURL = "https://models.dorf.example/v1"
	if got, err := guidedIncusGatewayURL(remote, gatewayURL, ""); err != nil || got != gatewayURL {
		t.Fatalf("remote Gateway URL=%q error=%v", got, err)
	}
	local := &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	if got, err := guidedIncusGatewayURL(local, gatewayURL, "10.44.0.1"); err != nil || got != "http://10.44.0.1:8317/v1" {
		t.Fatalf("local Gateway URL=%q error=%v", got, err)
	}
	existing := &core.SandboxProfile{Name: "remote-codex", IncusGatewayURL: "https://old.example/v1"}
	plans := []guidedProfilePlan{{Provider: core.SandboxProviderIncus, Name: existing.Name, Existing: existing}}
	if err := validateGuidedIncusGatewayProfiles(plans, remote, gatewayURL); err == nil || existing.IncusGatewayURL != "https://old.example/v1" {
		t.Fatalf("existing Incus profile was accepted or changed: profile=%#v error=%v", existing, err)
	}
	existing.IncusGatewayURL = gatewayURL
	if err := validateGuidedIncusGatewayProfiles(plans, remote, gatewayURL); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteGatewayPlanningReadsIncusAndE2BRoutes(t *testing.T) {
	const gatewayURL = "https://models.dorf.example/v1"
	incusProfile := &core.SandboxProfile{Name: "remote", IncusGatewayURL: gatewayURL}
	plans := []guidedProfilePlan{{Provider: core.SandboxProviderIncus, Name: incusProfile.Name, Existing: incusProfile}}
	plan, err := planRemoteGateway(context.Background(), setupOptions{}, plans, setupPresenter{}, fakeDNSResolver{}, ownedCloudflareEndpoints{})
	if err != nil || plan.URL != gatewayURL || plan.Mode != setupGatewayExisting {
		t.Fatalf("Incus Gateway plan=%#v error=%v", plan, err)
	}
	e2bProfile := &core.SandboxProfile{Name: "cloud", E2BGatewayURL: "https://other.example/v1"}
	plans = append(plans, guidedProfilePlan{Provider: core.SandboxProviderE2B, Name: e2bProfile.Name, Existing: e2bProfile})
	if _, err := planRemoteGateway(context.Background(), setupOptions{}, plans, setupPresenter{}, fakeDNSResolver{}, ownedCloudflareEndpoints{}); err == nil || !strings.Contains(err.Error(), "different model Gateway URLs") {
		t.Fatalf("mixed Gateway conflict error=%v", err)
	}
}

func setupTestDeploymentPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config", "deployment.json")
	err := deployment.Save(path, deployment.Config{Database: deployment.Database{
		Host: "127.0.0.1", Port: 5432, Name: "dorf", User: "dorf", Password: "secret",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return path
}
