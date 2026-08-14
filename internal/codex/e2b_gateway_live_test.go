package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/spine"
	terminalapp "github.com/aphronio/dorf/internal/terminal"
)

func TestLiveE2BScopedGatewayCompletesCodexTurn(t *testing.T) {
	if os.Getenv("DORF_E2B_GATEWAY_LIVE") != "1" {
		t.Skip("set DORF_E2B_GATEWAY_LIVE=1 to mutate the configured E2B account and Provider Gateway")
	}
	apiKey := os.Getenv("E2B_API_KEY")
	template := os.Getenv("DORF_E2B_TEMPLATE")
	publicGatewayURL := os.Getenv("DORF_E2B_PROVIDER_GATEWAY_URL")
	if apiKey == "" || template == "" || publicGatewayURL == "" {
		t.Fatal("E2B_API_KEY, DORF_E2B_TEMPLATE, and DORF_E2B_PROVIDER_GATEWAY_URL are required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	statePath := os.Getenv("DORF_PROVIDER_GATEWAY_STATE")
	if statePath == "" {
		statePath = filepath.Join(home, ".local", "share", "dorf", "provider-gateway")
	}
	connectionName := os.Getenv("DORF_PROVIDER_CONNECTION")
	if connectionName == "" {
		connectionName = "personal-chatgpt"
	}

	owner := e2bEndpointOwnership(t)
	client := e2b.Client{APIKey: apiKey}
	sandbox := e2b.Adapter{Client: client, Config: e2b.AdapterConfig{
		Template: template, Workspace: "/workspace/job", SandboxTimeout: 10 * time.Minute,
		ProcessTimeout: 2 * time.Minute, ProviderGatewayURL: publicGatewayURL,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := sandbox.ReconcileOwnedCreate(ctx, owner); err != nil {
		t.Fatal(err)
	}

	providerGateway := gateway.Gateway{StatePath: statePath}
	agent := Agent{Sandbox: sandbox, Port: 4500, Timeout: 3 * time.Minute}
	consumer := "sandbox:" + owner.SandboxID
	routeID := "e2b-live-" + owner.OwnershipNonce[:16]
	routeCreated := false
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cleanupCancel()
		if routeCreated {
			if err := providerGateway.RevokeExact(cleanupCtx, consumer, routeID); err != nil {
				t.Errorf("revoke exact Provider Route: %v", err)
			}
			if err := agent.RemoveRoute(cleanupCtx, owner); err != nil {
				t.Errorf("remove scoped route from E2B Sandbox: %v", err)
			}
		}
		if err := sandbox.DeleteOwned(cleanupCtx, owner); err != nil {
			t.Errorf("delete owned E2B Sandbox: %v", err)
		}
	})

	routeURL, err := sandbox.ProviderRouteURL(ctx, "http://private.invalid/v1")
	if err != nil {
		t.Fatal(err)
	}
	status, err := sandbox.Exec(ctx, owner, nil, "bash", "-c", `curl --silent --show-error --output /dev/null --write-out '%{http_code}' "$1/models"`, "dorf-unauthenticated-gateway-probe", routeURL)
	if err != nil || status.ExitCode != 0 || strings.TrimSpace(status.Stdout) != "401" {
		t.Fatalf("unauthenticated E2B Gateway probe = %#v, %v", status, err)
	}

	externals := terminalapp.Externals{Sandbox: sandbox, Gateway: providerGateway, Agent: agent}
	job := spine.Job{ID: owner.JobID, ProviderConnection: connectionName, Model: "gpt-5.6-sol"}
	durableSandbox := spine.Sandbox{ID: owner.SandboxID, JobID: owner.JobID, OwnershipNonce: owner.OwnershipNonce}
	if err := externals.RouteCreate(ctx, job, durableSandbox, spine.Route{ID: routeID, SandboxID: owner.SandboxID}); err != nil {
		t.Fatal(err)
	}
	routeCreated = true

	binding, err := agent.StartInitialTurn(ctx, owner, sandbox.Workspace(), "e2b-live-agent-run", "Reply with exactly: dorf-e2b-gateway-proof", "gpt-5.6-sol", "low")
	if err != nil {
		t.Fatal(err)
	}
	for !terminal(binding.Turn.Status) {
		binding, err = agent.WaitTurn(ctx, owner, binding.ThreadID, binding.Turn.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if binding.Turn.Status != "completed" || !strings.Contains(binding.Turn.Output, "dorf-e2b-gateway-proof") {
		t.Fatalf("Codex turn did not complete through scoped E2B Gateway route: %#v", binding.Turn)
	}
	t.Logf("proved E2B -> scoped Provider Gateway -> Codex turn %s; credentials and endpoint redacted", binding.Turn.ID)
}
