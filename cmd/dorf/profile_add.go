package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	cloudflareapp "github.com/aphronio/dorf/internal/cloudflare"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/postgres"
)

type profileAddOptions struct {
	Name          string
	Provider      core.SandboxProvider
	Harness       string
	GatewayURL    string
	AllowInternet bool
	SetDefault    bool
}

type preparedProfileAdd struct {
	Options     profileAddOptions
	Presenter   setupPresenter
	Inventory   []core.SandboxProfile
	Plans       []guidedProfilePlan
	GatewayURL  string
	PrivateIPv4 string
}

func parseProfileAddOptions(args []string, stderr io.Writer) (profileAddOptions, error) {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = strings.TrimSpace(args[0])
		args = args[1:]
	}
	set := flag.NewFlagSet("profile add", flag.ContinueOnError)
	set.SetOutput(stderr)
	provider := set.String("sandbox-provider", "", "Sandbox provider: incus or e2b")
	harness := set.String("harness", "", "Harness: codex or pi")
	gatewayURL := set.String("gateway-url", "", "existing stable HTTPS /v1 Provider Gateway URL")
	allowInternet := set.Bool("allow-internet", false, "allow E2B Sandbox internet egress")
	setDefault := set.Bool("set-default", false, "make the added profile the deployment default")
	if err := set.Parse(args); err != nil {
		return profileAddOptions{}, err
	}
	if set.NArg() > 1 || (name != "" && set.NArg() != 0) {
		return profileAddOptions{}, fmt.Errorf("profile add accepts at most one profile name")
	}
	if name == "" && set.NArg() == 1 {
		name = strings.TrimSpace(set.Arg(0))
	}
	return profileAddOptions{
		Name: name, Provider: core.SandboxProvider(strings.ToLower(strings.TrimSpace(*provider))), Harness: strings.ToLower(strings.TrimSpace(*harness)),
		GatewayURL: strings.TrimSpace(*gatewayURL), AllowInternet: *allowInternet, SetDefault: *setDefault,
	}, nil
}

func profileAddCommand(ctx context.Context, store postgres.Store, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	prepared, err := prepareProfileAdd(ctx, store, cfg, args, stdout, stderr)
	if err != nil {
		return err
	}
	if prepared.Presenter.interactive {
		prepared.Presenter.Section("Sandbox profile")
	}
	profiles, err := setupProfiles(ctx, store, cfg, setupOptions{
		ProfileName: prepared.Options.Name, Harness: prepared.Options.Harness, GatewayURL: prepared.GatewayURL,
		AllowInternet: prepared.Options.AllowInternet,
	}, prepared.Plans, prepared.GatewayURL, prepared.PrivateIPv4, prepared.Presenter)
	if err != nil {
		return err
	}
	profile, defaultName, err := settleProfileAddDefault(ctx, store, profiles[0], prepared.Options, prepared.Inventory, prepared.Presenter)
	if err != nil {
		return err
	}
	return writeProfileAddResult(stdout, prepared.Presenter, profile, defaultName)
}

func prepareProfileAdd(ctx context.Context, store postgres.Store, cfg config.Config, args []string, stdout, stderr io.Writer) (preparedProfileAdd, error) {
	options, err := parseProfileAddOptions(args, stderr)
	if err != nil {
		return preparedProfileAdd{}, err
	}
	presenter := newSetupPresenter(stdout)
	options, err = resolveProfileAddOptions(ctx, cfg, options, presenter)
	if err != nil {
		return preparedProfileAdd{}, err
	}
	if err := requireProfileAddProviderConfiguration(cfg, options); err != nil {
		return preparedProfileAdd{}, err
	}

	inventory, err := store.SandboxProfiles(ctx)
	if err != nil {
		return preparedProfileAdd{}, err
	}
	gatewayURL, privateIPv4, err := resolveProfileAddGateway(cfg, options, inventory)
	if err != nil {
		return preparedProfileAdd{}, err
	}
	setupOptions := setupOptions{
		ProfileName: options.Name, Harness: options.Harness, GatewayURL: gatewayURL,
		AllowInternet: options.AllowInternet,
	}
	plans, err := planGuidedProfiles(ctx, store, setupOptions, []core.SandboxProvider{options.Provider}, options.Harness, cfg.Incus)
	if err != nil {
		return preparedProfileAdd{}, err
	}
	if err := setupGuidedIncusReadiness(ctx, plans, cfg.Incus); err != nil {
		return preparedProfileAdd{}, err
	}
	if options.Provider == core.SandboxProviderE2B {
		if err := presenter.Run(ctx, "Checking E2B access", func(ctx context.Context) error {
			return (e2b.Client{APIKey: cfg.E2BAPIKey}).Check(ctx)
		}); err != nil {
			return preparedProfileAdd{}, fmt.Errorf("verify E2B project credential: %w", err)
		}
	}
	return preparedProfileAdd{
		Options: options, Presenter: presenter, Inventory: inventory, Plans: plans,
		GatewayURL: gatewayURL, PrivateIPv4: privateIPv4,
	}, nil
}

func settleProfileAddDefault(ctx context.Context, store postgres.Store, profile core.SandboxProfile, options profileAddOptions, inventory []core.SandboxProfile, presenter setupPresenter) (core.SandboxProfile, string, error) {
	defaultName := currentDefaultProfileName(inventory)
	makeDefault := options.SetDefault || defaultName == ""
	if presenter.interactive && !makeDefault && defaultName != profile.Name {
		if err := presenter.RunForm(ctx, presenter.ProfileDefaultGroup(profile.Name, defaultName, &makeDefault)); err != nil {
			return core.SandboxProfile{}, "", fmt.Errorf("select default Sandbox profile: %w", err)
		}
	}
	var err error
	if makeDefault {
		profile, err = store.SetDefaultSandboxProfile(ctx, profile.Name)
		if err != nil {
			return core.SandboxProfile{}, "", err
		}
		defaultName = profile.Name
	} else {
		profile, err = store.SandboxProfile(ctx, profile.Name)
		if err != nil {
			return core.SandboxProfile{}, "", err
		}
	}
	return profile, defaultName, nil
}

func writeProfileAddResult(stdout io.Writer, presenter setupPresenter, profile core.SandboxProfile, defaultName string) error {
	if presenter.interactive {
		presenter.Ready("Sandbox profile", setupProfileSummary(profile))
		if defaultName == profile.Name {
			presenter.Ready("Default profile", profile.Name)
		} else {
			presenter.Note("Default profile", defaultName+" remains the default")
		}
		return nil
	}
	return writeJSON(stdout, map[string]any{
		"profile": profileView(profile), "verified": profile.BaseVerified(), "default": profile.Default,
	})
}

func resolveProfileAddOptions(ctx context.Context, cfg config.Config, options profileAddOptions, presenter setupPresenter) (profileAddOptions, error) {
	provider, err := resolveProfileAddProvider(ctx, cfg, options.Provider, presenter)
	if err != nil {
		return profileAddOptions{}, err
	}
	options.Provider = provider
	if options.Provider != core.SandboxProviderIncus && options.Provider != core.SandboxProviderE2B {
		return profileAddOptions{}, fmt.Errorf("profile add requires --sandbox-provider incus or --sandbox-provider e2b")
	}
	if options.AllowInternet && options.Provider != core.SandboxProviderE2B {
		return profileAddOptions{}, fmt.Errorf("--allow-internet requires --sandbox-provider e2b")
	}
	if options.Harness == "" {
		options.Harness = "codex"
		if presenter.interactive {
			if err := presenter.RunForm(ctx, presenter.HarnessGroup(&options.Harness)); err != nil {
				return profileAddOptions{}, fmt.Errorf("select Harness: %w", err)
			}
		}
	}
	if options.Harness != "codex" && options.Harness != "pi" {
		return profileAddOptions{}, fmt.Errorf("profile add requires --harness codex or --harness pi")
	}
	if options.Name == "" {
		if options.Provider == core.SandboxProviderIncus {
			options.Name = guidedIncusProfileName(options.Harness, cfg.Incus)
		} else {
			options.Name = "cloud-" + options.Harness
		}
	}
	if err := postgres.ValidateSandboxProfileIdentity(options.Name, options.Harness); err != nil {
		return profileAddOptions{}, err
	}
	return options, nil
}

func resolveProfileAddProvider(ctx context.Context, cfg config.Config, provider core.SandboxProvider, presenter setupPresenter) (core.SandboxProvider, error) {
	if provider != "" {
		return provider, nil
	}
	available := configuredProfileAddProviders(cfg)
	switch {
	case !presenter.interactive:
		return "", fmt.Errorf("profile add requires --sandbox-provider incus or --sandbox-provider e2b")
	case len(available) == 0:
		return "", fmt.Errorf("no Sandbox provider access is configured; run dorf setup")
	case len(available) == 1:
		return available[0], nil
	default:
		provider = available[0]
		if err := presenter.RunForm(ctx, presenter.ProfileProviderGroup(&provider, available)); err != nil {
			return "", fmt.Errorf("select Sandbox provider: %w", err)
		}
		return provider, nil
	}
}

func configuredProfileAddProviders(cfg config.Config) []core.SandboxProvider {
	providers := make([]core.SandboxProvider, 0, 2)
	if cfg.Incus != nil {
		providers = append(providers, core.SandboxProviderIncus)
	}
	if strings.TrimSpace(cfg.E2BAPIKey) != "" {
		providers = append(providers, core.SandboxProviderE2B)
	}
	return providers
}

func requireProfileAddProviderConfiguration(cfg config.Config, options profileAddOptions) error {
	switch options.Provider {
	case core.SandboxProviderE2B:
		if strings.TrimSpace(cfg.E2BAPIKey) == "" {
			return fmt.Errorf("E2B access is not configured; run dorf setup")
		}
	case core.SandboxProviderIncus:
		if cfg.Incus == nil {
			return fmt.Errorf("Incus authority is not configured; run dorf setup")
		}
	}
	return nil
}

func resolveProfileAddGateway(cfg config.Config, options profileAddOptions, profiles []core.SandboxProfile) (string, string, error) {
	if options.Provider == core.SandboxProviderIncus && cfg.Incus != nil && strings.HasPrefix(cfg.Incus.Endpoint, "unix://") {
		address, err := resolveLocalProfileAddGateway(*cfg.Incus, profiles)
		return "", address, err
	}
	gatewayURL, err := resolveRemoteProfileAddGateway(cfg, options.GatewayURL, profiles)
	return gatewayURL, "", err
}

func resolveLocalProfileAddGateway(authority deployment.Incus, profiles []core.SandboxProfile) (string, error) {
	authorityHash, err := authority.AuthorityHash()
	if err != nil {
		return "", err
	}
	addresses := map[string]struct{}{}
	for _, profile := range profiles {
		if profile.IncusEndpointAuthorityHash != authorityHash {
			continue
		}
		if address, found := guidedIncusProfilePublishAddress(profile); found {
			addresses[address] = struct{}{}
		}
	}
	if len(addresses) != 1 {
		return "", fmt.Errorf("local Incus Sandbox routing is not configured unambiguously; run dorf setup")
	}
	for address := range addresses {
		return address, nil
	}
	return "", fmt.Errorf("local Incus Sandbox routing is not configured; run dorf setup")
}

func resolveRemoteProfileAddGateway(cfg config.Config, requested string, profiles []core.SandboxProfile) (string, error) {
	if requested != "" {
		return normalizeExactGatewayURL(requested)
	}
	if strings.TrimSpace(cfg.GatewayStatePath) != "" {
		owned, err := currentOwnedCloudflareEndpoints(cfg.GatewayStatePath)
		if err != nil {
			return "", err
		}
		if owned.ModelHostname != "" {
			return cloudflareapp.GatewayURL(owned.ModelHostname)
		}
	}
	plans := make([]guidedProfilePlan, 0, len(profiles))
	for index := range profiles {
		profile := &profiles[index]
		plans = append(plans, guidedProfilePlan{Provider: profile.Provider, Name: profile.Name, Harness: profile.Harness, Existing: profile})
	}
	gatewayURL, err := retainedGuidedRemoteGatewayURL(plans)
	if err != nil {
		return "", err
	}
	if gatewayURL == "" {
		return "", fmt.Errorf("a stable model Gateway is not configured; run dorf setup")
	}
	return gatewayURL, nil
}

func currentDefaultProfileName(profiles []core.SandboxProfile) string {
	for _, profile := range profiles {
		if profile.Default {
			return profile.Name
		}
	}
	return ""
}
