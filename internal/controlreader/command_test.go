package controlreader

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type readerCommandExecutor struct {
	calls   int
	command provider.Command
	session core.Session
	sandbox core.Sandbox
	stdout  string
}

func (e *readerCommandExecutor) ExecSandbox(_ context.Context, session core.Session, owned core.Sandbox, command provider.Command) (provider.CommandResult, error) {
	e.calls++
	e.command, e.session, e.sandbox = command, session, owned
	return provider.CommandResult{ExitCode: 7, Stdout: e.stdout, Stderr: "warning\n"}, nil
}

func TestCommandsUseAuthenticatedSessionCustodyAndCleanupFence(t *testing.T) {
	session := core.Session{ID: "job-1", SandboxProfile: "profile-1", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox-1", SessionID: session.ID, OwnershipNonce: strings.Repeat("a", 64)}
	store := &readerTestStore{session: session, sandbox: owned}
	executor := &readerCommandExecutor{stdout: "installed\n"}
	handler, err := NewHandler(strings.Repeat("b", 64), Service{Store: store, Runtimes: readerTestRuntimes{profile: session.SandboxProfile, commands: executor}})
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: readerHandlerTransport{handler: handler}}
	client, err := NewClient("http://control-reader.test:8756", strings.Repeat("b", 64), httpClient)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := NewClient("http://control-reader.test:8756", strings.Repeat("c", 64), httpClient)
	if err != nil {
		t.Fatal(err)
	}
	command := provider.Command{Argv: []string{"printf", "%s", "literal $(not-a-command)"}, Stdin: "input\n", TimeoutSeconds: 90}
	if _, err := wrong.Exec(context.Background(), owned.ID, command); !errors.Is(err, ErrUnauthorized) || executor.calls != 0 {
		t.Fatalf("unauthorized command reached provider: %v", err)
	}
	result, err := client.Exec(context.Background(), owned.ID, command)
	if err != nil || result.ExitCode != 7 || result.Stdout != "installed\n" || result.Stderr != "warning\n" {
		t.Fatalf("command result=%+v err=%v", result, err)
	}
	if executor.calls != 1 || !reflect.DeepEqual(executor.command, command) || executor.session != session || executor.sandbox != owned || store.fences != 1 {
		t.Fatal("command lost exact argv, stdin, or custody")
	}
	for _, invalid := range []provider.Command{{}, {Argv: []string{"sleep", "1"}, TimeoutSeconds: 121}, {Argv: []string{"bad\x00argument"}}, {Argv: []string{"cat"}, Stdin: strings.Repeat("a", provider.MaxCommandBytes-2)}} {
		if _, err := client.Exec(context.Background(), owned.ID, invalid); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("accepted invalid command: %v", err)
		}
	}
	executor.stdout = strings.Repeat("\x00", provider.MaxCommandOutputBytes)
	command = provider.Command{Argv: []string{"cat"}, Stdin: strings.Repeat("\x00", provider.MaxCommandBytes-3)}
	result, err = client.Exec(context.Background(), owned.ID, command)
	if err != nil || result.Stdout != executor.stdout || executor.command.Stdin != command.Stdin {
		t.Fatalf("JSON-escaped output did not survive transport: %v", err)
	}
	store.deliveryHeld = true
	if _, err := client.Exec(context.Background(), owned.ID, command); !errors.Is(err, ErrUnavailable) || executor.calls != 2 {
		t.Fatalf("upgrade-held command reached provider: %v", err)
	}
	store.deliveryHeld = false
	store.session.CleanupState = core.CleanupRequested
	if _, err := client.Exec(context.Background(), owned.ID, command); !errors.Is(err, ErrUnavailable) || executor.calls != 2 {
		t.Fatalf("cleanup-fenced command reached provider: %v", err)
	}
}
