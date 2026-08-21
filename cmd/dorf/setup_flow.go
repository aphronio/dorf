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
	Mode     setupGatewayMode
	URL      string
	Hostname string
}

func completeGuidedSetup(ctx context.Context, store postgres.Store, cfg *config.Config, options setupOptions, providers []core.SandboxProvider, presenter setupPresenter, stdout, stderr io.Writer) error {
	presenter.Section("Agent configuration")
	harness, err := setupHarness(ctx, options, presenter)
	if err != nil {
		return err
	}
	profilePlans, err := planGuidedProfiles(ctx, store, options, providers, harness)
	if err != nil {
		return err
	}
	bind, privateBridge, err := setupGatewayBind(ctx, store, profilePlans)
	if err != nil {
		return err
	}

	g := gateway.Gateway{StatePath: cfg.GatewayStatePath, PrivateBridge: privateBridge}
	connection, err := setupProviderConnection(ctx, g, bind, options, presenter)
	if err != nil {
		return err
	}
	presenter.Ready("OpenAI access", connection)

	gatewayURL := ""
	if containsSandboxProvider(providers, core.SandboxProviderE2B) {
		if err := setupE2BCredential(ctx, cfg, options, presenter); err != nil {
			return err
		}
		presenter.Ready("E2B access", "Project credential verified on this host")
		gatewayPlan, planErr := planRemoteGateway(ctx, options, profilePlans, presenter, net.DefaultResolver)
		if planErr != nil {
			return planErr
		}
		gatewayURL, err = setupRemoteGateway(ctx, g, bind, cfg.GatewayStatePath, gatewayPlan, options.Yes, presenter, stdout, stderr)
		if err != nil {
			return err
		}
		presenter.Ready("Cloud Gateway", gatewayURL)
	}

	presenter.Section("Sandbox profiles")
	profiles, err := setupProfiles(ctx, store, *cfg, options, profilePlans, gatewayURL, presenter)
	if err != nil {
		return err
	}
	defaultProfile, err := setupDefaultProfile(ctx, store, profiles, options.Yes, presenter)
	if err != nil {
		return err
	}
	if err := g.Check(ctx, connection); err != nil {
		return fmt.Errorf("verify Provider Connection %q: %w", connection, err)
	}
	presenter.Ready("Default profile", defaultProfile.Name+" · "+string(defaultProfile.Provider)+" · "+defaultProfile.Harness)
	presenter.Section("Ready")
	presenter.Ready("Dorf", "Agents can now run with "+defaultProfile.Name)
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

func setupGatewayBind(ctx context.Context, store postgres.Store, selected []guidedProfilePlan) (string, string, error) {
	profiles, err := store.SandboxProfiles(ctx)
	if err != nil {
		return "", "", err
	}
	network, err := gatewayIncusNetwork(profiles, selected)
	if err != nil {
		return "", "", err
	}
	if network == "" {
		return "127.0.0.1", "", nil
	}
	result, err := (incus.CommandRunner{}).Run(ctx, "incus", nil, "network", "get", network, "ipv4.address")
	if err != nil || result.ExitCode != 0 {
		return "", "", fmt.Errorf("resolve private Incus bridge %s for the Provider Gateway", network)
	}
	bind := strings.Split(strings.TrimSpace(result.Stdout), "/")[0]
	if bind == "" {
		return "", "", fmt.Errorf("Incus network %s returned no private IPv4 address", network)
	}
	return bind, network, nil
}

func gatewayIncusNetwork(profiles []core.SandboxProfile, selected []guidedProfilePlan) (string, error) {
	networks := map[string]struct{}{}
	for _, profile := range profiles {
		if profile.Provider == core.SandboxProviderIncus {
			networks[profile.IncusNetwork] = struct{}{}
		}
	}
	for _, profile := range selected {
		if profile.Provider != core.SandboxProviderIncus {
			continue
		}
		network := guidedIncusNetwork
		if profile.Existing != nil {
			network = profile.Existing.IncusNetwork
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

func setupProviderConnection(ctx context.Context, g gateway.Gateway, bind string, options setupOptions, presenter setupPresenter) (string, error) {
	if err := g.Provision(ctx, bind); err != nil {
		return "", fmt.Errorf("start the Provider Gateway: %w", err)
	}
	selectConnection := func() (string, error) {
		if options.Connection != "" {
			if err := g.Check(ctx, options.Connection); err != nil {
				return "", fmt.Errorf("existing AI connection %q is not ready: %w", options.Connection, err)
			}
			return options.Connection, nil
		}
		mode := options.ConnectionMode
		if mode == "" {
			if existing := unambiguousSetupConnection(func(name string) error { return g.Check(ctx, name) }); existing != "" {
				return existing, nil
			}
		}
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
			if g.Check(ctx, name) == nil {
				return name, nil
			}
			if err := g.ConnectChatGPT(ctx, name, bind, func(url, code string) {
				presenter.Note("Device sign-in", "Open "+url+" and enter "+code)
			}); err != nil {
				return "", err
			}
			return name, nil
		case setupConnectionOpenAI:
			const name = "openai-api"
			if g.Check(ctx, name) == nil {
				return name, nil
			}
			key := ""
			var err error
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
	connection, err := selectConnection()
	if err != nil {
		return "", err
	}
	if err := g.SetDefaultConnection(connection); err != nil {
		return "", fmt.Errorf("select default AI connection %q: %w", connection, err)
	}
	return connection, nil
}

func unambiguousSetupConnection(check func(string) error) string {
	chatGPT := check("personal-chatgpt") == nil
	openAI := check("openai-api") == nil
	switch {
	case chatGPT && !openAI:
		return "personal-chatgpt"
	case openAI && !chatGPT:
		return "openai-api"
	default:
		return ""
	}
}

func setupE2BCredential(ctx context.Context, cfg *config.Config, options setupOptions, presenter setupPresenter) error {
	key := strings.TrimSpace(cfg.E2BAPIKey)
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
	if options.E2BKeyFile == "" && strings.TrimSpace(cfg.E2BAPIKey) != "" {
		return nil
	}
	if cfg.DatabaseExternal {
		return fmt.Errorf("guided E2B credential storage is unavailable with external PostgreSQL; set E2B_API_KEY on the deployment")
	}
	if err := deployment.SaveE2BAPIKey(cfg.DeploymentPath, key); err != nil {
		return err
	}
	cfg.E2BAPIKey = key
	return nil
}

func planRemoteGateway(ctx context.Context, options setupOptions, profiles []guidedProfilePlan, presenter setupPresenter, resolver dnsResolver) (guidedGatewayPlan, error) {
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
			return guidedGatewayPlan{Mode: setupGatewayCloudflare, URL: wanted, Hostname: options.CloudflareHost}, nil
		}
		return guidedGatewayPlan{Mode: setupGatewayExisting, URL: existingURL}, nil
	}

	plan := guidedGatewayPlan{}
	interactiveHostname := false
	cloudflareDNSConfirmed := false
	switch {
	case options.CloudflareHost != "":
		plan.Mode, plan.Hostname = setupGatewayCloudflare, options.CloudflareHost
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
				presenter.Note("Gateway ingress", plan.Hostname+" already resolves; Dorf will not replace its DNS")
			default:
				cloudflareDNSConfirmed = true
				plan.Mode = setupGatewayCloudflare
				if err := presenter.RunForm(ctx, presenter.CloudflareGatewayGroup(&plan.Mode, delegation.Zone)); err != nil {
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
		if !cloudflareDNSConfirmed {
			if _, err := requireCloudflareDNS(ctx, resolver, plan.Hostname); err != nil {
				return guidedGatewayPlan{}, err
			}
			occupied, err := hostnameHasAddresses(ctx, resolver, plan.Hostname)
			if err != nil {
				return guidedGatewayPlan{}, fmt.Errorf("inspect address records for %s: %w", plan.Hostname, err)
			}
			if occupied {
				return guidedGatewayPlan{}, fmt.Errorf("%s already resolves; use --gateway-url for existing ingress or choose an unused --cloudflare-hostname", plan.Hostname)
			}
		}
		return plan, nil
	default:
		return guidedGatewayPlan{}, fmt.Errorf("Gateway setup must use Cloudflare Tunnel or an existing HTTPS URL")
	}
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

func setupRemoteGateway(ctx context.Context, g gateway.Gateway, bind, gatewayStatePath string, plan guidedGatewayPlan, yes bool, presenter setupPresenter, stdout, stderr io.Writer) (string, error) {
	switch plan.Mode {
	case setupGatewayExisting:
		origin := "http://" + bind + ":8317"
		tunnel := cloudflareapp.Tunnel{StatePath: filepath.Join(gatewayStatePath, "cloudflare"), Origin: origin}
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
				presenter.Note("Cloud Gateway", "Reconciling the existing Cloudflare Tunnel and host service")
				if _, err := tunnel.Reconcile(ctx, state.Hostname, stdout, stderr); err != nil {
					return "", err
				}
				presenter.Ready("Cloudflare Tunnel", state.Hostname+" · service active")
				if err := presenter.Run(ctx, "Checking the public Gateway route", func(ctx context.Context) error {
					return waitForRemoteGateway(ctx, g, plan.URL)
				}); err != nil {
					return "", err
				}
				return plan.URL, nil
			}
		}
		if err := g.CheckRemote(ctx, plan.URL); err != nil {
			return "", fmt.Errorf("existing Provider Gateway URL is not ready: %w", err)
		}
		return plan.URL, nil
	case setupGatewayCloudflare:
		origin := "http://" + bind + ":8317"
		confirmed := yes
		if !confirmed {
			description := fmt.Sprintf("Create one named Tunnel, route %s to %s, install cloudflared as a host service, and remove the temporary account credential.", plan.Hostname, origin)
			if err := presenter.RunForm(ctx, presenter.ConfirmGroup("Review Cloudflare changes", description, &confirmed)); err != nil {
				return "", err
			}
			if !confirmed {
				return "", errSetupCancelled
			}
		}
		tunnel := cloudflareapp.Tunnel{StatePath: filepath.Join(gatewayStatePath, "cloudflare"), Origin: origin}
		presenter.Note("Cloud Gateway", "Reconciling the Cloudflare Tunnel, DNS, and host service")
		if _, err := tunnel.Reconcile(ctx, plan.Hostname, stdout, stderr); err != nil {
			return "", err
		}
		presenter.Ready("Cloudflare Tunnel", plan.Hostname+" · service active")
		if err := presenter.Run(ctx, "Checking the public Gateway route", func(ctx context.Context) error {
			return waitForRemoteGateway(ctx, g, plan.URL)
		}); err != nil {
			return "", err
		}
		return plan.URL, nil
	default:
		return "", fmt.Errorf("Gateway setup plan is incomplete")
	}
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

func setupProfiles(ctx context.Context, store postgres.Store, cfg config.Config, options setupOptions, plans []guidedProfilePlan, gatewayURL string, presenter setupPresenter) ([]core.SandboxProfile, error) {
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
					profile, _, _, installErr = reconcileOfficialIncusProfile(ctx, store, name, plan.Harness, "v"+version.Version, "", "", guidedIncusNetwork, guidedIncusDiskSize)
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
		if !profile.BaseVerified() {
			err := presenter.Run(ctx, "Verifying "+profile.Name, func(ctx context.Context) error {
				var verifyErr error
				profile, verifyErr = profileapp.VerifyBase(ctx, store, func(profile core.SandboxProfile) (providerapi.Sandbox, error) {
					return sandboxForProfile(cfg, profile)
				}, profile.Name)
				return verifyErr
			})
			if err != nil {
				return nil, err
			}
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
