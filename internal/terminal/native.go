package terminal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type NativeHarness interface {
	InputCapabilities(context.Context, provider.Ownership, string) (core.InputCapabilities, error)
	SubmitNative(context.Context, provider.Ownership, core.Session, core.NativeEvent, core.HarnessInput, core.NativeMutation) (core.NativeAcknowledgement, error)
	ReadTurns(context.Context, provider.Ownership, string) (core.HarnessHistory, error)
	NativeIdle(context.Context, provider.Ownership, string) (bool, error)
}

func (e Externals) SubmitEvent(ctx context.Context, session core.Session, owned core.Sandbox, event core.NativeEvent, mutation core.NativeMutation) (core.NativeAcknowledgement, error) {
	agent, ok := e.Agent.(NativeHarness)
	if !ok || owned.SessionID != session.ID {
		return core.NativeAcknowledgement{}, core.ErrNativeUnavailable
	}
	owner, err := e.owner(ctx, owned.ID)
	if err != nil {
		return core.NativeAcknowledgement{}, err
	}
	if err := e.validateNativeAudio(ctx, agent, owner, session.Model, event); err != nil {
		return core.NativeAcknowledgement{}, err
	}
	input, err := e.nativeInput(ctx, owner, event)
	if err != nil {
		return core.NativeAcknowledgement{}, err
	}
	return agent.SubmitNative(ctx, owner, session, event, input, mutation)
}

func (e Externals) ReadNativeTurns(ctx context.Context, session core.Session, owned core.Sandbox) (core.HarnessHistory, error) {
	agent, ok := e.Agent.(NativeHarness)
	if !ok || owned.SessionID != session.ID {
		return core.HarnessHistory{}, core.ErrNativeUnavailable
	}
	if session.ThreadID == "" {
		return core.HarnessHistory{Harness: session.Harness, Turns: []core.HarnessTurn{}}, nil
	}
	owner, err := e.owner(ctx, owned.ID)
	if err != nil {
		return core.HarnessHistory{}, err
	}
	return agent.ReadTurns(ctx, owner, session.ThreadID)
}

func (e Externals) nativeInput(ctx context.Context, owner provider.Ownership, event core.NativeEvent) (core.HarnessInput, error) {
	input := core.HarnessInput{Text: event.Text, Observation: event.Type == core.InputToolOutput, DeveloperInstructions: event.DeveloperInstructions}
	if len(event.Attachments) == 0 {
		return input, nil
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return core.HarnessInput{}, err
	}
	directory := hex.EncodeToString(nonce)
	var index strings.Builder
	index.WriteString("\n\nAttached files (original name and local path):\n")
	for i, attachment := range event.Attachments {
		name := path.Join(e.Sandbox.Workspace(), "attachments", directory, fmt.Sprintf("%02d", i+1), attachmentBasename(attachment.Filename))
		if err := provider.WriteFileViaExec(ctx, owner, e.Sandbox.Workspace(), name, attachment.Contents, false, e.Sandbox.Exec); err != nil {
			return core.HarnessInput{}, err
		}
		fmt.Fprintf(&index, "- %q: %q\n", attachment.Filename, name)
		if attachment.Kind == "audio" {
			input.Audio = append(input.Audio, core.HarnessAudio{MediaType: audioMediaType(attachment.Contents), Bytes: attachment.Contents})
			continue
		}
		media := http.DetectContentType(attachment.Contents)
		if media == "image/png" || media == "image/jpeg" || media == "image/webp" {
			input.Images = append(input.Images, core.HarnessImage{MediaType: media, Bytes: attachment.Contents})
		}
	}
	input.Text += index.String()
	return input, nil
}

func (e Externals) NativeIdle(ctx context.Context, session core.Session, owned core.Sandbox) (bool, error) {
	agent, ok := e.Agent.(NativeHarness)
	if !ok || owned.SessionID != session.ID {
		return false, core.ErrNativeUnavailable
	}
	if session.ThreadID == "" {
		return true, nil
	}
	owner, err := e.owner(ctx, owned.ID)
	if err != nil {
		return false, err
	}
	if observer, ok := e.Sandbox.(provider.StatusObserver); ok {
		status, err := observer.ObserveOwned(ctx, owner)
		if err != nil {
			return false, err
		}
		if status.State != "running" {
			return false, nil
		}
	}
	return agent.NativeIdle(ctx, owner, session.ThreadID)
}

func (e Externals) InputCapabilities(ctx context.Context, session core.Session, owned core.Sandbox) (core.InputCapabilities, error) {
	agent, ok := e.Agent.(NativeHarness)
	if !ok || owned.SessionID != session.ID {
		return core.InputCapabilities{Model: session.Model, AudioMediaTypes: []string{}}, nil
	}
	owner, err := e.owner(ctx, owned.ID)
	if err != nil {
		return core.InputCapabilities{}, err
	}
	return agent.InputCapabilities(ctx, owner, session.Model)
}

func (e Externals) validateNativeAudio(ctx context.Context, agent NativeHarness, owner provider.Ownership, model string, event core.NativeEvent) error {
	if !slices.ContainsFunc(event.Attachments, func(a core.NativeAttachment) bool { return a.Kind == "audio" }) {
		return nil
	}
	capabilities, err := agent.InputCapabilities(ctx, owner, model)
	if err != nil {
		return err
	}
	if capabilities.Model != model {
		return fmt.Errorf("%w: input capabilities do not match the selected model", core.ErrInvalidEvent)
	}
	for _, attachment := range event.Attachments {
		if attachment.Kind != "audio" {
			continue
		}
		if len(attachment.Contents) == 0 || len(attachment.Contents) > capabilities.MaxAudioBytes || !slices.Contains(capabilities.AudioMediaTypes, audioMediaType(attachment.Contents)) {
			return fmt.Errorf("%w: native audio is not supported for this model or recording", core.ErrInvalidEvent)
		}
	}
	return nil
}

func audioMediaType(contents []byte) string {
	// net/http only recognizes MP4 brands beginning with "mp4", and MP3 with ID3.
	if len(contents) >= 12 && bytes.Equal(contents[4:8], []byte("ftyp")) && bytes.Equal(contents[8:12], []byte("M4A ")) {
		return "audio/mp4"
	}
	if len(contents) >= 4 && contents[0] == 0xff && contents[1]&0xe6 == 0xe2 && contents[2]&0xf0 != 0xf0 && contents[2]&0x0c != 0x0c {
		return "audio/mpeg"
	}
	switch media := http.DetectContentType(contents); media {
	case "audio/wave", "audio/x-wav":
		return "audio/wav"
	case "audio/mpeg":
		return media
	case "application/ogg":
		return "audio/ogg"
	case "video/mp4":
		return "audio/mp4"
	case "video/webm":
		return "audio/webm"
	default:
		return ""
	}
}
