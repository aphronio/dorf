package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/huh/v2"
	cloudflareapp "github.com/aphronio/dorf/internal/cloudflare"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/postgres"
	profileapp "github.com/aphronio/dorf/internal/profile"
	releaseapp "github.com/aphronio/dorf/internal/release"
	providerapi "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/version"
)

const (
	guidedIncusNetwork  = "incusbr0"
	guidedIncusDiskSize = "40GiB"
	guidedE2BTemplate   = "dorf/standard:248684ca-1b17-4251-aaf9-6aac792fa806"
)

type guidedProfilePlan struct {
	Provider core.SandboxProvider
	Name     string
	Harness  string
	Existing *core.SandboxProfile
}

type guidedGatewayPlan struct {
	Mode               setupGatewayMode
	URL                string
	Domain             string
	ControlURL         string
	ControlHostname    string
	ModelHostname      string
	ReplaceExistingDNS bool
}

type ownedCloudflareEndpoints struct {
	ControlHostname string
	ModelHostname   string
}

type guidedSetupPrepared struct {
	Gateway      gateway.Gateway
	Connection   string
	ProfilePlans []guidedProfilePlan
	GatewayPlan  guidedGatewayPlan
	GatewayURL   string
	ControlURL   string
	PrivateIPv4  string
}

type guidedRemoteGatewayPrepared struct {
	ProfilePlans []guidedProfilePlan
	Plan         guidedGatewayPlan
	URL          string
	ControlURL   string
}

// prepareGuidedSetup performs every offline or explicitly interactive setup
// operation that must affect the final Compose project. It deliberately stops
// before live Gateway, public-route, and Sandbox profile readiness.
func prepareGuidedSetup(ctx context.Context, store postgres.Store, cfg *config.Config, options setupOptions, providers []core.SandboxProvider, presenter setupPresenter, stdout, stderr io.Writer) (guidedSetupPrepared, error) {
	presenter.Section("Agent configuration")
	harness, err := setupHarness(ctx, options, presenter)
	if err != nil {
		return guidedSetupPrepared{}, err
	}
	profilePlans, err := planGuidedProfiles(ctx, store, options, providers, harness, cfg.Incus)
	if err != nil {
		return guidedSetupPrepared{}, err
	}
	stableHTTPS, err := setupStableHTTPSRequirement(options, profilePlans, cfg.Incus)
	if err != nil {
		return guidedSetupPrepared{}, err
	}
	if err := setupGuidedIncusReadiness(ctx, profilePlans, cfg.Incus); err != nil {
		return guidedSetupPrepared{}, err
	}
	g := gateway.Gateway{StatePath: cfg.GatewayStatePath}
	preparedAddress, gatewayPrepared, err := g.PreparedComposePublishAddress()
	if err != nil {
		return guidedSetupPrepared{}, err
	}
	bind, privateBridge, err := setupGatewayBind(ctx, store, profilePlans, cfg.Incus, preparedAddress, gatewayPrepared)
	if err != nil {
		return guidedSetupPrepared{}, err
	}
	if err := persistGuidedIncusAuthority(cfg, privateBridge); err != nil {
		return guidedSetupPrepared{}, err
	}
	g.PrivateBridge = privateBridge
	g.InternalDialOrigin = "http://" + bind + ":8317"
	connection, err := setupAIConnection(ctx, g, bind, options, presenter)
	if err != nil {
		return guidedSetupPrepared{}, err
	}
	presenter.Ready("OpenAI access", connection)

	prepared := guidedSetupPrepared{
		Gateway: g, Connection: connection, ProfilePlans: profilePlans,
		PrivateIPv4: bind,
	}
	if privateBridge == "" {
		prepared.PrivateIPv4 = ""
	}
	remote, err := prepareGuidedRemoteGateway(ctx, store, cfg, options, providers, profilePlans, stableHTTPS, presenter, stdout, stderr)
	if err != nil {
		return guidedSetupPrepared{}, err
	}
	prepared.ProfilePlans = remote.ProfilePlans
	prepared.GatewayPlan = remote.Plan
	prepared.GatewayURL = remote.URL
	prepared.ControlURL = remote.ControlURL
	return prepared, nil
}

func prepareGuidedRemoteGateway(
	ctx context.Context,
	store postgres.Store,
	cfg *config.Config,
	options setupOptions,
	providers []core.SandboxProvider,
	profilePlans []guidedProfilePlan,
	stableHTTPS bool,
	presenter setupPresenter,
	stdout, stderr io.Writer,
) (guidedRemoteGatewayPrepared, error) {
	prepared := guidedRemoteGatewayPrepared{ProfilePlans: profilePlans}
	if containsSandboxProvider(providers, core.SandboxProviderE2B) {
		if err := setupE2BCredential(ctx, cfg, options, presenter); err != nil {
			return guidedRemoteGatewayPrepared{}, err
		}
		presenter.Ready("E2B access", "Project credential verified on this host")
	}
	if !stableHTTPS {
		return prepared, nil
	}
	ownedEndpoints, err := currentOwnedCloudflareEndpoints(cfg.GatewayStatePath)
	if err != nil {
		return guidedRemoteGatewayPrepared{}, err
	}
	prepared.Plan, err = planRemoteGateway(ctx, options, profilePlans, presenter, net.DefaultResolver, ownedEndpoints)
	if err != nil {
		return guidedRemoteGatewayPrepared{}, err
	}
	if err := validateGuidedIncusGatewayProfiles(profilePlans, cfg.Incus, prepared.Plan.URL); err != nil {
		return guidedRemoteGatewayPrepared{}, err
	}
	prepared.ProfilePlans, err = retargetGuidedE2BProfiles(ctx, store, profilePlans, prepared.Plan.URL)
	if err != nil {
		return guidedRemoteGatewayPrepared{}, err
	}
	prepared.URL, err = prepareRemoteGateway(ctx, cfg.GatewayStatePath, prepared.Plan, presenter, stdout, stderr)
	if err != nil {
		return guidedRemoteGatewayPrepared{}, err
	}
	prepared.ControlURL = prepared.Plan.ControlURL
	return prepared, nil
}

func setupStableHTTPSRequirement(options setupOptions, plans []guidedProfilePlan, authority *deployment.Incus) (bool, error) {
	stableHTTPS, err := setupNeedsStableHTTPS(plans, authority)
	if err != nil {
		return false, err
	}
	if setupRemoteGatewayRequested(options) && !stableHTTPS {
		return false, fmt.Errorf("Gateway and Cloudflare setup flags require E2B or a remote HTTPS Incus authority")
	}
	return stableHTTPS, nil
}

func setupNeedsStableHTTPS(plans []guidedProfilePlan, authority *deployment.Incus) (bool, error) {
	stableHTTPS := false
	for _, plan := range plans {
		switch plan.Provider {
		case core.SandboxProviderE2B:
			stableHTTPS = true
		case core.SandboxProviderIncus:
			if authority == nil {
				return false, fmt.Errorf("Incus endpoint is not configured in the Dorf Deployment")
			}
			if strings.HasPrefix(authority.Endpoint, "https://") {
				stableHTTPS = true
			}
		}
	}
	return stableHTTPS, nil
}

func setupRemoteGatewayRequested(options setupOptions) bool {
	return options.GatewayURL != "" || options.CloudflareDomain != "" || options.CloudflareControlHostname != "" ||
		options.CloudflareModelHostname != "" || options.ReplaceCloudflareDNS
}

func guidedIncusReadinessScope(plans []guidedProfilePlan, authority *deployment.Incus) (string, string, bool, error) {
	for _, plan := range plans {
		if plan.Provider != core.SandboxProviderIncus {
			continue
		}
		if plan.Existing != nil {
			return plan.Existing.IncusProject, plan.Existing.IncusStoragePool, true, nil
		}
		project, storagePool, _, err := guidedIncusProfileTarget(authority)
		return project, storagePool, true, err
	}
	return "", "", false, nil
}

func setupGuidedIncusReadiness(ctx context.Context, plans []guidedProfilePlan, authority *deployment.Incus) error {
	return setupGuidedIncusReadinessWith(ctx, plans, authority, setupIncusEndpointReadiness)
}

func setupGuidedIncusReadinessWith(
	ctx context.Context,
	plans []guidedProfilePlan,
	authority *deployment.Incus,
	probe func(context.Context, *deployment.Incus, string, string) error,
) error {
	project, storagePool, selected, err := guidedIncusReadinessScope(plans, authority)
	if err != nil {
		return err
	}
	if !selected {
		return nil
	}
	if err := validateGuidedExistingIncusAuthorities(plans, authority); err != nil {
		return err
	}
	return probe(ctx, authority, project, storagePool)
}

func validateGuidedExistingIncusAuthorities(plans []guidedProfilePlan, authority *deployment.Incus) error {
	for _, plan := range plans {
		if plan.Provider != core.SandboxProviderIncus || plan.Existing == nil {
			continue
		}
		if err := validateIncusProfileEndpointAuthority(authority, *plan.Existing); err != nil {
			return err
		}
	}
	return nil
}

func hasGuidedIncusProfileNeedingLocalDefinition(plans []guidedProfilePlan) bool {
	for _, plan := range plans {
		if plan.Provider == core.SandboxProviderIncus && plan.Existing == nil {
			return true
		}
	}
	return false
}

func persistGuidedIncusAuthority(cfg *config.Config, privateBridge string) error {
	if privateBridge == "" {
		return nil
	}
	if cfg.Incus == nil {
		return incusSetupReadinessError{cause: fmt.Errorf("Incus endpoint is not configured in the Dorf Deployment")}
	}
	if !strings.HasPrefix(cfg.Incus.Endpoint, "unix://") {
		return incusSetupReadinessError{cause: fmt.Errorf("a private Incus bridge requires one local Unix endpoint authority")}
	}
	if err := deployment.RetainIncus(cfg.DeploymentPath, *cfg.Incus); err != nil {
		return err
	}
	stored, found, err := deployment.Load(cfg.DeploymentPath)
	if err != nil {
		return err
	}
	if !found || stored.Incus == nil || *stored.Incus != *cfg.Incus {
		return fmt.Errorf("Incus authority was not committed to the Dorf Deployment")
	}
	return nil
}

// completeGuidedSetup resumes immediately after the final Compose
// reconciliation. Every operation here requires the live deployment.
func completeGuidedSetup(ctx context.Context, store postgres.Store, cfg config.Config, options setupOptions, prepared guidedSetupPrepared, presenter setupPresenter) error {
	if prepared.GatewayURL != "" {
		if err := finalizeRemoteGateway(ctx, prepared.Gateway, prepared.GatewayPlan, cfg.GatewayStatePath, presenter); err != nil {
			return err
		}
		if prepared.ControlURL != "" {
			presenter.Ready("Dorf API", prepared.ControlURL)
		}
		presenter.Ready("Model Gateway", prepared.GatewayURL)
	}
	presenter.Section("Sandbox profiles")
	profiles, err := setupProfiles(ctx, store, cfg, options, prepared.ProfilePlans, prepared.GatewayURL, prepared.PrivateIPv4, presenter)
	if err != nil {
		return err
	}
	defaultProfile, err := setupDefaultProfile(ctx, store, profiles, options.Yes, presenter)
	if err != nil {
		return err
	}
	presenter.Ready("Default profile", defaultProfile.Name+" · "+string(defaultProfile.Provider)+" · "+defaultProfile.Harness)
	return nil
}

func setupHarness(ctx context.Context, options setupOptions, presenter setupPresenter) (string, error) {
	harness := strings.TrimSpace(options.Harness)
	if harness == "" && presenter.interactive && !options.Yes {
		harness = "codex"
		if err := presenter.RunForm(ctx, presenter.HarnessGroup(&harness)); err != nil {
			return "", err
		}
	}
	if harness == "" {
		harness = "codex"
	}
	if harness != "codex" && harness != "pi" {
		return "", fmt.Errorf("guided setup Harness must be codex or pi")
	}
	return harness, nil
}

func planGuidedProfiles(ctx context.Context, store postgres.Store, options setupOptions, providers []core.SandboxProvider, harness string, authority *deployment.Incus) ([]guidedProfilePlan, error) {
	plans := make([]guidedProfilePlan, 0, len(providers))
	for _, provider := range providers {
		name := options.ProfileName
		if name == "" {
			if provider == core.SandboxProviderIncus {
				name = guidedIncusProfileName(harness, authority)
			} else {
				name = "cloud-" + harness
			}
		}
		if err := postgres.ValidateSandboxProfileIdentity(name, harness); err != nil {
			return nil, err
		}
		plan := guidedProfilePlan{Provider: provider, Name: name, Harness: harness}
		existing, err := store.SandboxProfile(ctx, name)
		switch {
		case err == nil:
			if existing.Provider != provider || existing.Harness != harness {
				return nil, fmt.Errorf("Sandbox profile %q already exists with %s/%s; choose another profile name", name, existing.Provider, existing.Harness)
			}
			if provider == core.SandboxProviderE2B {
				if options.E2BTemplate != "" && options.E2BTemplate != existing.Artifact {
					return nil, fmt.Errorf("Sandbox profile %q already uses E2B template %s; update the profile explicitly before changing it", name, existing.Artifact)
				}
				if options.AllowInternet && !existing.E2BAllowInternet {
					return nil, fmt.Errorf("Sandbox profile %q does not admit general internet egress; update the profile explicitly before changing it", name)
				}
			}
			copy := existing
			plan.Existing = &copy
		case errors.Is(err, postgres.ErrProfileNotFound):
		default:
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func guidedIncusProfileName(harness string, authority *deployment.Incus) string {
	if authority != nil && strings.HasPrefix(authority.Endpoint, "https://") {
		return "incus-" + harness
	}
	return "local-" + harness
}

type incusSetupReadinessError struct{ cause error }

func (e incusSetupReadinessError) Error() string { return e.cause.Error() }
func (e incusSetupReadinessError) Unwrap() error { return e.cause }

// setupIncusEndpointReadiness checks only an explicitly selected Incus
// topology. It never resolves a retained Profile route or creates a Sandbox.
func setupIncusEndpointReadiness(ctx context.Context, authority *deployment.Incus, project, storagePool string) error {
	if authority == nil {
		return incusSetupReadinessError{cause: fmt.Errorf("Incus endpoint is not configured in the Dorf Deployment")}
	}
	connection, _, err := incusProfileConnection(authority, project, storagePool)
	if err != nil {
		return incusSetupReadinessError{cause: err}
	}
	if strings.HasPrefix(connection.Endpoint, "unix://") {
		if err := checkLocalKVMAccess(); err != nil {
			return incusSetupReadinessError{cause: err}
		}
	}
	client, err := (incus.SDKClientFactory{}).Open(ctx, connection)
	if err != nil {
		return incusSetupReadinessError{cause: err}
	}
	client.Close()
	return nil
}

func setupGatewayBind(ctx context.Context, store postgres.Store, selected []guidedProfilePlan, authority *deployment.Incus, preparedAddress string, prepared bool) (string, string, error) {
	profiles, err := store.SandboxProfiles(ctx)
	if err != nil {
		return "", "", err
	}
	address, privateBridge, resolve, err := selectGuidedGatewayBind(profiles, selected, authority, preparedAddress, prepared)
	if err != nil || !resolve {
		return address, privateBridge, err
	}
	project, storagePool, err := gatewayIncusScope(profiles, selected, authority, privateBridge)
	if err != nil {
		return "", "", err
	}
	connection, _, err := incusProfileConnection(authority, project, storagePool)
	if err != nil {
		return "", "", incusSetupReadinessError{cause: err}
	}
	if strings.HasPrefix(connection.Endpoint, "unix://") {
		if err := checkLocalKVMAccess(); err != nil {
			return "", "", incusSetupReadinessError{cause: err}
		}
	}
	address, err = resolveIncusNetworkIPv4(ctx, authority, project, storagePool, privateBridge)
	if err != nil {
		return "", "", incusSetupReadinessError{cause: fmt.Errorf("resolve private Incus bridge %s: %w", privateBridge, err)}
	}
	return address, privateBridge, nil
}

func selectGuidedGatewayBind(profiles []core.SandboxProfile, selected []guidedProfilePlan, authority *deployment.Incus, preparedAddress string, prepared bool) (string, string, bool, error) {
	if err := validateGuidedExistingIncusAuthorities(selected, authority); err != nil {
		return "", "", false, err
	}
	bridgePlans := guidedIncusBridgePlans(selected, authority)
	network, err := gatewayIncusNetwork(profiles, bridgePlans)
	if err != nil {
		return "", "", false, err
	}
	for _, profile := range profiles {
		if _, direct := guidedIncusProfilePublishAddress(profile); direct && profile.IncusNetwork == network {
			if err := validateIncusProfileEndpointAuthority(authority, profile); err != nil {
				return "", "", false, err
			}
		}
	}
	profileAddress, bridgeRequired, err := guidedIncusBridgeAuthority(profiles, bridgePlans, network)
	if err != nil {
		return "", "", false, err
	}
	needsLocalDefinition, err := guidedNeedsLocalIncusDefinition(selected, authority)
	if err != nil {
		return "", "", false, err
	}
	if profileAddress != "" {
		address, err := selectGatewayPublishAddress("", preparedAddress, prepared, profileAddress, true)
		if err != nil {
			return "", "", false, err
		}
		privateBridge := ""
		if needsLocalDefinition {
			privateBridge = network
		}
		return address, privateBridge, false, nil
	}
	if !bridgeRequired || !needsLocalDefinition {
		address, err := selectGatewayPublishAddress("", preparedAddress, prepared, "", false)
		return address, "", false, err
	}
	return "", network, true, nil
}

func guidedIncusBridgePlans(selected []guidedProfilePlan, authority *deployment.Incus) []guidedProfilePlan {
	if authority == nil || !strings.HasPrefix(authority.Endpoint, "https://") {
		return selected
	}
	bridgePlans := make([]guidedProfilePlan, 0, len(selected))
	for _, plan := range selected {
		if plan.Provider != core.SandboxProviderIncus || plan.Existing != nil {
			bridgePlans = append(bridgePlans, plan)
		}
	}
	return bridgePlans
}

func guidedNeedsLocalIncusDefinition(selected []guidedProfilePlan, authority *deployment.Incus) (bool, error) {
	if !hasGuidedIncusProfileNeedingLocalDefinition(selected) {
		return false, nil
	}
	if authority == nil {
		return false, incusSetupReadinessError{cause: fmt.Errorf("Incus endpoint is not configured in the Dorf Deployment")}
	}
	return strings.HasPrefix(authority.Endpoint, "unix://"), nil
}

func gatewayIncusScope(profiles []core.SandboxProfile, selected []guidedProfilePlan, authority *deployment.Incus, network string) (string, string, error) {
	for _, plan := range selected {
		if plan.Provider != core.SandboxProviderIncus {
			continue
		}
		if plan.Existing == nil {
			project, storagePool, _, err := guidedIncusProfileTarget(authority)
			return project, storagePool, err
		}
		if _, direct := guidedIncusProfilePublishAddress(*plan.Existing); direct && plan.Existing.IncusNetwork == network {
			return plan.Existing.IncusProject, plan.Existing.IncusStoragePool, nil
		}
	}
	for _, profile := range profiles {
		if _, direct := guidedIncusProfilePublishAddress(profile); direct && profile.IncusNetwork == network {
			return profile.IncusProject, profile.IncusStoragePool, nil
		}
	}
	project, storagePool, _, err := guidedIncusProfileTarget(authority)
	return project, storagePool, err
}

func guidedIncusProfileTarget(authority *deployment.Incus) (string, string, string, error) {
	if authority == nil {
		return "", "", "", incusSetupReadinessError{cause: fmt.Errorf("Incus endpoint is not configured in the Dorf Deployment")}
	}
	if strings.HasPrefix(authority.Endpoint, "https://") {
		return incus.RemoteProjectName, incus.DefaultStoragePool, incus.RemoteNetworkName, nil
	}
	return incus.DefaultProject, incus.DefaultStoragePool, guidedIncusNetwork, nil
}

func resolveIncusNetworkIPv4(ctx context.Context, authority *deployment.Incus, project, storagePool, network string) (string, error) {
	connection, _, err := incusProfileConnection(authority, project, storagePool)
	if err != nil {
		return "", err
	}
	client, err := (incus.SDKClientFactory{}).Open(ctx, connection)
	if err != nil {
		return "", err
	}
	defer client.Close()
	raw, err := client.NetworkIPv4(ctx, network)
	if err != nil {
		return "", err
	}
	address := strings.TrimSpace(raw)
	if ip, _, parseErr := net.ParseCIDR(address); parseErr == nil {
		address = ip.String()
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || !privateOrSharedIPv4(ip) {
		return "", fmt.Errorf("Incus network %s returned no private IPv4 address", network)
	}
	return ip.To4().String(), nil
}

func privateOrSharedIPv4(ip net.IP) bool {
	value := ip.To4()
	return value != nil && (value.IsPrivate() || value[0] == 100 && value[1]&0xc0 == 0x40)
}

func checkLocalKVMAccess() error {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		return fmt.Errorf("inspect /dev/kvm: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("/dev/kvm is not a character device")
	}
	device, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/kvm for VM access: %w", err)
	}
	if err := device.Close(); err != nil {
		return fmt.Errorf("close /dev/kvm readiness probe: %w", err)
	}
	return nil
}

func gatewayIncusNetwork(profiles []core.SandboxProfile, selected []guidedProfilePlan) (string, error) {
	networks := map[string]struct{}{}
	for _, profile := range profiles {
		if _, direct := guidedIncusProfilePublishAddress(profile); direct {
			networks[profile.IncusNetwork] = struct{}{}
		}
	}
	for _, profile := range selected {
		if profile.Provider != core.SandboxProviderIncus {
			continue
		}
		network := guidedIncusNetwork
		if profile.Existing != nil {
			if _, direct := guidedIncusProfilePublishAddress(*profile.Existing); !direct {
				continue
			} else {
				network = profile.Existing.IncusNetwork
			}
		}
		networks[network] = struct{}{}
	}
	if len(networks) == 0 {
		return "", nil
	}
	if len(networks) != 1 {
		return "", fmt.Errorf("guided setup found Incus profiles on multiple networks; select one network explicitly with the advanced provider command")
	}
	var network string
	for value := range networks {
		network = value
	}
	return network, nil
}

// guidedIncusBridgeAuthority identifies the one direct host bridge address
// already retained by selected or installed Incus Profiles. A verified
// Profile owns this route; a fresh network observation may confirm it but may
// never silently replace it.
func guidedIncusBridgeAuthority(profiles []core.SandboxProfile, selected []guidedProfilePlan, network string) (string, bool, error) {
	addresses := map[string]struct{}{}
	bridgeRequired := false
	consider := func(profile core.SandboxProfile) {
		if profile.Provider != core.SandboxProviderIncus || profile.IncusNetwork != network {
			return
		}
		if address, direct := guidedIncusProfilePublishAddress(profile); direct {
			addresses[address] = struct{}{}
			bridgeRequired = true
		}
	}
	for _, profile := range profiles {
		consider(profile)
	}
	for _, plan := range selected {
		if plan.Provider != core.SandboxProviderIncus {
			continue
		}
		if plan.Existing == nil {
			bridgeRequired = true
			continue
		}
		consider(*plan.Existing)
	}
	if len(addresses) > 1 {
		return "", false, fmt.Errorf("guided setup found Incus profiles with different Gateway addresses; update the profiles explicitly before reconciling Compose")
	}
	for address := range addresses {
		return address, bridgeRequired, nil
	}
	return "", bridgeRequired, nil
}

// guidedIncusProfilePublishAddress recognizes only the route shape guided
// local setup owns directly. HTTPS and nonstandard-port URLs remain explicit
// operator ingress and do not become a host bind by inference.
func guidedIncusProfilePublishAddress(profile core.SandboxProfile) (string, bool) {
	if profile.Provider != core.SandboxProviderIncus {
		return "", false
	}
	parsed, err := url.Parse(profile.IncusGatewayURL)
	if err != nil || parsed.Scheme != "http" || parsed.Port() != "8317" || parsed.Path != "/v1" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || !privateOrSharedIPv4(ip) {
		return "", false
	}
	return ip.To4().String(), true
}

func validateIncusProfileEndpointAuthority(authority *deployment.Incus, profile core.SandboxProfile) error {
	if authority == nil {
		return fmt.Errorf("Sandbox profile %q requires an Incus endpoint that is not configured in the Dorf Deployment", profile.Name)
	}
	authorityHash, err := authority.AuthorityHash()
	if err != nil {
		return fmt.Errorf("validate Incus Deployment endpoint authority: %w", err)
	}
	if profile.IncusEndpointAuthorityHash == "" || profile.IncusEndpointAuthorityHash != authorityHash {
		return fmt.Errorf("Sandbox profile %q belongs to a different Incus endpoint authority; update and re-verify the profile explicitly", profile.Name)
	}
	return nil
}

func setupAIConnection(ctx context.Context, g gateway.Gateway, bind string, options setupOptions, presenter setupPresenter) (string, error) {
	connection, retained, err := retainedSetupConnection(options, g.DefaultConnection, g.ConfiguredConnection)
	if err != nil {
		return "", err
	}
	if retained {
		if err := g.PrepareContainer(ctx, bind); err != nil {
			return "", err
		}
		return connection, nil
	}
	mode := options.ConnectionMode
	if mode == "" && presenter.interactive && !options.Yes {
		mode = setupConnectionChatGPT
		if err := presenter.RunForm(ctx, presenter.ConnectionGroup(&mode)); err != nil {
			return "", err
		}
	}
	if mode == "" {
		return "", fmt.Errorf("automated setup requires --ai-connection NAME or --connection-auth chatgpt|openai")
	}
	switch mode {
	case setupConnectionChatGPT:
		const name = "personal-chatgpt"
		if err := g.ConnectChatGPT(ctx, name, bind, func(url, code string) {
			presenter.Note("Device sign-in", "Open "+url+" and enter "+code)
		}); err != nil {
			return "", err
		}
		return name, nil
	case setupConnectionOpenAI:
		const name = "openai-api"
		key := ""
		if options.OpenAIKeyFile != "" {
			key, err = readSecretFile(options.OpenAIKeyFile, os.Stdin)
		} else if presenter.interactive && !options.Yes {
			if err = presenter.RunForm(ctx, presenter.SecretGroup("Enter your OpenAI API key", "Stored only in Dorf's protected Provider Gateway state.", &key)); err == nil {
				key = strings.TrimSpace(key)
			}
		} else {
			err = fmt.Errorf("automated OpenAI setup requires --openai-api-key-file")
		}
		if err != nil {
			return "", err
		}
		if err := g.ConnectOpenAIAPIKey(ctx, name, bind, key); err != nil {
			return "", err
		}
		return name, nil
	default:
		return "", fmt.Errorf("connection authentication must be chatgpt or openai")
	}
}

func retainedSetupConnection(
	options setupOptions,
	defaultConnection func() (string, error),
	configured func(string) (bool, error),
) (string, bool, error) {
	if name := strings.TrimSpace(options.Connection); name != "" {
		found, err := configured(name)
		if err != nil {
			return "", false, fmt.Errorf("inspect existing AI connection %q: %w", name, err)
		}
		if !found {
			return "", false, fmt.Errorf("existing AI connection %q is unavailable", name)
		}
		return name, true, nil
	}
	if options.ConnectionMode == "" {
		if name, err := defaultConnection(); err == nil {
			return name, true, nil
		}
		name, err := unambiguousSetupConnection(configured)
		if err != nil {
			return "", false, err
		}
		return name, name != "", nil
	}
	name := ""
	switch options.ConnectionMode {
	case setupConnectionChatGPT:
		name = "personal-chatgpt"
	case setupConnectionOpenAI:
		name = "openai-api"
	default:
		return "", false, nil
	}
	found, err := configured(name)
	if err != nil {
		return "", false, fmt.Errorf("inspect existing AI connection %q: %w", name, err)
	}
	return name, found, nil
}

func unambiguousSetupConnection(check func(string) (bool, error)) (string, error) {
	chatGPT, err := check("personal-chatgpt")
	if err != nil {
		return "", err
	}
	openAI, err := check("openai-api")
	if err != nil {
		return "", err
	}
	switch {
	case chatGPT && !openAI:
		return "personal-chatgpt", nil
	case openAI && !chatGPT:
		return "openai-api", nil
	default:
		return "", nil
	}
}

func setupE2BCredential(ctx context.Context, cfg *config.Config, options setupOptions, presenter setupPresenter) error {
	key := strings.TrimSpace(cfg.E2BAPIKey)
	configured := key != ""
	var err error
	if options.E2BKeyFile != "" {
		key, err = readSecretFile(options.E2BKeyFile, os.Stdin)
	} else if key == "" && presenter.interactive && !options.Yes {
		err = presenter.RunForm(ctx, presenter.SecretGroup("Enter your E2B API key", "Used only by this host to create and reconcile managed Sandboxes.", &key))
		key = strings.TrimSpace(key)
	} else if key == "" {
		err = fmt.Errorf("automated E2B setup requires E2B_API_KEY or --e2b-api-key-file")
	}
	if err != nil {
		return err
	}
	if err := (e2b.Client{APIKey: key}).Check(ctx); err != nil {
		return fmt.Errorf("verify E2B project credential: %w", err)
	}
	return retainSetupE2BCredential(cfg, key, !configured || options.E2BKeyFile != "")
}

func retainSetupE2BCredential(cfg *config.Config, key string, suppliedDuringSetup bool) error {
	if cfg.DatabaseExternal {
		if !suppliedDuringSetup {
			return nil
		}
		return fmt.Errorf("guided E2B credential storage is unavailable with external PostgreSQL; set E2B_API_KEY on the deployment")
	}
	if err := deployment.SaveE2BAPIKey(cfg.DeploymentPath, key); err != nil {
		return err
	}
	cfg.E2BAPIKey = key
	return nil
}

func planRemoteGateway(ctx context.Context, options setupOptions, profiles []guidedProfilePlan, presenter setupPresenter, resolver dnsResolver, owned ownedCloudflareEndpoints) (guidedGatewayPlan, error) {
	cloudflareRequested := options.CloudflareDomain != ""
	if !cloudflareRequested && options.GatewayURL == "" && owned.ControlHostname != "" && owned.ModelHostname != "" {
		return retainedCloudflareGatewayPlan(owned)
	}

	existingURL, err := retainedGuidedRemoteGatewayURL(profiles)
	if err != nil {
		return guidedGatewayPlan{}, err
	}

	if options.GatewayURL != "" {
		if owned.ControlHostname != "" {
			return guidedGatewayPlan{}, fmt.Errorf("remove the Dorf-owned Cloudflare Tunnel before selecting a custom --gateway-url")
		}
		normalized, err := normalizeExactGatewayURL(options.GatewayURL)
		if err != nil {
			return guidedGatewayPlan{}, err
		}
		return guidedGatewayPlan{Mode: setupGatewayExisting, URL: normalized}, nil
	}
	if cloudflareRequested {
		plan, err := newCloudflareGatewayPlan(
			options.CloudflareDomain,
			options.CloudflareControlHostname,
			options.CloudflareModelHostname,
			options.ReplaceCloudflareDNS,
		)
		if err != nil {
			return guidedGatewayPlan{}, err
		}
		return approveCloudflareGatewayPlan(ctx, options, presenter, resolver, plan, owned)
	}
	if existingURL != "" {
		return guidedGatewayPlan{Mode: setupGatewayExisting, URL: existingURL}, nil
	}
	if !presenter.interactive || options.Yes {
		return guidedGatewayPlan{}, fmt.Errorf("automated remote Sandbox setup requires --gateway-url or --cloudflare-domain")
	}

	domain := ""
	if err := presenter.RunForm(ctx, presenter.TextGroup(
		"Choose your Dorf domain",
		"The domain must already use Cloudflare DNS. Dorf leaves its apex untouched.",
		"dorf.run", &domain, validateCloudflareHostname,
	)); err != nil {
		return guidedGatewayPlan{}, err
	}
	domain, err = normalizeCloudflareHostname(domain)
	if err != nil {
		return guidedGatewayPlan{}, err
	}
	if err := requireCloudflareZone(ctx, resolver, domain); err != nil {
		return guidedGatewayPlan{}, err
	}
	controlHostname, modelHostname := "api."+domain, "models."+domain
	if err := presenter.RunForm(ctx, presenter.PublicEndpointsGroup(domain, &controlHostname, &modelHostname)); err != nil {
		return guidedGatewayPlan{}, err
	}
	plan, err := newCloudflareGatewayPlan(domain, controlHostname, modelHostname, false)
	if err != nil {
		return guidedGatewayPlan{}, err
	}
	return approveCloudflareGatewayPlan(ctx, options, presenter, resolver, plan, owned)
}

func retainedGuidedRemoteGatewayURL(profiles []guidedProfilePlan) (string, error) {
	existingURLs := map[string]struct{}{}
	for _, profile := range profiles {
		existingURL := guidedProfileRemoteGatewayURL(profile)
		if existingURL == "" {
			continue
		}
		normalized, err := normalizeExactGatewayURL(existingURL)
		if err != nil {
			return "", fmt.Errorf("Sandbox profile %q model Gateway: %w", profile.Name, err)
		}
		existingURLs[normalized] = struct{}{}
	}
	if len(existingURLs) > 1 {
		return "", fmt.Errorf("selected Sandbox profiles use different model Gateway URLs; update them explicitly before rerunning setup")
	}
	for value := range existingURLs {
		return value, nil
	}
	return "", nil
}

func guidedProfileRemoteGatewayURL(profile guidedProfilePlan) string {
	if profile.Existing == nil {
		return ""
	}
	if profile.Provider == core.SandboxProviderE2B {
		return profile.Existing.E2BGatewayURL
	}
	if profile.Provider == core.SandboxProviderIncus && strings.HasPrefix(profile.Existing.IncusGatewayURL, "https://") {
		return profile.Existing.IncusGatewayURL
	}
	return ""
}

func newCloudflareGatewayPlan(domain, controlHostname, modelHostname string, replaceExistingDNS bool) (guidedGatewayPlan, error) {
	domain, err := normalizeCloudflareHostname(domain)
	if err != nil {
		return guidedGatewayPlan{}, fmt.Errorf("Dorf domain: %w", err)
	}
	if strings.TrimSpace(controlHostname) == "" {
		controlHostname = "api." + domain
	}
	if strings.TrimSpace(modelHostname) == "" {
		modelHostname = "models." + domain
	}
	controlHostname, err = normalizeCloudflareHostname(controlHostname)
	if err != nil {
		return guidedGatewayPlan{}, fmt.Errorf("Control API hostname: %w", err)
	}
	modelHostname, err = normalizeCloudflareHostname(modelHostname)
	if err != nil {
		return guidedGatewayPlan{}, fmt.Errorf("model Gateway hostname: %w", err)
	}
	if controlHostname == modelHostname {
		return guidedGatewayPlan{}, fmt.Errorf("Control API and model Gateway hostnames must differ")
	}
	if !directChildHostname(controlHostname, domain) || !directChildHostname(modelHostname, domain) {
		return guidedGatewayPlan{}, fmt.Errorf("Control API and model Gateway hostnames must each be one direct subdomain of %s", domain)
	}
	controlURL, _ := cloudflareapp.ControlURL(controlHostname)
	gatewayURL, _ := cloudflareapp.GatewayURL(modelHostname)
	return guidedGatewayPlan{
		Mode: setupGatewayCloudflare, URL: gatewayURL, Domain: domain, ControlURL: controlURL,
		ControlHostname: controlHostname, ModelHostname: modelHostname, ReplaceExistingDNS: replaceExistingDNS,
	}, nil
}

func retainedCloudflareGatewayPlan(owned ownedCloudflareEndpoints) (guidedGatewayPlan, error) {
	controlURL, err := cloudflareapp.ControlURL(owned.ControlHostname)
	if err != nil {
		return guidedGatewayPlan{}, err
	}
	gatewayURL, err := cloudflareapp.GatewayURL(owned.ModelHostname)
	if err != nil {
		return guidedGatewayPlan{}, err
	}
	return guidedGatewayPlan{
		Mode: setupGatewayCloudflare, URL: gatewayURL, Domain: commonEndpointParent(owned), ControlURL: controlURL,
		ControlHostname: owned.ControlHostname, ModelHostname: owned.ModelHostname,
	}, nil
}

func approveCloudflareGatewayPlan(ctx context.Context, options setupOptions, presenter setupPresenter, resolver dnsResolver, plan guidedGatewayPlan, owned ownedCloudflareEndpoints) (guidedGatewayPlan, error) {
	occupied, err := inspectCloudflareGatewayPlan(ctx, resolver, plan, owned)
	if err != nil {
		return guidedGatewayPlan{}, err
	}
	if len(occupied) == 0 || plan.ReplaceExistingDNS {
		return plan, nil
	}
	if !presenter.interactive || options.Yes {
		return guidedGatewayPlan{}, fmt.Errorf("%s already resolves; choose unused public endpoints or pass --replace-cloudflare-dns", strings.Join(occupied, " and "))
	}
	confirmed := false
	description := fmt.Sprintf("Replace unrelated DNS routes for %s?", strings.Join(occupied, " and "))
	if err := presenter.RunForm(ctx, presenter.ConfirmGroup("Replace existing Cloudflare DNS", description, &confirmed)); err != nil {
		return guidedGatewayPlan{}, err
	}
	if !confirmed {
		return guidedGatewayPlan{}, errSetupCancelled
	}
	plan.ReplaceExistingDNS = true
	return plan, nil
}

func inspectCloudflareGatewayPlan(ctx context.Context, resolver dnsResolver, plan guidedGatewayPlan, owned ownedCloudflareEndpoints) ([]string, error) {
	if err := requireCloudflareZone(ctx, resolver, plan.Domain); err != nil {
		return nil, err
	}
	for _, hostname := range []string{plan.ControlHostname, plan.ModelHostname} {
		delegation, err := requireCloudflareDNS(ctx, resolver, hostname)
		if err != nil {
			return nil, err
		}
		if delegation.Zone != plan.Domain {
			return nil, fmt.Errorf("%s is delegated separately from the selected Dorf domain %s", hostname, plan.Domain)
		}
	}
	if owned.ControlHostname != "" && owned.ControlHostname != plan.ControlHostname {
		return nil, fmt.Errorf("the Dorf-owned Cloudflare Tunnel already uses Control API hostname %s; remove that Tunnel before selecting %s", owned.ControlHostname, plan.ControlHostname)
	}
	if owned.ModelHostname != "" && owned.ModelHostname != plan.ModelHostname {
		return nil, fmt.Errorf("the Dorf-owned Cloudflare Tunnel already uses model Gateway hostname %s; remove that Tunnel before selecting %s", owned.ModelHostname, plan.ModelHostname)
	}
	occupied := make([]string, 0, 2)
	for _, endpoint := range []struct {
		hostname string
		owned    string
	}{
		{hostname: plan.ControlHostname, owned: owned.ControlHostname},
		{hostname: plan.ModelHostname, owned: owned.ModelHostname},
	} {
		if endpoint.hostname == endpoint.owned {
			continue
		}
		hasAddresses, err := hostnameHasAddresses(ctx, resolver, endpoint.hostname)
		if err != nil {
			return nil, fmt.Errorf("inspect address records for %s: %w", endpoint.hostname, err)
		}
		if hasAddresses {
			occupied = append(occupied, endpoint.hostname)
		}
	}
	return occupied, nil
}

func requireCloudflareZone(ctx context.Context, resolver dnsResolver, domain string) error {
	delegation, err := requireCloudflareDNS(ctx, resolver, domain)
	if err != nil {
		return err
	}
	if delegation.Zone != domain {
		return fmt.Errorf("Dorf domain %s must be the Cloudflare DNS zone apex; detected %s", domain, delegation.Zone)
	}
	return nil
}

func validateCloudflareHostname(raw string) error {
	_, err := normalizeCloudflareHostname(raw)
	return err
}

func normalizeCloudflareHostname(raw string) (string, error) {
	controlURL, err := cloudflareapp.ControlURL(raw)
	if err != nil {
		return "", err
	}
	parsed, _ := url.Parse(controlURL)
	return parsed.Hostname(), nil
}

func directChildHostname(hostname, domain string) bool {
	label, found := strings.CutSuffix(hostname, "."+domain)
	return found && label != "" && !strings.Contains(label, ".")
}

func commonEndpointParent(owned ownedCloudflareEndpoints) string {
	_, controlParent, controlFound := strings.Cut(owned.ControlHostname, ".")
	_, modelParent, modelFound := strings.Cut(owned.ModelHostname, ".")
	if controlFound && modelFound && controlParent == modelParent {
		return controlParent
	}
	return ""
}

func currentOwnedCloudflareEndpoints(gatewayStatePath string) (ownedCloudflareEndpoints, error) {
	state, found, err := (cloudflareapp.Tunnel{StatePath: filepath.Join(gatewayStatePath, "cloudflare")}).Current()
	if err != nil || !found {
		return ownedCloudflareEndpoints{}, err
	}
	return ownedCloudflareEndpoints{ControlHostname: state.Hostname, ModelHostname: state.ModelHostname}, nil
}

func requireSupportedCloudflareState(gatewayStatePath string) error {
	if strings.TrimSpace(gatewayStatePath) == "" {
		return nil
	}
	owned, err := currentOwnedCloudflareEndpoints(gatewayStatePath)
	if err != nil || owned.ControlHostname == "" {
		return err
	}
	domain := commonEndpointParent(owned)
	if owned.ModelHostname == "" || domain == "" ||
		!directChildHostname(owned.ControlHostname, domain) || !directChildHostname(owned.ModelHostname, domain) {
		return fmt.Errorf("existing Cloudflare state uses the retired single-origin hostname layout; remove its Tunnel, DNS routes, and local state before rerunning dorf setup")
	}
	return nil
}

func requireCloudflareDNS(ctx context.Context, resolver dnsResolver, hostname string) (dnsDelegation, error) {
	delegation, err := discoverDNSDelegation(ctx, resolver, hostname)
	if err != nil {
		return dnsDelegation{}, fmt.Errorf("inspect DNS for %s: %w", hostname, err)
	}
	if delegation.Zone == "" {
		return dnsDelegation{}, fmt.Errorf("no public DNS delegation was found for %s; use --gateway-url after configuring HTTPS ingress", hostname)
	}
	if !delegation.Cloudflare {
		return dnsDelegation{}, fmt.Errorf("DNS for %s is delegated to %s, not Cloudflare; use --gateway-url after configuring HTTPS ingress", hostname, delegation.Zone)
	}
	return delegation, nil
}

func validateExactGatewayURL(raw string) error {
	_, err := normalizeExactGatewayURL(raw)
	return err
}

func normalizeExactGatewayURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/v1" {
		return "", fmt.Errorf("enter one exact HTTPS URL ending in /v1")
	}
	return parsed.String(), nil
}

func prepareRemoteGateway(ctx context.Context, gatewayStatePath string, plan guidedGatewayPlan, presenter setupPresenter, stdout, stderr io.Writer) (string, error) {
	tunnel := cloudflareapp.Tunnel{
		StatePath:          filepath.Join(gatewayStatePath, "cloudflare"),
		ReplaceExistingDNS: plan.ReplaceExistingDNS,
	}
	switch plan.Mode {
	case setupGatewayExisting:
		return plan.URL, nil
	case setupGatewayCloudflare:
		presenter.Note("Public ingress", "Preparing the Cloudflare Tunnel and DNS routes")
		state, err := tunnel.Prepare(ctx, plan.ControlHostname, plan.ModelHostname, stdout, stderr)
		if err != nil {
			return "", err
		}
		presenter.Ready("Cloudflare Tunnel", state.Hostname+" + "+state.ModelHostname+" · prepared")
		return plan.URL, nil
	default:
		return "", fmt.Errorf("Gateway setup plan is incomplete")
	}
}

func finalizeRemoteGateway(ctx context.Context, g gateway.Gateway, plan guidedGatewayPlan, gatewayStatePath string, presenter setupPresenter) error {
	tunnel := cloudflareapp.Tunnel{
		StatePath: filepath.Join(gatewayStatePath, "cloudflare"),
	}
	state, found, err := tunnel.Current()
	if err != nil {
		return err
	}
	managed := false
	if found {
		ownedURL, urlErr := cloudflareapp.GatewayURL(state.ModelHostname)
		if urlErr != nil {
			return urlErr
		}
		ownedControlURL, urlErr := cloudflareapp.ControlURL(state.Hostname)
		if urlErr != nil {
			return urlErr
		}
		managed = ownedURL == plan.URL && ownedControlURL == plan.ControlURL
	}
	if !managed {
		if err := g.CheckRemote(ctx, plan.URL); err != nil {
			return fmt.Errorf("existing Provider Gateway URL is not ready: %w", err)
		}
		return nil
	}
	presenter.Ready("Cloudflare Tunnel", state.Hostname+" + "+state.ModelHostname+" · Compose active")
	controlProbeURL, err := state.ProbeURL(state.Hostname)
	if err != nil {
		return err
	}
	return presenter.Run(ctx, "Checking the public Dorf routes", func(ctx context.Context) error {
		managedGateway, err := managedRemoteGateway(g, state)
		if err != nil {
			return err
		}
		return waitForRemoteGateway(ctx, managedGateway, plan.URL, plan.ControlURL, controlProbeURL)
	})
}

func managedRemoteGateway(g gateway.Gateway, state cloudflareapp.State) (gateway.Gateway, error) {
	probeURL, err := state.ProbeURL(state.ModelHostname)
	if err != nil {
		return gateway.Gateway{}, err
	}
	g.DeploymentProbeURL = probeURL
	return g, nil
}

func waitForRemoteGateway(ctx context.Context, g gateway.Gateway, gatewayURL, controlURL, controlProbeURL string) error {
	// The availability preflight may have cached an expected negative DNS
	// answer immediately before the Tunnel route was created. Readiness uses a
	// fresh public resolver so that setup does not wait for the host's negative
	// cache TTL before proving the new route.
	client := freshDNSHTTPClient()
	g.Client = client
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for {
		if err := g.CheckRemote(ctx, gatewayURL); err == nil {
			if err := checkPublicDeploymentProbe(ctx, controlProbeURL, client); err == nil {
				ready, detail := controlAPIReady(ctx, version.Version, controlURL+"/v1", client)
				if ready {
					return nil
				}
				last = errors.New(detail)
			} else {
				last = err
			}
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Cloudflare hostname did not reach Dorf within 90 seconds: %w", last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func checkPublicDeploymentProbe(ctx context.Context, probeURL string, client *http.Client) error {
	if client == nil {
		return fmt.Errorf("public Dorf deployment identity is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return err
	}
	probeClient := *client
	probeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := probeClient.Do(request)
	if err != nil {
		return fmt.Errorf("public Dorf deployment identity is unavailable: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("public Dorf deployment identity returned HTTP %d, want 204", response.StatusCode)
	}
	return nil
}

func setupProfiles(ctx context.Context, store postgres.Store, cfg config.Config, options setupOptions, plans []guidedProfilePlan, gatewayURL, privateIPv4 string, presenter setupPresenter) ([]core.SandboxProfile, error) {
	profiles := make([]core.SandboxProfile, 0, len(plans))
	for _, plan := range plans {
		provider, name := plan.Provider, plan.Name
		var profile core.SandboxProfile
		var err error
		if plan.Existing != nil {
			profile = *plan.Existing
		} else {
			switch provider {
			case core.SandboxProviderIncus:
				err = presenter.Run(ctx, "Installing official Incus Sandbox", func(ctx context.Context) error {
					var installErr error
					project, storagePool, network, targetErr := guidedIncusProfileTarget(cfg.Incus)
					if targetErr != nil {
						return targetErr
					}
					profileGatewayURL, routeErr := guidedIncusGatewayURL(cfg.Incus, gatewayURL, privateIPv4)
					if routeErr != nil {
						return routeErr
					}
					releaseTag, manifestPath, archive := releaseapp.OfficialImageRelease(), "", ""
					if options.IncusManifest != "" {
						releaseTag, manifestPath, archive = "", options.IncusManifest, options.IncusArchive
					}
					profile, _, _, installErr = reconcileOfficialIncusProfileDefinition(ctx, store, name, plan.Harness, releaseTag, manifestPath, archive,
						cfg.Incus, project, storagePool, network, guidedIncusDiskSize, profileGatewayURL)
					return installErr
				})
			case core.SandboxProviderE2B:
				template, templateErr := setupE2BTemplate(options)
				if templateErr != nil {
					return nil, templateErr
				}
				profile, _, err = store.CreateSandboxProfile(ctx, core.SandboxProfile{
					Name: name, Provider: provider, Harness: plan.Harness, Artifact: template,
					E2BGatewayURL: gatewayURL, E2BSandboxTimeout: 55 * time.Minute,
					E2BAllowInternet: options.AllowInternet,
				})
			}
			if err != nil {
				return nil, err
			}
		}
		if plan.Existing != nil && profile.BaseVerified() {
			profiles = append(profiles, profile)
			presenter.Ready("Sandbox profile", profile.Name+" · "+string(profile.Provider)+" · verified")
			continue
		}
		err = presenter.Run(ctx, "Verifying "+profile.Name, func(ctx context.Context) error {
			var verifyErr error
			profile, verifyErr = profileapp.VerifyBase(ctx, store, func(profile core.SandboxProfile) (providerapi.Sandbox, error) {
				return sandboxForProfile(cfg, profile)
			}, profile.Name)
			return verifyErr
		})
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
		presenter.Ready("Sandbox profile", profile.Name+" · "+string(profile.Provider)+" · verified")
	}
	return profiles, nil
}

func guidedIncusGatewayURL(authority *deployment.Incus, stableHTTPS, privateIPv4 string) (string, error) {
	if authority == nil {
		return "", fmt.Errorf("Incus endpoint is not configured in the Dorf Deployment")
	}
	if strings.HasPrefix(authority.Endpoint, "https://") {
		if err := validateExactGatewayURL(stableHTTPS); err != nil {
			return "", fmt.Errorf("remote Incus model Gateway: %w", err)
		}
		return stableHTTPS, nil
	}
	ip := net.ParseIP(privateIPv4)
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("local Incus model Gateway bridge is unavailable")
	}
	return "http://" + privateIPv4 + ":8317/v1", nil
}

func validateGuidedIncusGatewayProfiles(plans []guidedProfilePlan, authority *deployment.Incus, gatewayURL string) error {
	if authority == nil || !strings.HasPrefix(authority.Endpoint, "https://") {
		return nil
	}
	for _, plan := range plans {
		if plan.Provider != core.SandboxProviderIncus || plan.Existing == nil {
			continue
		}
		if plan.Existing.IncusGatewayURL != gatewayURL {
			return fmt.Errorf("Sandbox profile %q uses model Gateway %s; update it explicitly before selecting %s", plan.Name, plan.Existing.IncusGatewayURL, gatewayURL)
		}
	}
	return nil
}

func retargetGuidedE2BProfiles(ctx context.Context, store postgres.Store, plans []guidedProfilePlan, gatewayURL string) ([]guidedProfilePlan, error) {
	for index := range plans {
		plan := &plans[index]
		if plan.Provider != core.SandboxProviderE2B || plan.Existing == nil {
			continue
		}
		profile, err := retargetGuidedE2BProfile(ctx, store, *plan.Existing, gatewayURL)
		if err != nil {
			return nil, err
		}
		plan.Existing = &profile
	}
	return plans, nil
}

func retargetGuidedE2BProfile(ctx context.Context, store postgres.Store, profile core.SandboxProfile, gatewayURL string) (core.SandboxProfile, error) {
	if gatewayURL == "" || profile.E2BGatewayURL == gatewayURL {
		return profile, nil
	}
	updated, _, err := store.UpdateSandboxProfile(ctx, profile.Name, postgres.SandboxProfilePatch{E2BGatewayURL: &gatewayURL})
	if err != nil {
		return core.SandboxProfile{}, fmt.Errorf("update Sandbox profile %q for the prepared model Gateway: %w", profile.Name, err)
	}
	return updated, nil
}

func setupE2BTemplate(options setupOptions) (string, error) {
	template := strings.TrimSpace(options.E2BTemplate)
	if template == "" {
		template = guidedE2BTemplate
	}
	if err := validateExactE2BTemplate(template); err != nil {
		return "", err
	}
	return template, nil
}

func validateExactE2BTemplate(value string) error {
	value = strings.TrimSpace(value)
	name, build, found := strings.Cut(value, ":")
	if !found || strings.TrimSpace(name) == "" || strings.TrimSpace(build) == "" || strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("enter an exact template name:build-id reference")
	}
	return nil
}

func setupDefaultProfile(ctx context.Context, store postgres.Store, profiles []core.SandboxProfile, yes bool, presenter setupPresenter) (core.SandboxProfile, error) {
	if len(profiles) == 0 {
		return core.SandboxProfile{}, fmt.Errorf("setup produced no Sandbox profile")
	}
	selected := profiles[0].Name
	if len(profiles) > 1 && presenter.interactive && !yes {
		options := make([]setupChoice[string], 0, len(profiles))
		for _, profile := range profiles {
			options = append(options, setupChoice[string]{
				Title: profile.Name, Description: string(profile.Provider) + " · " + profile.Harness, Value: profile.Name,
			})
		}
		group := huh.NewGroup(setupSelect(presenter, &selected, options...)).
			Title("Which Sandbox profile should be the default?").
			Description("Every Job may still select another verified profile explicitly.")
		if err := presenter.RunForm(ctx, group); err != nil {
			return core.SandboxProfile{}, err
		}
	}
	return store.SetDefaultSandboxProfile(ctx, selected)
}
