package terminal

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type cancellableSandbox struct {
	provider.Sandbox
	request provider.RunRequest
	owner   provider.Ownership
	result  provider.RunResult
	err     error
}

func (s *cancellableSandbox) Run(_ context.Context, owner provider.Ownership, request provider.RunRequest) (provider.RunResult, error) {
	s.request, s.owner = request, owner
	return s.result, s.err
}

func TestSandboxCommandUsesCancellationRunnerAndOnlyConfirmsStoppedTimeout(t *testing.T) {
	session := core.Session{ID: "command-session"}
	owned := core.Sandbox{ID: "command-sandbox", SessionID: session.ID, OwnershipNonce: "owner"}
	command := provider.Command{Argv: []string{"python3", "-c", "import sys; print(sys.stdin.read())"}, Stdin: "literal input", TimeoutSeconds: 10}
	for _, test := range []struct {
		name     string
		stopped  bool
		err      error
		wantCode int
		wantErr  bool
	}{
		{name: "success", stopped: true},
		{name: "confirmed-timeout", stopped: true, err: provider.ErrCommandTimeout, wantCode: 124},
		{name: "unconfirmed-timeout", err: provider.ErrCommandTimeout, wantErr: true},
		{name: "cancelled-observation", stopped: true, err: context.Canceled, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sandbox := &cancellableSandbox{result: provider.RunResult{Result: provider.Result{Stdout: "out", Stderr: "stage", Truncated: true}, Stopped: test.stopped}, err: test.err}
			result, err := (Externals{Sandbox: sandbox}).ExecSandbox(t.Context(), session, owned, command)
			if (err != nil) != test.wantErr || result.ExitCode != test.wantCode {
				t.Fatalf("exit=%d error=%v", result.ExitCode, err)
			}
			if test.wantErr && !errors.Is(err, test.err) {
				t.Fatalf("lost failure identity: %v", err)
			}
			if !reflect.DeepEqual(sandbox.request.Args, command.Argv) || string(sandbox.request.Stdin) != command.Stdin || sandbox.request.Timeout != 10*time.Second || sandbox.request.MaxOutputBytes != provider.MaxCommandOutputBytes || sandbox.owner != ownershipMetadata(owned) {
				t.Fatal("command input, process identity, deadline, or custody changed")
			}
			if !test.wantErr && (result.Stdout != "out" || result.Stderr != "stage" || !result.Truncated) {
				t.Fatal("lost process output")
			}
		})
	}
}
