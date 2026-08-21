package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

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

func TestProfileUpdatePatchContainsOnlyExplicitFlags(t *testing.T) {
	var stderr strings.Builder
	patch, err := parseSandboxProfilePatch(context.Background(), "update managed", core.SandboxProviderE2B, []string{
		"--gateway-url", "https://replacement.example/v1", "--allow-internet=false",
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if patch.E2BGatewayURL == nil || *patch.E2BGatewayURL != "https://replacement.example/v1" ||
		patch.E2BAllowInternet == nil || *patch.E2BAllowInternet || patch.E2BArtifact != nil ||
		patch.E2BSandboxTimeout != nil || patch.Harness != nil ||
		patch.IncusArtifact != nil || patch.IncusNetwork != nil || patch.IncusDiskSize != nil {
		t.Fatalf("patch contains omitted or incorrect fields: %#v", patch)
	}
	if _, err := parseSandboxProfilePatch(context.Background(), "update managed", core.SandboxProviderE2B, nil, &stderr); err == nil || !strings.Contains(err.Error(), "at least one field flag") {
		t.Fatalf("empty patch error=%v", err)
	}
	if _, err := parseSandboxProfilePatch(context.Background(), "update managed", core.SandboxProviderE2B, []string{"--sandbox-provider", "e2b"}, &stderr); err == nil {
		t.Fatal("profile update accepted a provider change")
	}
	if _, err := parseSandboxProfilePatch(context.Background(), "update managed", core.SandboxProviderE2B, []string{"--image", "dorf"}, &stderr); err == nil || !strings.Contains(err.Error(), "does not accept Incus fields") {
		t.Fatalf("E2B Incus-field error=%v", err)
	}
}

func TestProviderGatewayStatusDistinguishesHistoricalVerificationFromCurrentReachability(t *testing.T) {
	verifiedAt := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	profile := core.SandboxProfile{
		Name: "managed", Provider: core.SandboxProviderE2B, E2BGatewayURL: "https://gateway.example/v1",
		Verification: &core.ProfileVerification{
			ContractVersion: core.BaseProfileContract, ProbeCompletedAt: verifiedAt, CleanedAt: verifiedAt.Add(time.Second),
		},
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
	local := newProviderGatewayStatusView(core.SandboxProfile{
		Name: "local", Provider: core.SandboxProviderIncus, IncusNetwork: "incusbr0",
		Verification: &core.ProfileVerification{
			ContractVersion: core.BaseProfileContract, ProbeCompletedAt: verifiedAt, CleanedAt: verifiedAt.Add(time.Second),
		},
	}, "personal-chatgpt", nil, nil)
	if !local.Ready || local.SandboxPath.Status != "historical" || !strings.Contains(local.SandboxPath.Detail, "no Sandbox was created") {
		t.Fatalf("local status overclaimed live Sandbox reachability: %#v", local)
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
  "hostname": "dorf.example.com",
  "origin": "http://127.0.0.1:8317",
  "probe_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := core.SandboxProfile{Provider: core.SandboxProviderE2B, E2BGatewayURL: "https://dorf.example.com/v1"}
	g, err := remoteGatewayForProviderStatus(config.Config{GatewayStatePath: statePath}, profile)
	if err != nil || g.Client == nil || g.DeploymentProbeURL != "https://dorf.example.com/.dorf/probe/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
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
		"--yes", "--ai-connection", "personal-chatgpt",
		"--sandbox-provider", "incus", "--sandbox-provider", "e2b",
		"--harness", "codex", "--e2b-template", "dorf:exact-build",
		"--gateway-url", "https://gateway.example/v1", "--allow-internet",
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !options.Yes || options.Connection != "personal-chatgpt" || options.ProfileName != "" ||
		options.Harness != "codex" || options.E2BTemplate != "dorf:exact-build" ||
		options.GatewayURL != "https://gateway.example/v1" || !options.AllowInternet ||
		!containsSandboxProvider(options.SandboxProviders, core.SandboxProviderIncus) ||
		!containsSandboxProvider(options.SandboxProviders, core.SandboxProviderE2B) {
		t.Fatalf("options=%#v", options)
	}
	if _, err := parseSetupOptions([]string{"--profile", "local-codex", "--sandbox-provider", "incus", "--sandbox-provider", "e2b"}, &stderr); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("multi-provider named profile error=%v", err)
	}
	if _, err := parseSetupOptions([]string{"--sandbox-provider", "incus", "--ai-connection", "personal-chatgpt", "--connection-auth", "chatgpt"}, &stderr); err == nil || !strings.Contains(err.Error(), "either") {
		t.Fatalf("conflicting connection input error=%v", err)
	}
	if _, err := parseSetupOptions([]string{"--database", "native"}, &stderr); err == nil {
		t.Fatal("removed database selection was accepted")
	}
	for _, removed := range []string{"--provider", "--connection"} {
		if _, err := parseSetupOptions([]string{removed, "legacy"}, &stderr); err == nil {
			t.Fatalf("removed setup flag %s was accepted", removed)
		}
	}
	if _, err := parseSetupOptions([]string{"--sandbox-provider", "unknown"}, &stderr); err == nil {
		t.Fatal("unknown Sandbox provider was accepted")
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
		got := unambiguousSetupConnection(func(name string) error {
			if test.ready[name] {
				return nil
			}
			return context.Canceled
		})
		if got != test.want {
			t.Fatalf("ready connections=%v got=%q want=%q", test.ready, got, test.want)
		}
	}
	selected := []core.SandboxProvider{}
	presenter := setupPresenter{}
	view := presenter.ProviderGroup(&selected, true).Content()
	for _, label := range []string{"Local · Incus", "Hardware-isolated Linux VMs on this machine", "Cloud · E2B", "Managed Linux VMs"} {
		if !strings.Contains(view, label) {
			t.Fatalf("provider selector omitted %q:\n%s", label, view)
		}
	}
	keymap := setupKeyMap()
	if keymap.Select.Filter.Enabled() {
		t.Fatal("short setup choices should not advertise filtering")
	}
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
	if !strings.Contains(styles.Focused.SelectSelector.String(), "› ") ||
		!strings.Contains(styles.Focused.MultiSelectSelector.String(), "› ") {
		t.Fatalf("setup menus do not share the same selector")
	}
	if focused := styles.Focused.FocusedButton.Render("Continue"); !strings.Contains(focused, "›") {
		t.Fatalf("focused confirmation is not structurally marked: %q", focused)
	}
	if blurred := styles.Focused.BlurredButton.Render("Cancel"); strings.Contains(blurred, "›") {
		t.Fatalf("inactive confirmation looks focused: %q", blurred)
	}
	if strings.Contains(view, "    Hardware-isolated") || strings.Contains(view, "    Managed Linux") {
		t.Fatalf("provider descriptions carry embedded indentation:\n%s", view)
	}
	withoutKVM := presenter.ProviderGroup(&selected, false)
	if !strings.Contains(withoutKVM.Content(), "[—] Local · Incus") ||
		!strings.Contains(withoutKVM.Content(), "Unavailable on this machine · KVM not detected") ||
		strings.Contains(withoutKVM.Content(), "[ ] Local · Incus") {
		t.Fatalf("KVM-less provider selector is misleading:\n%s\n%s", withoutKVM.Header(), withoutKVM.Content())
	}
	for _, group := range []*huh.Group{
		presenter.HarnessGroup(new(string)),
		presenter.ConnectionGroup(new(setupConnectionMode)),
		presenter.CloudflareGatewayGroup(new(setupGatewayMode), "example.com"),
	} {
		if strings.TrimSpace(group.Header()) == "" || strings.TrimSpace(group.Content()) == "" {
			t.Fatalf("guided setup group is empty: header=%q content=%q", group.Header(), group.Content())
		}
	}
	harness := "codex"
	harnessForm := huh.NewForm(presenter.HarnessGroup(&harness)).WithKeyMap(setupKeyMap()).WithTheme(huh.ThemeFunc(setupTheme))
	model, _ := harnessForm.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	harnessContent := model.(*huh.Form).View()
	for _, text := range []string{"Codex", "OpenAI Codex Harness", "Pi", "Pi coding-agent Harness"} {
		if !strings.Contains(harnessContent, text) {
			t.Fatalf("Harness selector omitted %q:\n%s", text, harnessContent)
		}
	}
}

func TestGuidedGatewayPlanningReusesTheDurableProfileContract(t *testing.T) {
	existing := core.SandboxProfile{
		Name: "cloud-codex", Provider: core.SandboxProviderE2B, Harness: "codex",
		Artifact: "dorf:exact-build", E2BGatewayURL: "https://gateway.example/v1",
	}
	plan, err := planRemoteGateway(context.Background(), setupOptions{}, []guidedProfilePlan{{
		Provider: core.SandboxProviderE2B, Name: existing.Name, Harness: existing.Harness, Existing: &existing,
	}}, setupPresenter{}, fakeDNSResolver{})
	if err != nil || plan.Mode != setupGatewayExisting || plan.URL != existing.E2BGatewayURL {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	_, err = planRemoteGateway(context.Background(), setupOptions{CloudflareHost: "other.example.com"}, []guidedProfilePlan{{
		Provider: core.SandboxProviderE2B, Name: existing.Name, Harness: existing.Harness, Existing: &existing,
	}}, setupPresenter{}, fakeDNSResolver{records: map[string][]*net.NS{
		"example.com": {{Host: "cash.ns.cloudflare.com."}, {Host: "lana.ns.cloudflare.com."}},
	}})
	if err == nil || !strings.Contains(err.Error(), "update the profile explicitly") {
		t.Fatalf("conflicting Gateway error=%v", err)
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
		"example.com": {{Host: "cash.ns.cloudflare.com."}, {Host: "lana.ns.cloudflare.com."}},
	}}
	plan, err := planRemoteGateway(context.Background(), setupOptions{CloudflareHost: "dorf.example.com"}, nil, setupPresenter{}, cloudflare)
	if err != nil || plan.Mode != setupGatewayCloudflare || plan.URL != "https://dorf.example.com/v1" {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}

	nonCloudflare := fakeDNSResolver{records: map[string][]*net.NS{
		"example.com": {{Host: "ns1.other-provider.example."}},
	}}
	if _, err := planRemoteGateway(context.Background(), setupOptions{CloudflareHost: "dorf.example.com"}, nil, setupPresenter{}, nonCloudflare); err == nil || !strings.Contains(err.Error(), "not Cloudflare") {
		t.Fatalf("non-Cloudflare error=%v", err)
	}

	occupied := cloudflare
	occupied.addresses = map[string][]net.IPAddr{"dorf.example.com": {{IP: net.ParseIP("192.0.2.1")}}}
	if _, err := planRemoteGateway(context.Background(), setupOptions{CloudflareHost: "dorf.example.com"}, nil, setupPresenter{}, occupied); err == nil || !strings.Contains(err.Error(), "already resolves") {
		t.Fatalf("occupied hostname error=%v", err)
	}
}

func TestGuidedGatewayBindPreservesExistingLocalProfiles(t *testing.T) {
	network, err := gatewayIncusNetwork([]core.SandboxProfile{{
		Name: "local-codex", Provider: core.SandboxProviderIncus, IncusNetwork: "incusbr0",
	}}, []guidedProfilePlan{{Provider: core.SandboxProviderE2B, Name: "cloud-codex"}})
	if err != nil || network != "incusbr0" {
		t.Fatalf("network=%q err=%v", network, err)
	}
	_, err = gatewayIncusNetwork([]core.SandboxProfile{{Provider: core.SandboxProviderIncus, IncusNetwork: "incusbr0"}}, []guidedProfilePlan{{
		Provider: core.SandboxProviderIncus, Name: "local-pi", Existing: &core.SandboxProfile{Provider: core.SandboxProviderIncus, IncusNetwork: "otherbr0"},
	}})
	if err == nil || !strings.Contains(err.Error(), "multiple networks") {
		t.Fatalf("ambiguous network error=%v", err)
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
}

func TestSandboxForProfileSelectsOneConcreteAdapter(t *testing.T) {
	local, err := sandboxForProfile(config.Config{Workspace: "/workspace/job"}, core.SandboxProfile{
		Name: "local", Provider: core.SandboxProviderIncus, Artifact: strings.Repeat("a", 64),
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := local.(incus.Adapter); !ok {
		t.Fatalf("local adapter = %T", local)
	}
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

func TestWorkflowHistorySortsNaturalFactsAndIncludesRunsAndRevisions(t *testing.T) {
	base := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	job := core.Job{ID: "job-1", AdmittedAt: base, SandboxProfile: "e2b"}
	mainSandbox := core.MainSandboxName(job.ID)
	entries := workflowHistory(coding.Snapshot{
		Job: coding.Job{Job: job},
		Deliveries: []core.Delivery{{
			Message:  core.Message{ID: "message-1", Sequence: 1, FromKind: core.MessageFromHuman, AdmittedAt: base.Add(time.Second)},
			AgentRun: core.AgentRun{ID: "run-secret", MessageID: "message-1", Role: "implement", State: core.AgentRunCompleted, InputRevision: "revision-0", StartedAt: base.Add(4 * time.Second), FinishedAt: base.Add(5 * time.Second)},
		}},
		Actions: []core.Action{
			{ID: "action-secret", Kind: core.ActionSandboxCreate, Scope: mainSandbox, State: core.ActionSucceeded, CreatedAt: base.Add(2 * time.Second), SettledAt: base.Add(3 * time.Second)},
			{Kind: coding.ActionGitHubPullRequest, State: core.ActionSucceeded, Scope: "revision-1", CreatedAt: base.Add(7 * time.Second), SettledAt: base.Add(8 * time.Second)},
		},
		Revisions: []coding.Revision{
			{Generation: 0, OID: "revision-0", ObservedAt: base},
			{Generation: 1, OID: "revision-1", ComparisonBase: "revision-0", ObservedAt: base.Add(6 * time.Second)},
		},
		Evidence: []core.Evidence{{ID: "evidence-secret", Kind: "git-revision", Revision: "revision-1", FinishedAt: base.Add(6500 * time.Millisecond)}},
		Proposal: &coding.Proposal{Number: 42, ProposedRevision: "revision-1"},
	})
	for i := 1; i < len(entries); i++ {
		if entries[i].At.Before(entries[i-1].At) {
			t.Fatalf("history is not chronological: %#v", entries)
		}
	}
	if len(entries) != 12 {
		t.Fatalf("history has %d entries, want 12 human events: %#v", len(entries), entries)
	}
	var story strings.Builder
	for _, entry := range entries {
		story.WriteString(entry.Text + "\n")
	}
	for _, want := range []string{
		"Job admitted",
		"Starting Revision accepted · revision-0",
		"Message 1 received from Human",
		"Creating primary Sandbox · E2B",
		"Primary Sandbox ready · E2B · 1s",
		"Implementation agent started",
		"Implementation agent completed · 1s",
		"Revision generation 1 observed · revision-1",
		"Git revision evidence recorded · Revision revision-1",
		"Creating pull request",
		"Pull request created · 1s",
		"Pull request #42 ready · Revision revision-1",
	} {
		if !strings.Contains(story.String(), want) {
			t.Fatalf("history is missing factual token %q:\n%s", want, story.String())
		}
	}
	for _, plumbing := range []string{"action-secret", "run-secret", "message-1", "evidence-secret"} {
		if strings.Contains(story.String(), plumbing) {
			t.Fatalf("human history leaked plumbing %q:\n%s", plumbing, story.String())
		}
	}
	abandoned := workflowHistory(coding.Snapshot{
		Job:     coding.Job{Job: core.Job{AdmittedAt: base}},
		Outcome: &coding.Outcome{Kind: coding.OutcomeAbandoned, ObservedAt: base.Add(time.Second)},
	})
	last := abandoned[len(abandoned)-1]
	if !strings.Contains(last.Text, "Outcome Abandoned") || strings.Contains(last.Text, "GitHub") {
		t.Fatalf("pre-Proposal abandonment history = %#v", last)
	}
}

func TestRenderHistoryGroupsLocalDatesAndHidesFactCategories(t *testing.T) {
	first := time.Date(2026, 8, 18, 15, 31, 57, 0, time.Local)
	entries := []historyEntry{
		{At: first, Text: "Job admitted"},
		{At: first.Add(time.Minute), Text: "Creating primary Sandbox · Incus"},
		{At: first.Add(24 * time.Hour), Text: "Cleanup complete"},
	}
	var output strings.Builder
	renderHistory(&output, entries)
	got := output.String()
	for _, want := range []string{
		"  18 Aug 2026\n    15:31  Job admitted",
		"    15:32  Creating primary Sandbox · Incus",
		"  19 Aug 2026\n    15:31  Cleanup complete",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("human history is missing %q:\n%s", want, got)
		}
	}
	for _, machineCopy := range []string{"2026-08-18T", "Action       ", "AgentRun     "} {
		if strings.Contains(got, machineCopy) {
			t.Fatalf("human history exposed machine copy %q:\n%s", machineCopy, got)
		}
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

func TestRenderWorkflowExecutionAttentionLeadsToTruthfulRepair(t *testing.T) {
	job := core.Job{ID: "job-123", AdmissionOpen: true}
	execution := taskResultView{TaskID: "task-1", State: absurd.TaskFailed, LastError: "clone repository: DNS failed"}
	var output strings.Builder
	renderWorkflowExecutionAttention(&output, job, execution, "Clone repository")
	want := "  attention: workflow stopped\n" +
		"  operation: Clone repository\n" +
		"  reason: clone repository: DNS failed\n" +
		"  next: repair the cause, then run dorf retry job-123\n"
	if output.String() != want {
		t.Fatalf("attention output:\n%s\nwant:\n%s", output.String(), want)
	}

	output.Reset()
	renderWorkflowExecutionAttention(&output, core.Job{ID: "job-123", CleanupState: core.CleanupComplete}, execution, "Complete")
	if output.Len() != 0 {
		t.Fatalf("completed Job rendered non-actionable attention: %q", output.String())
	}

	output.Reset()
	renderWorkflowExecutionAttention(&output, core.Job{ID: "job-123", CleanupState: core.CleanupScheduled}, execution, "Deleting Sandbox")
	if got := output.String(); !strings.Contains(got, "attention: cleanup stopped") || !strings.Contains(got, "operation: Deleting Sandbox") || !strings.Contains(got, "dorf retry job-123") {
		t.Fatalf("failed cleanup attention:\n%s", got)
	}

	output.Reset()
	renderWorkflowExecutionAttention(&output, job, taskResultView{State: absurd.TaskRunning}, "Clone repository")
	if output.Len() != 0 {
		t.Fatalf("running task rendered failure attention: %q", output.String())
	}
}

func TestCompletedWorkflowAttentionOffersCleanupInsteadOfRetry(t *testing.T) {
	job := core.Job{ID: "job-123", AdmissionOpen: true, CleanupState: core.CleanupPending, WorkflowAttention: "E2B template is unavailable"}
	execution := taskResultView{TaskID: "task-1", State: absurd.TaskCompleted}
	var output strings.Builder
	renderWorkflowAttentionRecovery(&output, job, execution)
	if got := output.String(); got != "  next: run dorf cleanup job-123 to release resources\n" || strings.Contains(got, "retry") {
		t.Fatalf("recovery output=%q", got)
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

func TestRemoteGitAdmissionRejectsOnlyOfflineE2BProfiles(t *testing.T) {
	offline := core.SandboxProfile{Name: "cloud-codex", Provider: core.SandboxProviderE2B}
	if err := requireRemoteGitAccess(offline); err == nil || !strings.Contains(err.Error(), "use --local-repo") || !strings.Contains(err.Error(), "update and reverify") {
		t.Fatalf("offline E2B error=%v", err)
	}
	for _, profile := range []core.SandboxProfile{
		{Name: "cloud-online", Provider: core.SandboxProviderE2B, E2BAllowInternet: true},
		{Name: "local", Provider: core.SandboxProviderIncus},
	} {
		if err := requireRemoteGitAccess(profile); err != nil {
			t.Fatalf("profile %#v rejected remote Git: %v", profile, err)
		}
	}
}
