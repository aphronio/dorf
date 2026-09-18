package incus

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

// The caller creates and later removes one disposable, owned guest. This test
// never chooses an existing application VM or operates through ambient remotes.
func TestLiveCommandCancellation(t *testing.T) {
	name, project := os.Getenv("DORF_INCUS_COMMAND_VM"), os.Getenv("DORF_INCUS_COMMAND_PROJECT")
	if name == "" || project == "" {
		t.Skip("set DORF_INCUS_COMMAND_VM and DORF_INCUS_COMMAND_PROJECT for a disposable guest")
	}
	owner := provider.Ownership{SessionID: "command-proof", SandboxID: name, OwnershipNonce: strings.Repeat("1", 64)}
	connection := DefaultConnectionConfig()
	connection.Project = project
	adapter := Adapter{Sandbox: Sandbox{Config: Config{Workspace: "/workspace/job", Connection: connection}}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	result, err := adapter.Run(ctx, owner, provider.RunRequest{Args: []string{"python3", "-c", "import os,sys; print(os.environ['DORF_PROBE']); sys.stdout.buffer.write(sys.stdin.buffer.read())"}, Stdin: []byte("literal stdin"), Env: map[string]string{"DORF_PROBE": "literal environment"}, Timeout: 5 * time.Second})
	if err != nil || !result.Stopped || !strings.Contains(result.Stdout, "literal stdin") || !strings.Contains(result.Stdout, "literal environment") {
		t.Fatalf("stdin/env execution failed: stopped=%t error=%v", result.Stopped, err)
	}
	result, err = adapter.Run(ctx, owner, provider.RunRequest{Args: []string{"sleep", "10"}, Timeout: time.Second})
	if !errors.Is(err, provider.ErrCommandTimeout) || !result.Stopped {
		t.Fatalf("timeout was not confirmed: stopped=%t error=%v", result.Stopped, err)
	}
	commandCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	started := time.Now()
	result, err = adapter.Run(commandCtx, owner, provider.RunRequest{Args: []string{"python3", "-c", `import os,signal,time,pathlib; signal.signal(signal.SIGTERM,signal.SIG_IGN); pathlib.Path("/tmp/dorf-command-proof.pid").write_text(str(os.getpid())); time.sleep(40)`}, Timeout: 45 * time.Second})
	if !errors.Is(err, context.DeadlineExceeded) || !result.Stopped || time.Since(started) > 9*time.Second {
		t.Fatalf("cancellation was not promptly confirmed: stopped=%t duration=%s error=%v", result.Stopped, time.Since(started), err)
	}
	pidResult, err := adapter.Exec(ctx, owner, nil, "cat", "/tmp/dorf-command-proof.pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidResult.Stdout))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := adapter.Exec(ctx, owner, nil, "python3", "-c", `import pathlib,sys; p=pathlib.Path('/proc/`+strconv.Itoa(pid)+`/cmdline'); sys.exit(1 if p.exists() and b'dorf-command-proof.pid' in p.read_bytes() else 0)`)
	if err != nil || probe.ExitCode != 0 {
		t.Fatalf("cancelled process remained alive: exit=%d error=%v", probe.ExitCode, err)
	}
	t.Logf("confirmed stdin/env, deadline and cancellation with remote process absence in %s", time.Since(started))
}
