package core

import (
	"strings"
	"testing"
	"time"
)

func TestSandboxProfileDefinitionHashBindsEveryRuntimeFact(t *testing.T) {
	profile := SandboxProfile{
		Name:                       "local-codex",
		Provider:                   SandboxProviderIncus,
		Harness:                    "codex",
		Artifact:                   strings.Repeat("a", 64),
		IncusEndpointAuthorityHash: strings.Repeat("b", 64),
		IncusProject:               "dorf",
		IncusStoragePool:           "default",
		IncusNetwork:               "incusbr0",
		IncusDiskSize:              "40GiB",
		IncusGatewayURL:            "http://10.10.10.1:8317/v1",
	}
	want := profile.CurrentDefinitionHash()
	if want != "e3f67bf6e1968cef3925f82c8da9773b37bb56a3b220803231580444619b4fda" {
		t.Fatalf("definition hash=%q", want)
	}
	changes := []func(*SandboxProfile){
		func(p *SandboxProfile) { p.Provider = SandboxProviderE2B },
		func(p *SandboxProfile) { p.Harness = "pi" },
		func(p *SandboxProfile) { p.Artifact = strings.Repeat("c", 64) },
		func(p *SandboxProfile) { p.IncusEndpointAuthorityHash = strings.Repeat("d", 64) },
		func(p *SandboxProfile) { p.IncusProject = "other" },
		func(p *SandboxProfile) { p.IncusStoragePool = "fast" },
		func(p *SandboxProfile) { p.IncusNetwork = "private0" },
		func(p *SandboxProfile) { p.IncusDiskSize = "80GiB" },
		func(p *SandboxProfile) { p.IncusGatewayURL = "https://gateway.example/v1" },
		func(p *SandboxProfile) { p.E2BGatewayURL = "https://e2b-gateway.example/v1" },
		func(p *SandboxProfile) { p.E2BSandboxTimeout = time.Hour },
		func(p *SandboxProfile) { p.E2BAllowInternet = true },
	}
	for index, change := range changes {
		candidate := profile
		change(&candidate)
		if got := candidate.CurrentDefinitionHash(); got == want {
			t.Fatalf("change %d did not change definition hash", index)
		}
	}
}

func TestSandboxProfileBaseVerifiedRequiresExactPersistedDefinition(t *testing.T) {
	profile := SandboxProfile{
		Provider:                   SandboxProviderIncus,
		Harness:                    "codex",
		Artifact:                   strings.Repeat("a", 64),
		IncusEndpointAuthorityHash: strings.Repeat("b", 64),
		IncusProject:               "dorf",
		IncusStoragePool:           "default",
		IncusNetwork:               "incusbr0",
		IncusDiskSize:              "40GiB",
		IncusGatewayURL:            "http://10.10.10.1:8317/v1",
	}
	settled := time.Unix(1_800_000_000, 0)
	profile.DefinitionHash = profile.CurrentDefinitionHash()
	profile.Verification = &ProfileVerification{
		ContractVersion:  BaseProfileContract,
		DefinitionHash:   profile.DefinitionHash,
		ProbeCompletedAt: settled,
		CleanedAt:        settled,
	}
	if !profile.BaseVerified() {
		t.Fatal("exact current definition was not verified")
	}
	profile.IncusNetwork = "changed"
	if profile.BaseVerified() {
		t.Fatal("stale verification remained eligible after an in-memory definition change")
	}
	profile.IncusNetwork = "incusbr0"
	profile.DefinitionHash = ""
	profile.Verification.DefinitionHash = ""
	if profile.BaseVerified() {
		t.Fatal("legacy rows without definition hashes became eligible")
	}
}
