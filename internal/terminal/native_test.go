package terminal

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type audioHarness struct {
	Harness
	NativeHarness
	capabilities core.InputCapabilities
	input        core.HarnessInput
	submissions  int
}

func (h *audioHarness) InputCapabilities(context.Context, provider.Ownership, string) (core.InputCapabilities, error) {
	return h.capabilities, nil
}
func (h *audioHarness) SubmitNative(_ context.Context, _ provider.Ownership, _ core.Session, _ core.NativeEvent, input core.HarnessInput, _ core.NativeMutation) (core.NativeAcknowledgement, error) {
	h.input = input
	h.submissions++
	return core.NativeAcknowledgement{}, nil
}

type audioSandbox struct {
	provider.Sandbox
	writes [][]byte
}

func (s *audioSandbox) Workspace() string { return "/workspace" }
func (s *audioSandbox) Exec(_ context.Context, _ provider.Ownership, input []byte, _ ...string) (provider.Result, error) {
	s.writes = append(s.writes, input)
	return provider.Result{}, nil
}
func TestNativeAudioValidatesBeforeWritingOrSubmitting(t *testing.T) {
	recording := []byte("OggS\x00synthetic recording")
	for _, scenario := range []string{"supported", "unsupported", "wrong-model", "oversize", "bad-format", "ordinary-file"} {
		t.Run(scenario, func(t *testing.T) {
			harness := &audioHarness{capabilities: core.InputCapabilities{Model: "selected", AudioMediaTypes: []string{"audio/ogg"}, MaxAudioBytes: 100}}
			sandbox := &audioSandbox{}
			event := core.NativeEvent{Type: core.InputMessage, ClientID: "input", Attachments: []core.NativeAttachment{{Kind: "audio", Filename: "recording.ogg", Contents: recording}}}
			switch scenario {
			case "unsupported":
				harness.capabilities.AudioMediaTypes = nil
			case "wrong-model":
				harness.capabilities.Model = "other"
			case "oversize":
				harness.capabilities.MaxAudioBytes = len(recording) - 1
			case "bad-format":
				event.Attachments[0].Contents = []byte("not audio")
			case "ordinary-file":
				event.Attachments[0].Kind = ""
			}
			external := Externals{Agent: harness, Sandbox: sandbox, Ownership: func(context.Context, string) (provider.Ownership, error) { return provider.Ownership{}, nil }}
			_, err := external.SubmitEvent(context.Background(), core.Session{ID: "session", Model: "selected"}, core.Sandbox{SessionID: "session"}, event, core.NativeMutation{})
			if scenario != "supported" && scenario != "ordinary-file" {
				if !errors.Is(err, core.ErrInvalidEvent) || harness.submissions != 0 || len(sandbox.writes) != 0 {
					t.Fatalf("err=%v submissions=%d writes=%d", err, harness.submissions, len(sandbox.writes))
				}
				return
			}
			if err != nil || harness.submissions != 1 || len(sandbox.writes) != 1 || !bytes.Equal(sandbox.writes[0], recording) {
				t.Fatalf("err=%v input=%+v writes=%v", err, harness.input, sandbox.writes)
			}
			if scenario == "ordinary-file" {
				if len(harness.input.Audio) != 0 {
					t.Fatal("ordinary attachment promoted to audio")
				}
			} else if len(harness.input.Audio) != 1 || harness.input.Audio[0].MediaType != "audio/ogg" || !bytes.Equal(harness.input.Audio[0].Bytes, recording) {
				t.Fatalf("audio=%+v", harness.input.Audio)
			}
		})
	}
}
func TestAudioMediaTypes(t *testing.T) {
	for _, example := range []struct {
		contents []byte
		want     string
	}{
		{[]byte("RIFF\x00\x00\x00\x00WAVE"), "audio/wav"},
		{[]byte("ID3\x00"), "audio/mpeg"},
		{[]byte{0xff, 0xfb, 0x90, 0x00}, "audio/mpeg"},
		{[]byte("\x00\x00\x00\x18ftypM4A \x00\x00\x00\x00M4A isom"), "audio/mp4"},
		{[]byte("\x1a\x45\xdf\xa3synthetic"), "audio/webm"},
		{[]byte("OggS\x00synthetic"), "audio/ogg"},
		{[]byte("unrecognized"), ""},
	} {
		if got := audioMediaType(example.contents); got != example.want {
			t.Errorf("%x: got %q want %q", example.contents, got, example.want)
		}
	}
}
