package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
	Hostname           string
	ReplaceExistingDNS bool
}

type guidedSetupPrepared struct {
	Gateway      gateway.Gateway
	Connection   string
	ProfilePlans []guidedProfilePlan
	GatewayPlan  guidedGatewayPlan
	GatewayURL   string
	PrivateIPv4  string
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
	profilePlans, err := planGuidedProfiles(ctx, store, options, providers, harness)
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
	gatewayURL := ""
	if containsSandboxProvider(providers, core.SandboxProviderE2B) {
		if err := setupE2BCredential(ctx, cfg, options, presenter); err != nil {
			return guidedSetupPrepared{}, err
		}
		presenter.Ready("E2B access", "Project credential verified on this host")
		ownedHostname, ownedErr := currentOwnedCloudflareHostname(cfg.GatewayStatePath)
		if ownedErr != nil {
			return guidedSetupPrepared{}, ownedErr
		}
		gatewayPlan, planErr := planRemoteGateway(ctx, options, profilePlans, presenter, net.DefaultResolver, ownedHostname)
		if planErr != nil {
			return guidedSetupPrepared{}, planErr
		}
		gatewayURL, err = prepareRemoteGateway(ctx, cfg.GatewayStatePath, gatewayPlan, options.Yes, presenter, stdout, stderr)
		if err != nil {
			return guidedSetupPrepared{}, err
		}
		prepared.GatewayPlan = gatewayPlan
		prepared.GatewayURL = gatewayURL
	}
	return prepared, nil
}

func guidedIncusReadinessScope(plans []guidedProfilePlan) (string, string, bool) {
	for _, plan := range plans {
		if plan.Provider != core.SandboxProviderIncus {
			continue
		}
		if plan.Existing != nil {
			return plan.Existing.IncusProject, plan.Existing.IncusStoragePool, true
		}
		return incus.DefaultProject, incus.DefaultStoragePool, true
	}
	return "", "", false
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
	project, storagePool, selected := guidedIncusReadinessScope(plans)
	if !selected {
		return nil
	}
	if authority != nil && strings.HasPrefix(authority.Endpoint, "https://") {
		return guidedRemoteIncusSetupError()
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

// persistGuidedIncusAuthority commits a newly selected local endpoint before
// setup creates a Profile whose definition is bound to that endpoint hash.
func persistGuidedIncusAuthority(cfg *config.Config, privateBridge string) error {
	if privateBridge == "" {
		return nil
	}
	if cfg.Incus == nil {
		return incusSetupReadinessError{cause: fmt.Errorf("Incus endpoint is not configured in the Dorf Deployment")}
	}
	if !strings.HasPrefix(cfg.Incus.Endpoint, "unix://") {
		return guidedRemoteIncusSetupError()
	}
	return deployment.SaveIncus(cfg.DeploymentPath, *cfg.Incus)
}

func guidedRemoteIncusSetupError() incusSetupReadinessError {
	return incusSetupReadinessError{cause: fmt.Errorf("guided remote Incus setup is not supported; configure an explicit guest-reachable Gateway route and profile after the remote terminal passes")}
}

// completeGuidedSetup resumes immediately after the final Compose
// reconciliation. Every operation here requires the live deployment.
func completeGuidedSetup(ctx context.Context, store postgres.Store, cfg config.Config, options setupOptions, prepared guidedSetupPrepared, presenter setupPresenter) error {
	if prepared.GatewayURL != "" {
		if err := finalizeRemoteGateway(ctx, prepared.Gateway, prepared.GatewayPlan, cfg.GatewayStatePath, presenter); err != nil {
			return err
		}
		presenter.Ready("Cloud Gateway", prepared.GatewayURL)
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

func planGuidedProfiles(ctx context.Context, store postgres.Store, options setupOptions, providers []core.SandboxProvider, harness string) ([]guidedProfilePlan, error) {
	plans := make([]guidedProfilePlan, 0, len(providers))
	for _, provider := range providers {
		name := options.ProfileName
		if name == "" {
			if provider == core.SandboxProviderIncus {
				name = "local-" + harness
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
	project, storagePool := gatewayIncusScope(profiles, selected, privateBridge)
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
	network, err := gatewayIncusNetwork(profiles, selected)
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
	profileAddress, bridgeRequired, err := guidedIncusBridgeAuthority(profiles, selected, network)
	if err != nil {
		return "", "", false, err
	}
	needsLocalDefinition := hasGuidedIncusProfileNeedingLocalDefinition(selected)
	if needsLocalDefinition {
		if authority == nil {
			return "", "", false, incusSetupReadinessError{cause: fmt.Errorf("Incus endpoint is not configured in the Dorf Deployment")}
		}
		if strings.HasPrefix(authority.Endpoint, "https://") {
			return "", "", false, guidedRemoteIncusSetupError()
		}
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

func gatewayIncusScope(profiles []core.SandboxProfile, selected []guidedProfilePlan, network string) (string, string) {
	for _, plan := range selected {
		if plan.Provider != core.SandboxProviderIncus {
			continue
		}
		if plan.Existing == nil {
			return incus.DefaultProject, incus.DefaultStoragePool
		}
		if _, direct := guidedIncusProfilePublishAddress(*plan.Existing); direct && plan.Existing.IncusNetwork == network {
			return plan.Existing.IncusProject, plan.Existing.IncusStoragePool
		}
	}
	for _, profile := range profiles {
		if _, direct := guidedIncusProfilePublishAddress(profile); direct && profile.IncusNetwork == network {
			return profile.IncusProject, profile.IncusStoragePool
		}
	}
	return incus.DefaultProject, incus.DefaultStoragePool
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

func planRemoteGateway(ctx context.Context, options setupOptions, profiles []guidedProfilePlan, presenter setupPresenter, resolver dnsResolver, ownedCloudflareHostname string) (guidedGatewayPlan, error) {
	for _, profile := range profiles {
		if profile.Provider != core.SandboxProviderE2B || profile.Existing == nil {
			continue
		}
		existingURL := profile.Existing.E2BGatewayURL
		if options.GatewayURL != "" {
			provided, err := normalizeExactGatewayURL(options.GatewayURL)
			if err != nil {
				return guidedGatewayPlan{}, err
			}
			if provided != existingURL {
				return guidedGatewayPlan{}, fmt.Errorf("Sandbox profile %q already uses %s; update the profile explicitly before changing its Gateway URL", profile.Name, existingURL)
			}
		}
		if options.CloudflareHost != "" {
			wanted, err := cloudflareapp.GatewayURL(options.CloudflareHost)
			if err != nil {
				return guidedGatewayPlan{}, err
			}
			if wanted != existingURL {
				return guidedGatewayPlan{}, fmt.Errorf("Sandbox profile %q already uses %s; update the profile explicitly before changing its Gateway URL", profile.Name, existingURL)
			}
			if _, err := requireCloudflareDNS(ctx, resolver, options.CloudflareHost); err != nil {
				return guidedGatewayPlan{}, err
			}
			parsed, _ := url.Parse(wanted)
			hostname := parsed.Hostname()
			if err := requireUnusedOrOwnedCloudflareHostname(ctx, resolver, hostname, ownedCloudflareHostname, options.ReplaceCloudflareDNS); err != nil {
				return guidedGatewayPlan{}, err
			}
			return guidedGatewayPlan{
				Mode: setupGatewayCloudflare, URL: wanted, Hostname: hostname,
				ReplaceExistingDNS: options.ReplaceCloudflareDNS,
			}, nil
		}
		return guidedGatewayPlan{Mode: setupGatewayExisting, URL: existingURL}, nil
	}

	plan := guidedGatewayPlan{}
	interactiveHostname := false
	cloudflareDNSConfirmed := false
	switch {
	case options.CloudflareHost != "":
		plan.Mode, plan.Hostname = setupGatewayCloudflare, options.CloudflareHost
		plan.ReplaceExistingDNS = options.ReplaceCloudflareDNS
	case options.GatewayURL != "":
		plan.Mode, plan.URL = setupGatewayExisting, options.GatewayURL
	case presenter.interactive && !options.Yes:
		interactiveHostname = true
		if err := presenter.RunForm(ctx, presenter.TextGroup(
			"Choose the Gateway hostname",
			"Cloud Sandboxes will reach Dorf through this stable public hostname.",
			"dorf.example.com", &plan.Hostname,
			func(value string) error { _, err := cloudflareapp.GatewayURL(value); return err }),
		); err != nil {
			return guidedGatewayPlan{}, err
		}
		plan.Hostname = strings.TrimSpace(plan.Hostname)
		delegation, lookupErr := discoverDNSDelegation(ctx, resolver, plan.Hostname)
		switch {
		case lookupErr != nil:
			plan.Mode = setupGatewayExisting
			presenter.Note("Gateway ingress", "DNS provider could not be confirmed; use existing HTTPS ingress")
		case delegation.Cloudflare:
			occupied, addressErr := hostnameHasAddresses(ctx, resolver, plan.Hostname)
			switch {
			case addressErr != nil:
				plan.Mode = setupGatewayExisting
				presenter.Note("Gateway ingress", "Existing DNS records could not be ruled out; use existing HTTPS ingress")
			case occupied:
				plan.Mode = setupGatewayExisting
				cloudflareDNSConfirmed = true
				if err := presenter.RunForm(ctx, presenter.CloudflareGatewayGroup(&plan.Mode, delegation.Zone, true)); err != nil {
					return guidedGatewayPlan{}, err
				}
				plan.ReplaceExistingDNS = plan.Mode == setupGatewayCloudflare
			default:
				cloudflareDNSConfirmed = true
				plan.Mode = setupGatewayCloudflare
				if err := presenter.RunForm(ctx, presenter.CloudflareGatewayGroup(&plan.Mode, delegation.Zone, false)); err != nil {
					return guidedGatewayPlan{}, err
				}
			}
		default:
			plan.Mode = setupGatewayExisting
			presenter.Note("Gateway ingress", "Cloudflare DNS was not detected; use existing HTTPS ingress")
		}
	default:
		return guidedGatewayPlan{}, fmt.Errorf("automated E2B setup requires --gateway-url or --cloudflare-hostname")
	}
	switch plan.Mode {
	case setupGatewayExisting:
		if plan.URL == "" && plan.Hostname != "" {
			var err error
			plan.URL, err = cloudflareapp.GatewayURL(plan.Hostname)
			if err != nil {
				return guidedGatewayPlan{}, err
			}
		}
		if plan.URL == "" {
			if err := presenter.RunForm(ctx, presenter.TextGroup(
				"Enter the existing Provider Gateway URL",
				"It must be an exact stable HTTPS URL ending in /v1.",
				"https://gateway.example.com/v1", &plan.URL,
				validateExactGatewayURL),
			); err != nil {
				return guidedGatewayPlan{}, err
			}
		}
		var err error
		plan.URL, err = normalizeExactGatewayURL(plan.URL)
		if err != nil {
			return guidedGatewayPlan{}, err
		}
		if interactiveHostname {
			confirmed := false
			description := fmt.Sprintf("Route %s to the private Dorf Gateway. Continue once that HTTPS ingress is ready.", plan.URL)
			if err := presenter.RunForm(ctx, presenter.ConfirmGroup("Use existing HTTPS ingress", description, &confirmed)); err != nil {
				return guidedGatewayPlan{}, err
			}
			if !confirmed {
				return guidedGatewayPlan{}, errSetupCancelled
			}
		}
		return plan, nil
	case setupGatewayCloudflare:
		if plan.Hostname == "" {
			if err := presenter.RunForm(ctx, presenter.TextGroup(
				"Choose the Cloudflare hostname",
				"The domain must already use Cloudflare DNS.",
				"dorf.example.com", &plan.Hostname,
				func(value string) error { _, err := cloudflareapp.GatewayURL(value); return err }),
			); err != nil {
				return guidedGatewayPlan{}, err
			}
		}
		plan.Hostname = strings.TrimSpace(plan.Hostname)
		var err error
		plan.URL, err = cloudflareapp.GatewayURL(plan.Hostname)
		if err != nil {
			return guidedGatewayPlan{}, err
		}
		parsedGateway, _ := url.Parse(plan.URL)
		plan.Hostname = parsedGateway.Hostname()
		if !cloudflareDNSConfirmed {
			if _, err := requireCloudflareDNS(ctx, resolver, plan.Hostname); err != nil {
				return guidedGatewayPlan{}, err
			}
			if err := requireUnusedOrOwnedCloudflareHostname(ctx, resolver, plan.Hostname, ownedCloudflareHostname, plan.ReplaceExistingDNS); err != nil {
				return guidedGatewayPlan{}, err
			}
		}
		return plan, nil
	default:
		return guidedGatewayPlan{}, fmt.Errorf("Gateway setup must use Cloudflare Tunnel or an existing HTTPS URL")
	}
}

func requireUnusedOrOwnedCloudflareHostname(ctx context.Context, resolver dnsResolver, hostname, ownedHostname string, allowReplacement bool) error {
	if hostname == ownedHostname || allowReplacement {
		return nil
	}
	occupied, err := hostnameHasAddresses(ctx, resolver, hostname)
	if err != nil {
		return fmt.Errorf("inspect address records for %s: %w", hostname, err)
	}
	if occupied {
		return fmt.Errorf("%s already resolves; use --gateway-url for existing ingress or choose an unused --cloudflare-hostname", hostname)
	}
	return nil
}

func currentOwnedCloudflareHostname(gatewayStatePath string) (string, error) {
	state, found, err := (cloudflareapp.Tunnel{StatePath: filepath.Join(gatewayStatePath, "cloudflare")}).Current()
	if err != nil || !found {
		return "", err
	}
	return state.Hostname, nil
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

func prepareRemoteGateway(ctx context.Context, gatewayStatePath string, plan guidedGatewayPlan, yes bool, presenter setupPresenter, stdout, stderr io.Writer) (string, error) {
	tunnel := cloudflareapp.Tunnel{
		StatePath:          filepath.Join(gatewayStatePath, "cloudflare"),
		Origin:             cloudflareapp.ComposeGatewayOrigin,
		ReplaceExistingDNS: plan.ReplaceExistingDNS,
	}
	switch plan.Mode {
	case setupGatewayExisting:
		state, found, err := tunnel.Current()
		if err != nil {
			return "", err
		}
		if found {
			ownedURL, urlErr := cloudflareapp.GatewayURL(state.Hostname)
			if urlErr != nil {
				return "", urlErr
			}
			if ownedURL == plan.URL {
				presenter.Note("Cloud Gateway", "Preparing the existing Cloudflare Tunnel for Compose")
				state, err = tunnel.Prepare(ctx, state.Hostname, stdout, stderr)
				if err != nil {
					return "", err
				}
				presenter.Ready("Cloudflare Tunnel", state.Hostname+" · prepared")
				return plan.URL, nil
			}
		}
		return plan.URL, nil
	case setupGatewayCloudflare:
		confirmed := yes
		if !confirmed {
			description := fmt.Sprintf("Create one named Tunnel, route %s to the Compose Provider Gateway, and remove the temporary account credential.", plan.Hostname)
			if plan.ReplaceExistingDNS {
				description = fmt.Sprintf("Create one named Tunnel and replace %s's existing DNS route with the Compose Provider Gateway.", plan.Hostname)
			}
			if err := presenter.RunForm(ctx, presenter.ConfirmGroup("Review Cloudflare changes", description, &confirmed)); err != nil {
				return "", err
			}
			if !confirmed {
				return "", errSetupCancelled
			}
		}
		presenter.Note("Cloud Gateway", "Preparing the Cloudflare Tunnel and DNS route")
		state, err := tunnel.Prepare(ctx, plan.Hostname, stdout, stderr)
		if err != nil {
			return "", err
		}
		presenter.Ready("Cloudflare Tunnel", state.Hostname+" · prepared")
		return plan.URL, nil
	default:
		return "", fmt.Errorf("Gateway setup plan is incomplete")
	}
}

func finalizeRemoteGateway(ctx context.Context, g gateway.Gateway, plan guidedGatewayPlan, gatewayStatePath string, presenter setupPresenter) error {
	tunnel := cloudflareapp.Tunnel{
		StatePath: filepath.Join(gatewayStatePath, "cloudflare"),
		Origin:    cloudflareapp.ComposeGatewayOrigin,
	}
	state, found, err := tunnel.Current()
	if err != nil {
		return err
	}
	managed := false
	if found {
		ownedURL, urlErr := cloudflareapp.GatewayURL(state.Hostname)
		if urlErr != nil {
			return urlErr
		}
		managed = ownedURL == plan.URL
	}
	if !managed {
		if err := g.CheckRemote(ctx, plan.URL); err != nil {
			return fmt.Errorf("existing Provider Gateway URL is not ready: %w", err)
		}
		return nil
	}
	presenter.Ready("Cloudflare Tunnel", state.Hostname+" · Compose active")
	return presenter.Run(ctx, "Checking the public Gateway route", func(ctx context.Context) error {
		managedGateway, err := managedRemoteGateway(g, state)
		if err != nil {
			return err
		}
		return waitForRemoteGateway(ctx, managedGateway, plan.URL)
	})
}

func managedRemoteGateway(g gateway.Gateway, state cloudflareapp.State) (gateway.Gateway, error) {
	probeURL, err := state.ProbeURL()
	if err != nil {
		return gateway.Gateway{}, err
	}
	g.DeploymentProbeURL = probeURL
	return g, nil
}

func waitForRemoteGateway(ctx context.Context, g gateway.Gateway, gatewayURL string) error {
	// The availability preflight may have cached an expected negative DNS
	// answer immediately before the Tunnel route was created. Readiness uses a
	// fresh public resolver so that setup does not wait for the host's negative
	// cache TTL before proving the new route.
	g.Client = freshDNSHTTPClient()
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for {
		if err := g.CheckRemote(ctx, gatewayURL); err == nil {
			return nil
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
					gatewayURL := "http://" + privateIPv4 + ":8317/v1"
					releaseTag, manifestPath, archive := releaseapp.OfficialImageRelease(), "", ""
					if options.IncusManifest != "" {
						releaseTag, manifestPath, archive = "", options.IncusManifest, options.IncusArchive
					}
					profile, _, _, installErr = reconcileOfficialIncusProfileDefinition(ctx, store, name, plan.Harness, releaseTag, manifestPath, archive,
						cfg.Incus, incus.DefaultProject, incus.DefaultStoragePool, guidedIncusNetwork, guidedIncusDiskSize, gatewayURL)
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
