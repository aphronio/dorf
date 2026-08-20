package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/doctor"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/postgres"
	profileapp "github.com/aphronio/dorf/internal/profile"
	releaseapp "github.com/aphronio/dorf/internal/release"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func profileCommand(ctx context.Context, store postgres.Store, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("profile requires: create, install, update, verify, set-default, show, or list")
	}
	switch args[0] {
	case "install":
		return installOfficialIncusProfile(ctx, store, args[1:], stdout, stderr)
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("profile create requires NAME")
		}
		profile, err := parseSandboxProfile(ctx, "create "+args[1], args[1], args[2:], stderr)
		if err != nil {
			return err
		}
		stored, created, err := store.CreateSandboxProfile(ctx, profile)
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"profile": profileView(stored), "created": created})
	case "update":
		if len(args) < 2 {
			return fmt.Errorf("profile update requires NAME")
		}
		current, err := store.SandboxProfile(ctx, args[1])
		if err != nil {
			return err
		}
		patch, err := parseSandboxProfilePatch(ctx, "update "+args[1], current.Provider, args[2:], stderr)
		if err != nil {
			return err
		}
		stored, updated, err := store.UpdateSandboxProfile(ctx, args[1], patch)
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"profile": profileView(stored), "updated": updated})
	case "verify":
		if len(args) != 2 {
			return fmt.Errorf("profile verify requires NAME")
		}
		verified, err := profileapp.VerifyBase(ctx, store, func(profile core.SandboxProfile) (provider.Sandbox, error) {
			return sandboxForProfile(cfg, profile)
		}, args[1])
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"profile": profileView(verified), "verified": true})
	case "set-default":
		if len(args) != 2 {
			return fmt.Errorf("profile set-default requires NAME")
		}
		profile, err := store.SetDefaultSandboxProfile(ctx, args[1])
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"profile": profile.Name, "default": true})
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("profile show requires NAME")
		}
		profile, err := store.SandboxProfile(ctx, args[1])
		if err != nil {
			return err
		}
		return writeJSON(stdout, profileView(profile))
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("profile list takes no arguments")
		}
		profiles, err := store.SandboxProfiles(ctx)
		if err != nil {
			return err
		}
		views := make([]sandboxProfileView, 0, len(profiles))
		for _, profile := range profiles {
			views = append(views, profileView(profile))
		}
		return writeJSON(stdout, views)
	default:
		return fmt.Errorf("unsupported profile command %q", args[0])
	}
}

func installOfficialIncusProfile(ctx context.Context, store postgres.Store, args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("profile install requires NAME")
	}
	name := strings.TrimSpace(args[0])
	set := flag.NewFlagSet("profile install "+name, flag.ContinueOnError)
	set.SetOutput(stderr)
	harness := set.String("harness", "", "Harness: codex or pi")
	network := set.String("network", "incusbr0", "Incus network")
	diskSize := set.String("disk-size", "40GiB", "Incus root disk size")
	manifestPath := set.String("manifest", "", "verified release image manifest")
	archive := set.String("archive", "", "matching Incus VM archive")
	releaseTag := set.String("release", "", "immutable Dorf GitHub release tag")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	local := *manifestPath != "" || *archive != ""
	if (*releaseTag == "" && !local) || (*releaseTag != "" && local) || (local && (*manifestPath == "" || *archive == "")) {
		return fmt.Errorf("profile install requires exactly --release or both --manifest and --archive")
	}
	if err := postgres.ValidateSandboxProfileIdentity(name, *harness); err != nil {
		return err
	}
	alias := "dorf-profile-" + name
	var installed releaseapp.Manifest
	var err error
	if *releaseTag != "" {
		installed, err = releaseapp.InstallPublishedImage(ctx, *releaseTag, alias)
	} else {
		installed, err = releaseapp.InstallImage(ctx, *manifestPath, *archive, alias)
	}
	if err != nil {
		return err
	}
	profile, created, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderIncus, Harness: *harness,
		Artifact: installed.ImageFingerprint, IncusNetwork: *network, IncusDiskSize: *diskSize,
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{
		"profile": profileView(profile), "created": created, "official_release": installed.ReleaseTag,
		"next": "dorf profile verify " + profile.Name,
	})
}

type sandboxProfileView struct {
	Name              string                   `json:"name"`
	Provider          core.SandboxProvider     `json:"provider"`
	Harness           string                   `json:"harness"`
	Artifact          string                   `json:"artifact"`
	IncusNetwork      string                   `json:"incus_network,omitempty"`
	IncusDiskSize     string                   `json:"incus_disk_size,omitempty"`
	E2BGatewayURL     string                   `json:"e2b_gateway_url,omitempty"`
	E2BSandboxTimeout string                   `json:"e2b_sandbox_timeout,omitempty"`
	E2BAllowInternet  *bool                    `json:"e2b_allow_internet,omitempty"`
	Default           bool                     `json:"default"`
	Verified          bool                     `json:"verified"`
	CreatedAt         time.Time                `json:"created_at"`
	Verification      *profileVerificationView `json:"verification,omitempty"`
}

type profileVerificationView struct {
	ContractVersion string    `json:"contract_version"`
	HarnessVersion  string    `json:"harness_version,omitempty"`
	AttemptedAt     time.Time `json:"attempted_at"`
	VerifiedAt      time.Time `json:"verified_at,omitempty"`
	CleanedAt       time.Time `json:"cleaned_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
}

func profileView(profile core.SandboxProfile) sandboxProfileView {
	view := sandboxProfileView{
		Name: profile.Name, Provider: profile.Provider, Harness: profile.Harness, Artifact: profile.Artifact,
		IncusNetwork: profile.IncusNetwork, IncusDiskSize: profile.IncusDiskSize,
		E2BGatewayURL: profile.E2BGatewayURL,
		Default:       profile.Default, Verified: profile.BaseVerified(), CreatedAt: profile.CreatedAt,
	}
	if profile.E2BSandboxTimeout > 0 {
		view.E2BSandboxTimeout = profile.E2BSandboxTimeout.String()
	}
	if profile.Provider == core.SandboxProviderE2B {
		allowInternet := profile.E2BAllowInternet
		view.E2BAllowInternet = &allowInternet
	}
	if profile.Verification != nil {
		view.Verification = &profileVerificationView{
			ContractVersion: profile.Verification.ContractVersion, HarnessVersion: profile.Verification.HarnessVersion,
			AttemptedAt: profile.Verification.AttemptedAt, VerifiedAt: profile.Verification.ProbeCompletedAt,
			CleanedAt: profile.Verification.CleanedAt, LastError: profile.Verification.LastError,
		}
	}
	return view
}

func parseSandboxProfile(ctx context.Context, command, name string, args []string, stderr io.Writer) (core.SandboxProfile, error) {
	set := flag.NewFlagSet("profile "+command, flag.ContinueOnError)
	set.SetOutput(stderr)
	provider := set.String("provider", "", "Sandbox provider: incus or e2b")
	harness := set.String("harness", "", "Harness: codex or pi")
	image := set.String("image", "", "existing Incus image alias or fingerprint")
	network := set.String("network", "incusbr0", "Incus network")
	diskSize := set.String("disk-size", "40GiB", "Incus root disk size")
	template := set.String("template", "", "exact E2B template build reference")
	gatewayURL := set.String("gateway-url", "", "deployment-owned HTTPS Provider Gateway URL")
	sandboxTimeout := set.Duration("sandbox-timeout", 55*time.Minute, "E2B running timeout")
	allowInternet := set.Bool("allow-internet", false, "allow E2B Sandbox internet egress")
	if err := set.Parse(args); err != nil {
		return core.SandboxProfile{}, err
	}
	if set.NArg() != 0 {
		return core.SandboxProfile{}, fmt.Errorf("profile %s received unexpected arguments", command)
	}
	profile := core.SandboxProfile{Name: name, Provider: core.SandboxProvider(strings.TrimSpace(*provider)), Harness: strings.TrimSpace(*harness)}
	switch profile.Provider {
	case core.SandboxProviderIncus:
		if strings.TrimSpace(*template) != "" || strings.TrimSpace(*gatewayURL) != "" {
			return core.SandboxProfile{}, fmt.Errorf("Incus profile does not accept E2B template or Gateway flags")
		}
		fingerprint, err := incus.ResolveImageFingerprint(ctx, *image, nil)
		if err != nil {
			return core.SandboxProfile{}, err
		}
		profile.Artifact, profile.IncusNetwork, profile.IncusDiskSize = fingerprint, *network, *diskSize
	case core.SandboxProviderE2B:
		if strings.TrimSpace(*image) != "" {
			return core.SandboxProfile{}, fmt.Errorf("E2B profile does not accept --image")
		}
		profile.Artifact, profile.E2BGatewayURL = *template, *gatewayURL
		profile.E2BSandboxTimeout, profile.E2BAllowInternet = *sandboxTimeout, *allowInternet
	default:
		return core.SandboxProfile{}, fmt.Errorf("profile requires --provider incus or --provider e2b")
	}
	return profile, nil
}

func parseSandboxProfilePatch(ctx context.Context, command string, provider core.SandboxProvider, args []string, stderr io.Writer) (postgres.SandboxProfilePatch, error) {
	set := flag.NewFlagSet("profile "+command, flag.ContinueOnError)
	set.SetOutput(stderr)
	harness := set.String("harness", "", "Harness: codex or pi")
	image := set.String("image", "", "existing Incus image alias or fingerprint")
	network := set.String("network", "", "Incus network")
	diskSize := set.String("disk-size", "", "Incus root disk size")
	template := set.String("template", "", "exact E2B template build reference")
	gatewayURL := set.String("gateway-url", "", "deployment-owned HTTPS Provider Gateway URL")
	sandboxTimeout := set.Duration("sandbox-timeout", 0, "E2B running timeout")
	allowInternet := set.Bool("allow-internet", false, "allow E2B Sandbox internet egress")
	if err := set.Parse(args); err != nil {
		return postgres.SandboxProfilePatch{}, err
	}
	if set.NArg() != 0 {
		return postgres.SandboxProfilePatch{}, fmt.Errorf("profile %s received unexpected arguments", command)
	}
	visited := map[string]bool{}
	set.Visit(func(value *flag.Flag) { visited[value.Name] = true })
	if len(visited) == 0 {
		return postgres.SandboxProfilePatch{}, fmt.Errorf("profile %s requires at least one field flag", command)
	}
	switch provider {
	case core.SandboxProviderIncus:
		if visited["template"] || visited["gateway-url"] || visited["sandbox-timeout"] || visited["allow-internet"] {
			return postgres.SandboxProfilePatch{}, fmt.Errorf("Incus profile update does not accept E2B fields")
		}
	case core.SandboxProviderE2B:
		if visited["image"] || visited["network"] || visited["disk-size"] {
			return postgres.SandboxProfilePatch{}, fmt.Errorf("E2B profile update does not accept Incus fields")
		}
	default:
		return postgres.SandboxProfilePatch{}, fmt.Errorf("unsupported Sandbox provider %q", provider)
	}
	var patch postgres.SandboxProfilePatch
	if visited["harness"] {
		patch.Harness = harness
	}
	if visited["image"] {
		fingerprint, err := incus.ResolveImageFingerprint(ctx, *image, nil)
		if err != nil {
			return postgres.SandboxProfilePatch{}, err
		}
		patch.IncusArtifact = &fingerprint
	}
	if visited["network"] {
		patch.IncusNetwork = network
	}
	if visited["disk-size"] {
		patch.IncusDiskSize = diskSize
	}
	if visited["template"] {
		patch.E2BArtifact = template
	}
	if visited["gateway-url"] {
		patch.E2BGatewayURL = gatewayURL
	}
	if visited["sandbox-timeout"] {
		patch.E2BSandboxTimeout = sandboxTimeout
	}
	if visited["allow-internet"] {
		patch.E2BAllowInternet = allowInternet
	}
	return patch, nil
}

func selectedSandboxProfile(ctx context.Context, store postgres.Store, name string) (core.SandboxProfile, error) {
	profile, err := sandboxProfileByNameOrDefault(ctx, store, name)
	if err != nil {
		return core.SandboxProfile{}, err
	}
	if !profile.BaseVerified() {
		return core.SandboxProfile{}, fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup; run dorf profile verify %s", profile.Name, core.BaseProfileContract, profile.Name)
	}
	return profile, nil
}

func sandboxProfileByNameOrDefault(ctx context.Context, store postgres.Store, name string) (core.SandboxProfile, error) {
	var profile core.SandboxProfile
	var err error
	if strings.TrimSpace(name) == "" {
		profile, err = store.DefaultSandboxProfile(ctx)
	} else {
		profile, err = store.SandboxProfile(ctx, name)
	}
	if err != nil {
		return core.SandboxProfile{}, err
	}
	return profile, nil
}

func appendProfileVerificationCheck(checks []doctor.Check, profile core.SandboxProfile) []doctor.Check {
	check := doctor.Check{Name: "sandbox-profile-verification", Status: "failed"}
	if profile.BaseVerified() {
		check.Status = "ready"
		check.Detail = fmt.Sprintf("%s verified; Harness %s", core.BaseProfileContract, profile.Verification.HarnessVersion)
	} else {
		check.Detail = fmt.Sprintf("profile %s has not completed %s verification and cleanup; run dorf profile verify %s", profile.Name, core.BaseProfileContract, profile.Name)
	}
	return append(checks, check)
}
