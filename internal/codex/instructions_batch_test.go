package codex

import (
	"context"
	"errors"
	"strings"
	"testing"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type batchInstructionSandbox struct {
	*instructionSandbox
}

func (s batchInstructionSandbox) ReadFiles(_ context.Context, _ provider.Ownership, names []string, _ int) (map[string][]byte, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	files := make(map[string][]byte)
	for _, name := range names {
		if contents, exists := s.files[name]; exists {
			files[name] = []byte(contents)
		}
	}
	return files, nil
}

func TestInstructionBatchPreservesValidationAndMissingFileSemantics(t *testing.T) {
	for _, contents := range []string{"", "valid instructions", strings.Repeat("x", maxInstructionFileBytes), "invalid\x00", "invalid\xff", strings.Repeat("x", maxInstructionFileBytes+1)} {
		sandbox := &instructionSandbox{files: map[string]string{"SOUL.md": contents}}
		separate := Agent{Sandbox: sandbox}
		batched := Agent{Sandbox: batchInstructionSandbox{sandbox}}
		want, wantErr := separate.readWorkspaceInstructions(context.Background(), testOwner("batch"), "/workspace/job")
		got, err := batched.readWorkspaceInstructions(context.Background(), testOwner("batch"), "/workspace/job")
		if (err != nil) != (wantErr != nil) || err == nil && *got != *want {
			t.Fatal("batch changed instruction validation or missing-file semantics")
		}
	}
	failure := errors.New("provider unavailable")
	agent := Agent{Sandbox: batchInstructionSandbox{&instructionSandbox{readErr: failure}}}
	if _, err := agent.readWorkspaceInstructions(context.Background(), testOwner("batch"), "/workspace/job"); !errors.Is(err, failure) {
		t.Fatalf("lost provider error: %v", err)
	}
}

func TestInstructionBatchRefreshesChangedFilesOnRetainedThread(t *testing.T) {
	f := newInstructionFixture(t)
	f.agent.Sandbox = batchInstructionSandbox{f.sandbox}
	owner := testOwner("batch")
	f.submit(t, owner, &instructionSession{initial: true}, "exact")
	unchanged := &instructionSession{}
	f.submit(t, owner, unchanged, "exact")
	f.requireInjection(t, unchanged, false, false)
	f.sandbox.files["AGENTS.md"] = "Updated operating rules."
	f.sandbox.files["SOUL.md"] = "Updated persona."
	changed := &instructionSession{}
	f.submit(t, owner, changed, "exact")
	f.requireInjection(t, changed, true, true)
	delete(f.sandbox.files, "AGENTS.md")
	delete(f.sandbox.files, "SOUL.md")
	deleted := &instructionSession{}
	f.submit(t, owner, deleted, "exact")
	f.requireInjection(t, deleted, true, true)
}
