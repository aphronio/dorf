package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

// Exercise the real bounded file writer: recovery must read the same manifest
// that capture put in the workspace, without any database transcript.
func TestContinuityCaptureRoundTripAndRejectsChangedBinding(t *testing.T) {
	ctx := context.Background()
	server, _ := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
		if method == "initialize" {
			return map[string]any{}, false
		}
		if method != "thread/read" {
			t.Errorf("unexpected native operation %s", method)
		}
		return map[string]any{"thread": map[string]any{"id": "thread", "turns": []any{
			map[string]any{"id": "first", "status": "completed"},
			map[string]any{"id": "second", "status": "interrupted"},
		}}}, false
	})
	defer server.Close()
	sandbox := continuitySandbox{root: t.TempDir()}
	agent := Agent{Sandbox: sandbox}
	owner := testOwner("continuity")
	expected, err := agent.captureContinuity(ctx, owner, dialTestProtocol(t, server), "thread")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := agent.readContinuity(ctx, owner, "thread")
	if err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("manifest=%v error=%v", actual, err)
	}
	for _, invalid := range [][]retainedTurn{
		nil,
		{{ThreadID: "other", TurnID: "first", TurnOutcome: "completed"}},
		{{ThreadID: "thread", TurnID: "first", TurnOutcome: "inProgress"}},
		{expected[0], expected[0]},
	} {
		raw, _ := json.Marshal(invalid)
		if err := os.WriteFile(filepath.Join(sandbox.root, continuityFile), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := agent.readContinuity(ctx, owner, "thread"); err == nil {
			t.Fatalf("accepted invalid manifest %v", invalid)
		}
	}
}

type continuitySandbox struct {
	provider.Sandbox
	root string
}

func (s continuitySandbox) Workspace() string { return s.root }
func (s continuitySandbox) ReadFile(_ context.Context, _ provider.Ownership, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.root, name))
}
func (s continuitySandbox) Exec(ctx context.Context, _ provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		return provider.Result{ExitCode: 1, Stderr: string(output)}, err
	}
	return provider.Result{Stdout: string(output)}, nil
}
