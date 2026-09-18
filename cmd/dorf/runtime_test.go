package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/incus"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/terminal"
)

func TestConfiguredObservationsSurviveUnavailableExport(t *testing.T) {
	for _, test := range []struct {
		name, endpoint, attributes string
		warning                    bool
	}{
		{name: "disabled"},
		{name: "invalid resource", endpoint: "http://127.0.0.1:1/v1/logs", attributes: "missing-equals", warning: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", test.endpoint)
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", test.attributes)
			var stderr bytes.Buffer
			observations, _, close := configuredObservations(context.Background(), &stderr)
			defer close()
			if observations == nil {
				t.Fatal("instruction tracking was disabled with diagnostic export")
			}
			if got := strings.Contains(stderr.String(), "could not initialize"); got != test.warning {
				t.Fatalf("initialization warning=%t, want %t", got, test.warning)
			}
		})
	}
}

func TestSessionOperationRejectsForeignSandbox(t *testing.T) {
	session := core.Session{ID: "job-direct"}
	message := core.Message{ID: "message-direct", SessionID: session.ID, Input: "raw caller prompt\nwith exact spacing\n"}
	sandbox := core.Sandbox{ID: core.MainSandboxName(session.ID), SessionID: session.ID, Name: core.DefaultSandbox}
	execution := core.AgentMessageExecution{
		Session: session, Message: message, Sandbox: sandbox,
		AgentRun: core.AgentRun{ID: core.AgentRunID(message.ID), SessionID: session.ID, MessageID: message.ID, SandboxID: sandbox.ID},
	}
	resolved := composedAgentExecution{externals: terminal.Externals{
		Sandbox: ordinarySandbox{}, Agent: ordinaryHarness{Harness: codex.Agent{}},
	}}
	if _, err := resolved.ResolveAgentRunOperation(context.Background(), execution); err != nil {
		t.Fatalf("direct operation: %v", err)
	}

	for name, mutate := range map[string]func(*core.AgentMessageExecution){
		"run Sandbox":  func(value *core.AgentMessageExecution) { value.AgentRun.SandboxID = "foreign" },
		"Sandbox name": func(value *core.AgentMessageExecution) { value.Sandbox.Name = "foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := execution
			mutate(&changed)
			if _, err := resolved.ResolveAgentRunOperation(context.Background(), changed); err == nil {
				t.Fatal("changed direct Agent contract resolved a Harness operation")
			}
		})
	}
}

// Embedding only the ordinary interfaces deliberately hides review methods.
type ordinarySandbox struct{ provider.Sandbox }
type ordinaryHarness struct{ terminal.Harness }

func TestSandboxForIncusProfileRequiresAndUsesExactDeploymentAuthority(t *testing.T) {
	authority := deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	authorityHash, err := authority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	profile := core.SandboxProfile{
		Name: "local-codex", Provider: core.SandboxProviderIncus,
		Artifact: strings.Repeat("a", 64), IncusEndpointAuthorityHash: authorityHash,
		IncusProject: "dorf", IncusStoragePool: "default", IncusNetwork: "incusbr0",
		IncusDiskSize: "40GiB", IncusGatewayURL: "http://10.44.0.1:8317/v1",
	}

	resolved, err := sandboxForProfile(config.Config{Workspace: "/workspace/job", Incus: &authority}, profile)
	if err != nil {
		t.Fatal(err)
	}
	adapter, ok := resolved.(incus.Adapter)
	if !ok {
		t.Fatalf("Incus runtime = %T", resolved)
	}
	connection := adapter.Config.Connection
	if connection.Endpoint != authority.Endpoint || connection.Project != profile.IncusProject || connection.StoragePool != profile.IncusStoragePool ||
		adapter.Config.ProviderGatewayURL != profile.IncusGatewayURL {
		t.Fatalf("Incus runtime does not preserve endpoint/profile custody: %#v", adapter.Config)
	}

	if _, err := sandboxForProfile(config.Config{Workspace: "/workspace/job"}, profile); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing Incus authority error = %v", err)
	}
	profile.IncusEndpointAuthorityHash = strings.Repeat("b", 64)
	if _, err := sandboxForProfile(config.Config{Workspace: "/workspace/job", Incus: &authority}, profile); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched Incus authority error = %v", err)
	}
}
