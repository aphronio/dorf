package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/e2b"
)

func TestLiveE2BAuthenticatedEndpointRecoversCodexThread(t *testing.T) {
	if os.Getenv("DORF_E2B_CODEX_ENDPOINT_LIVE") != "1" {
		t.Skip("set DORF_E2B_CODEX_ENDPOINT_LIVE=1 to mutate the configured E2B account")
	}
	apiKey := os.Getenv("E2B_API_KEY")
	template := os.Getenv("DORF_E2B_TEMPLATE")
	if apiKey == "" || template == "" {
		t.Fatal("E2B_API_KEY and DORF_E2B_TEMPLATE are required")
	}

	owner := e2bEndpointOwnership(t)
	client := e2b.Client{APIKey: apiKey}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sandbox, err := client.Create(ctx, e2b.CreateRequest{Template: template, Timeout: 10 * time.Minute, Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	providerID := sandbox.ProviderID
	defer func() {
		if providerID == "" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		if err := client.DeleteOwned(cleanupCtx, providerID, owner); err != nil {
			t.Errorf("cleanup E2B Sandbox %s: %v", providerID, err)
		}
	}()

	envd, err := client.ConnectEnvd(ctx, providerID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := e2b.NewExecutor(envd, nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := client.ConnectEndpoint(ctx, providerID, 4500, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	endpointExec(t, ctx, executor, []byte("endpoint-proof-only\n"), `umask 077; install -d -m 700 /root/.config/dorf; cat > /root/.config/dorf/provider-route.key`)
	controlToken, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	endpointExec(t, ctx, executor, []byte(controlToken+"\n"), controlCapabilityScript())
	endpointExec(t, ctx, executor, nil, appServerScript(endpoint.ListenURL, tokenSHA256(controlToken), false))

	var first *protocol
	deadline := time.Now().Add(30 * time.Second)
	for {
		first, err = dialProtocol(ctx, endpoint.DialURL, controlToken, endpoint.Headers())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote Codex endpoint did not become ready: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	threadID, err := first.startThread(ctx, "/workspace/job", "gpt-5.6-sol", "danger-full-access")
	if err != nil {
		first.connection.CloseNow()
		t.Fatal(err)
	}
	turn, err := first.startTurn(ctx, threadID, "/workspace/job", "endpoint-proof-message", "persist this endpoint proof", "gpt-5.6-sol", "low", "danger-full-access")
	if err != nil {
		first.connection.CloseNow()
		t.Fatal(err)
	}
	first.connection.CloseNow()

	second, err := dialProtocol(ctx, endpoint.DialURL, controlToken, endpoint.Headers())
	if err != nil {
		t.Fatal(err)
	}
	defer second.connection.CloseNow()
	if err := second.resumeThread(ctx, threadID); err != nil {
		t.Fatal(err)
	}
	turns, err := second.readTurns(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, observed := range turns {
		if observed.ID == turn.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("reconnected controller did not observe exact turn %s: %#v", turn.ID, turns)
	}

	if err := client.DeleteOwned(ctx, providerID, owner); err != nil {
		t.Fatal(err)
	}
	providerID = ""
	t.Logf("proved remote endpoint %s with two scoped capabilities and recovered Codex thread %s turn %s", endpoint.DialURL, threadID, turn.ID)
}

func endpointExec(t *testing.T, ctx context.Context, executor *e2b.Executor, stdin []byte, script string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	result, err := executor.Exec(ctx, e2b.ExecRequest{
		Argv: []string{"/bin/bash", "-lc", script}, Stdin: stdin,
		ProcessTimeout: 30 * time.Second, Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		var exitErr *e2b.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("endpoint command exited %d: %s", result.ExitCode, strings.TrimSpace(stderr.String()))
		}
		t.Fatal(err)
	}
}

func e2bEndpointOwnership(t *testing.T) e2b.Ownership {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(raw)
	return e2b.Ownership{
		JobID:          "e2b-codex-endpoint-proof-" + nonce[:12],
		SandboxID:      "dorf-e2b-codex-endpoint-" + nonce[:12],
		OwnershipNonce: nonce,
	}
}
