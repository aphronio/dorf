package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestParseProfileAddOptionsKeepsTheFriendlySurfaceSmall(t *testing.T) {
	var stderr bytes.Buffer
	options, err := parseProfileAddOptions([]string{
		"cloud-pi",
		"--sandbox-provider", "e2b",
		"--harness", "pi",
		"--allow-internet",
		"--set-default",
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if options.Name != "cloud-pi" || options.Provider != "e2b" || options.Harness != "pi" || !options.AllowInternet || !options.SetDefault {
		t.Fatalf("options=%#v", options)
	}
}

func TestProfileAddDefaultsToTheOfficialProfileIdentity(t *testing.T) {
	options, err := resolveProfileAddOptions(context.Background(), config.Config{}, profileAddOptions{
		Provider: core.SandboxProviderE2B,
	}, setupPresenter{})
	if err != nil {
		t.Fatal(err)
	}
	if options.Name != "cloud-codex" || options.Harness != "codex" {
		t.Fatalf("options=%#v", options)
	}
}

func TestProfileAddReusesTheConfiguredGatewayWithoutChangingProfiles(t *testing.T) {
	profiles := []core.SandboxProfile{{
		Name: "cloud-pi", Provider: core.SandboxProviderE2B, Harness: "pi",
		E2BGatewayURL: "https://models.example.test/v1",
	}}
	gatewayURL, privateIPv4, err := resolveProfileAddGateway(config.Config{}, profileAddOptions{Provider: core.SandboxProviderE2B}, profiles)
	if err != nil || gatewayURL != "https://models.example.test/v1" || privateIPv4 != "" {
		t.Fatalf("gateway=%q private=%q error=%v", gatewayURL, privateIPv4, err)
	}
	if profiles[0].E2BGatewayURL != "https://models.example.test/v1" {
		t.Fatalf("profile changed=%#v", profiles[0])
	}

	local := config.Config{Incus: &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}}
	authorityHash, err := local.Incus.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	profiles = []core.SandboxProfile{{
		Name: "local-codex", Provider: core.SandboxProviderIncus, Harness: "codex",
		IncusEndpointAuthorityHash: authorityHash, IncusGatewayURL: "http://10.44.0.1:8317/v1",
	}}
	gatewayURL, privateIPv4, err = resolveProfileAddGateway(local, profileAddOptions{Provider: core.SandboxProviderIncus}, profiles)
	if err != nil || gatewayURL != "" || privateIPv4 != "10.44.0.1" {
		t.Fatalf("local gateway=%q private=%q error=%v", gatewayURL, privateIPv4, err)
	}
}

func TestProfileAddHandsMissingProviderConfigurationBackToSetup(t *testing.T) {
	for _, test := range []struct {
		provider string
		want     string
	}{
		{provider: "e2b", want: "E2B access is not configured; run dorf setup"},
		{provider: "incus", want: "Incus authority is not configured; run dorf setup"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := profileCommand(context.Background(), postgres.Store{}, config.Config{}, []string{
				"add", "--sandbox-provider", test.provider, "--harness", "codex",
			}, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout=%q", stdout.String())
			}
		})
	}
}
