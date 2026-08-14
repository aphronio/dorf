package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/e2b"
	provider "github.com/aphronio/dorf/internal/sandbox"
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
	sandbox := e2b.Adapter{Client: client, Config: e2b.AdapterConfig{
		Template: template, Workspace: "/workspace/job", SandboxTimeout: 10 * time.Minute, ProcessTimeout: 30 * time.Second,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := sandbox.ReconcileOwnedCreate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	present := true
	defer func() {
		if !present {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		if err := sandbox.DeleteOwned(cleanupCtx, owner); err != nil {
			t.Errorf("cleanup E2B Sandbox: %v", err)
		}
	}()

	installed, err := sandbox.Exec(ctx, owner, []byte("endpoint-proof-only\n"), "bash", "-lc", `umask 077; install -d -m 700 /root/.config/dorf; cat > /root/.config/dorf/provider-route.key`)
	if err != nil || installed.ExitCode != 0 {
		t.Fatalf("install proof-only route marker: %#v, %v", installed, err)
	}
	agent := Agent{Sandbox: sandbox, Port: 4500, Timeout: 2 * time.Minute}
	var threadID string
	var turn TurnOutcome
	if err := agent.withServer(ctx, owner, func(first *protocol) error {
		var err error
		threadID, err = first.startThread(ctx, "/workspace/job", "gpt-5.6-sol", "danger-full-access")
		if err != nil {
			return err
		}
		turn, err = first.startTurn(ctx, threadID, "/workspace/job", "endpoint-proof-message", "persist this endpoint proof", "gpt-5.6-sol", "low", "danger-full-access")
		first.connection.CloseNow()
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var turns []TurnOutcome
	if err := agent.withServer(ctx, owner, func(second *protocol) error {
		if err := second.resumeThread(ctx, threadID); err != nil {
			return err
		}
		var err error
		turns, err = second.readTurns(ctx, threadID)
		return err
	}); err != nil {
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

	if err := sandbox.DeleteOwned(ctx, owner); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		owned, err := sandbox.OwnedPresent(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		if !owned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("common E2B Sandbox remained present after ownership-guarded deletion")
		}
		time.Sleep(250 * time.Millisecond)
	}
	present = false
	t.Logf("proved common E2B Sandbox contract and recovered Codex thread %s turn %s", threadID, turn.ID)
}

func e2bEndpointOwnership(t *testing.T) provider.Ownership {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(raw)
	return provider.Ownership{
		JobID:          "e2b-codex-endpoint-proof-" + nonce[:12],
		SandboxID:      "dorf-e2b-codex-endpoint-" + nonce[:12],
		OwnershipNonce: nonce,
	}
}
