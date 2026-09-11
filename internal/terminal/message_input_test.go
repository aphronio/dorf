package terminal

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestMessageAttachmentsReachInitialFollowAndSteerAndRestoreWorkingFiles(t *testing.T) {
	for _, route := range []string{"initial", "follow", "steer"} {
		t.Run(route, func(t *testing.T) {
			files := &attachmentSandbox{workspace: t.TempDir()}
			agent := &attachmentHarness{}
			store := blob.Store{Root: t.TempDir()}
			image, document := []byte("image snapshot"), []byte{'a', 0, '\n', 255}
			attachments := []core.MessageAttachment{
				storeAttachment(t, store, "screen пример.png", "image/png", core.MessageAttachmentImage, image),
				storeAttachment(t, store, "../report\n.csv", "text/csv", core.MessageAttachmentFile, document),
			}
			owner := provider.Ownership{JobID: "job", SandboxID: "sandbox", OwnershipNonce: "owned"}
			externals := Externals{Sandbox: files, Agent: agent, Blobs: store, Ownership: func(context.Context, string) (provider.Ownership, error) { return owner, nil }}
			job := core.Job{ID: owner.JobID}
			run := core.AgentRun{ID: "run", MessageID: "message", JobID: job.ID, SandboxID: owner.SandboxID}
			message := core.Message{ID: "message", Input: "Read both.\n", Attachments: attachments, TargetTurnID: "active"}
			if route != "initial" {
				run.ThreadID = "thread"
			}
			operation, err := NewAgentRunOperation(externals, core.AgentMessageExecution{Job: job, AgentRun: run, Message: message, Sandbox: core.Sandbox{ID: owner.SandboxID, JobID: job.ID}})
			if err != nil {
				t.Fatal(err)
			}
			submit := func() error {
				if route == "steer" {
					_, err := externals.AgentSteer(context.Background(), job, core.Delivery{AgentRun: run, Message: message})
					return err
				}
				_, err := operation.Submit(context.Background(), run, message.Input)
				return err
			}
			if err := submit(); err != nil {
				t.Fatal(err)
			}
			if agent.route != route || agent.owner != owner || len(agent.input.Images) != 1 || !bytes.Equal(agent.input.Images[0].Bytes, image) {
				t.Fatalf("native input was not bound to the exact owner and image: %+v", agent)
			}
			if !strings.HasPrefix(agent.input.Text, message.Input+"\n\n") || !strings.Contains(agent.input.Text, fmt.Sprintf("%q", attachments[1].Filename)) {
				t.Fatalf("user text or original filename was lost: %q", agent.input.Text)
			}
			imagePath := filepath.Join(files.workspace, ".dorf", "attachments", message.ID, "01", "screen пример.png")
			filePath := filepath.Join(files.workspace, ".dorf", "attachments", message.ID, "02", ".._report_.csv")
			for name, want := range map[string][]byte{imagePath: image, filePath: document} {
				got, err := os.ReadFile(name)
				if err != nil || !bytes.Equal(got, want) || !strings.Contains(agent.input.Text, fmt.Sprintf("%q", name)) {
					t.Fatalf("forwardable file %q = %v, err=%v, prompt=%q", name, got, err, agent.input.Text)
				}
			}
			firstInput := agent.input.Text
			if err := os.Remove(imagePath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filePath, []byte("agent edits"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := submit(); err != nil || agent.input.Text != firstInput {
				t.Fatalf("retry did not restore the same input: %v", err)
			}
			for name, want := range map[string][]byte{imagePath: image, filePath: document} {
				got, err := os.ReadFile(name)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("retry changed accepted bytes for %q: %v", name, err)
				}
			}
		})
	}
}

func TestAttachmentFailurePreventsNativeSubmitButDoesNotPreventRecovery(t *testing.T) {
	files := &attachmentSandbox{workspace: t.TempDir()}
	agent := &attachmentHarness{}
	owner := provider.Ownership{JobID: "job", SandboxID: "sandbox"}
	externals := Externals{Sandbox: files, Agent: agent, Blobs: blob.Store{Root: t.TempDir()}, Ownership: func(context.Context, string) (provider.Ownership, error) { return owner, nil }}
	run := core.AgentRun{ID: "run", MessageID: "message", JobID: owner.JobID, SandboxID: owner.SandboxID}
	operation, err := NewAgentRunOperation(externals, core.AgentMessageExecution{Job: core.Job{ID: owner.JobID}, AgentRun: run,
		Sandbox: core.Sandbox{ID: owner.SandboxID, JobID: owner.JobID}, Message: core.Message{ID: "message", Attachments: []core.MessageAttachment{{Filename: "missing.png", Digest: strings.Repeat("0", 64), ByteSize: 10, Kind: core.MessageAttachmentImage}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operation.Submit(context.Background(), run, "inspect"); err == nil || !strings.Contains(err.Error(), "missing.png") {
		t.Fatalf("missing bytes did not fail clearly: %v", err)
	}
	if agent.route != "" || files.writes != 0 {
		t.Fatal("failed verification reached native submission or file materialization")
	}
	binding, err := operation.Recover(context.Background(), run)
	if err != nil || binding.Turn.ID != "accepted" || agent.route != "" || files.writes != 0 {
		t.Fatalf("recovery depends on attachment availability: %+v, %v", binding, err)
	}
}

func storeAttachment(t *testing.T, store blob.Store, filename, mediaType string, kind core.MessageAttachmentKind, contents []byte) core.MessageAttachment {
	t.Helper()
	ref, err := store.Put(contents)
	if err != nil {
		t.Fatal(err)
	}
	return core.MessageAttachment{Filename: filename, MediaType: mediaType, Kind: kind, Digest: ref.Digest, ByteSize: ref.ByteSize}
}

type attachmentSandbox struct {
	provider.Sandbox
	workspace string
	writes    int
}

func (s *attachmentSandbox) Workspace() string { return s.workspace }
func (s *attachmentSandbox) Exec(ctx context.Context, _ provider.Ownership, input []byte, argv ...string) (provider.Result, error) {
	s.writes++
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := provider.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if exit, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

type attachmentHarness struct {
	Harness
	input core.HarnessInput
	route string
	owner provider.Ownership
}

func (*attachmentHarness) Name() string { return "codex" }
func (h *attachmentHarness) StartInitialTurn(_ context.Context, owner provider.Ownership, _, _ string, input core.HarnessInput, _, _ string, _ bool) (core.HarnessBinding, error) {
	h.input, h.route, h.owner = input, "initial", owner
	return core.HarnessBinding{}, nil
}
func (h *attachmentHarness) StartTurn(_ context.Context, owner provider.Ownership, _, _, _ string, input core.HarnessInput, _, _ string, _ bool) (core.HarnessBinding, error) {
	h.input, h.route, h.owner = input, "follow", owner
	return core.HarnessBinding{}, nil
}
func (h *attachmentHarness) SteerTurn(_ context.Context, owner provider.Ownership, _, _, _ string, input core.HarnessInput) (string, error) {
	h.input, h.route, h.owner = input, "steer", owner
	return "active", nil
}
func (*attachmentHarness) ReadInitialTurns(context.Context, provider.Ownership, string) (core.HarnessHistory, error) {
	return core.HarnessHistory{Harness: "codex", ThreadID: "thread", Turns: []core.HarnessTurn{{ID: "accepted", Status: "running"}}}, nil
}
