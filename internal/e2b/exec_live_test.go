package e2b

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestLiveEnvdExecPreservesProcessSemantics(t *testing.T) {
	if os.Getenv("DORF_E2B_EXEC_LIVE") != "1" {
		t.Skip("set DORF_E2B_EXEC_LIVE=1 to mutate the configured E2B account")
	}
	apiKey := os.Getenv("E2B_API_KEY")
	template := os.Getenv("DORF_E2B_TEMPLATE")
	if apiKey == "" || template == "" {
		t.Fatal("E2B_API_KEY and DORF_E2B_TEMPLATE are required")
	}
	owner := liveOwnership(t, "exec")
	client := Client{APIKey: apiKey}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sandbox, err := client.Create(ctx, CreateRequest{Template: template, Timeout: 10 * time.Minute, Owner: owner})
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

	connection, err := client.ConnectEnvd(ctx, providerID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExecutor(connection, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := Adapter{Client: client, Config: AdapterConfig{Workspace: "/workspace/job", SandboxTimeout: 10 * time.Minute, ProcessTimeout: 30 * time.Second}}
	owned := provider.Ownership{JobID: owner.JobID, SandboxID: owner.SandboxID, OwnershipNonce: owner.OwnershipNonce}
	fileContents := []byte{'b', 'u', 'n', 'd', 'l', 'e', 0, 0xff}
	if err := adapter.PutFile(ctx, owned, "/tmp/dorf/live-put-file.bin", fileContents); err != nil {
		t.Fatal(err)
	}
	if err := adapter.PutFile(ctx, owned, "/tmp/dorf/live-put-file.bin", fileContents); err != nil {
		t.Fatal(err)
	}
	observedFile, err := adapter.Exec(ctx, owned, nil, "cat", "/tmp/dorf/live-put-file.bin")
	if err != nil || observedFile.ExitCode != 0 || !bytes.Equal([]byte(observedFile.Stdout), fileContents) {
		t.Fatalf("PutFile bytes=%v exit=%d err=%v", []byte(observedFile.Stdout), observedFile.ExitCode, err)
	}

	input := []byte{'i', 0, 0xff, 'n'}
	var stdout, stderr bytes.Buffer
	result, err := executor.Exec(ctx, ExecRequest{
		Argv:  []string{"python3", "-c", "import sys; data=sys.stdin.buffer.read(); sys.stdout.buffer.write(data); sys.stderr.buffer.write(b'err\\x00'); sys.exit(17)"},
		Stdin: input, ProcessTimeout: 15 * time.Second, Stdout: &stdout, Stderr: &stderr,
	})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || result.ExitCode != 17 || !result.Exited {
		t.Fatalf("nonzero exec result=%#v error=%v", result, err)
	}
	if !bytes.Equal(stdout.Bytes(), input) || !bytes.Equal(stderr.Bytes(), []byte{'e', 'r', 'r', 0}) {
		t.Fatalf("raw stdout=%v stderr=%v", stdout.Bytes(), stderr.Bytes())
	}

	timeoutStarted := time.Now()
	timeoutResult, err := executor.Exec(ctx, ExecRequest{Argv: []string{"sleep", "30"}, ProcessTimeout: 500 * time.Millisecond})
	var timeoutErr *ProcessTimeoutError
	if !errors.As(err, &timeoutErr) || timeoutResult.PID == 0 || time.Since(timeoutStarted) > 10*time.Second {
		t.Fatalf("remote process timeout result=%#v elapsed=%s error=%v", timeoutResult, time.Since(timeoutStarted), err)
	}

	cancelCtx, cancelObservation := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancelObservation()
	_, err = executor.Exec(cancelCtx, ExecRequest{Argv: []string{"sleep", "30"}, ProcessTimeout: 10 * time.Second})
	var indeterminateErr *IndeterminateExecError
	if !errors.As(err, &indeterminateErr) || indeterminateErr.PID == 0 {
		t.Fatalf("canceled exec error = %v", err)
	}
	killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer killCancel()
	if err := executor.Kill(killCtx, indeterminateErr.PID); err != nil {
		t.Fatalf("kill indeterminate process %d: %v", indeterminateErr.PID, err)
	}

	if err := client.DeleteOwned(ctx, providerID, owner); err != nil {
		t.Fatal(err)
	}
	waitForOwned(t, ctx, client, owner, false)
	providerID = ""
	t.Logf("proved native envd exec for %s with envd %s", owner.SandboxID, connection.Version)
}
