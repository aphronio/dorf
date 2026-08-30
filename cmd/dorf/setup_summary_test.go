package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
)

func TestSetupProfileSummaryNamesEachField(t *testing.T) {
	profile := core.SandboxProfile{Name: "incus-codex", Provider: core.SandboxProviderIncus, Harness: "codex"}

	got := setupProfileSummary(profile)
	for _, want := range []string{"incus-codex", "Incus provider", "Codex Harness", "verification required"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q does not name %q", got, want)
		}
	}
	if strings.Contains(got, " · ") {
		t.Fatalf("summary remains an unlabeled tuple: %q", got)
	}
}

func TestSetupProfileInventoryExplainsConfiguredE2BWithoutProfile(t *testing.T) {
	var output bytes.Buffer
	presenter := setupPresenter{output: &output}
	profiles := []core.SandboxProfile{{
		Name: "incus-codex", Provider: core.SandboxProviderIncus, Harness: "codex",
	}}

	presentSetupProfileInventory(presenter, config.Config{E2BAPIKey: "configured-secret"}, profiles)

	got := output.String()
	for _, want := range []string{
		"Sandbox profile: incus-codex uses the Incus provider with the Codex Harness; verification required",
		"E2B profile: Not configured. Run dorf profile add --sandbox-provider e2b",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("inventory output %q does not contain %q", got, want)
		}
	}
}
