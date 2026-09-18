package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type NativeHarness interface {
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
		name := path.Join(e.Sandbox.Workspace(), ".dorf", "attachments", directory, fmt.Sprintf("%02d", i+1), attachmentBasename(attachment.Filename))
		if err := provider.WriteFileViaExec(ctx, owner, e.Sandbox.Workspace(), name, attachment.Contents, false, e.Sandbox.Exec); err != nil {
			return core.HarnessInput{}, err
		}
		fmt.Fprintf(&index, "- %q: %q\n", attachment.Filename, name)
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
