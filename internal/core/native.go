package core

import (
	"context"
	"errors"
	"path"
	"strings"
	"unicode/utf8"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

var (
	ErrNativeUnavailable = errors.New("native Session is not ready")
	ErrNativeUnknown     = errors.New("native event outcome is unknown; do not resubmit")
	ErrInvalidEvent      = errors.New("invalid Session event")
)

const (
	InputMessage    = "input.message"
	InputToolOutput = "input.tool_output"
	InputCancel     = "input.cancel"
)

// NativeEvent is transient input, not an admitted or replayable database record.
// ClientID attributes native input; it does not provide duplicate suppression.
type NativeEvent struct {
	Type                  string             `json:"type"`
	ClientID              string             `json:"client_id,omitempty"`
	Text                  string             `json:"text,omitempty"`
	DeveloperInstructions *string            `json:"developer_instructions,omitempty"`
	RefreshSkills         bool               `json:"refresh_skills,omitempty"`
	Attachments           []NativeAttachment `json:"attachments,omitempty"`
}

type NativeAttachment struct {
	Filename string `json:"filename"`
	Contents []byte `json:"contents"`
	Kind     string `json:"kind,omitempty"`
}

type NativeAcknowledgement struct {
	Harness  string `json:"harness"`
	Type     string `json:"type"`
	ClientID string `json:"client_id,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	TurnID   string `json:"turn_id,omitempty"`
}

// NativeState retains only the cutoff and unresolved mutation needed to protect
// maintenance. Neither an input payload nor a native execution result lives here.
type NativeState struct {
	Revision       int64
	PendingInputID string
	PendingTurnID  string
}

// NativeMutation records custody before native writes. Bind runs before input
// can be sent. Begin runs before configuration/input mutation or exact cancel.
type NativeMutation struct {
	Unused bool
	Bind   func(context.Context, string) error
	Begin  func(context.Context, string) (string, error)
}

type NativeSession interface {
	InputCapabilities(context.Context, Session, Sandbox) (InputCapabilities, error)
	SubmitEvent(context.Context, Session, Sandbox, NativeEvent, NativeMutation) (NativeAcknowledgement, error)
	ReadNativeTurns(context.Context, Session, Sandbox) (HarnessHistory, error)
	NativeIdle(context.Context, Session, Sandbox) (bool, error)
}

func (e NativeEvent) Validate() error {
	if e.Type == InputCancel {
		if e.hasInput() {
			return ErrInvalidEvent
		}
		return nil
	}
	if e.Type != InputMessage && e.Type != InputToolOutput {
		return ErrInvalidEvent
	}
	if !validInputIdentity(e.ClientID) || !validInputText(e) {
		return ErrInvalidEvent
	}
	if len(e.Attachments) > MaxAttachments || (e.Type == InputToolOutput && len(e.Attachments) != 0) {
		return ErrInvalidEvent
	}
	for _, a := range e.Attachments {
		if !ValidAttachmentFilename(a.Filename) || len(a.Contents) > provider.MaxFileWriteBytes || (a.Kind != "" && a.Kind != "audio") {
			return ErrInvalidEvent
		}
	}
	return nil
}

func validInputIdentity(id string) bool {
	return id != "" && len(id) <= 256 && path.Base(id) == id && id != "." && id != ".." && !strings.ContainsAny(id, "\\\x00") && utf8.ValidString(id)
}
func validInputText(e NativeEvent) bool {
	return utf8.ValidString(e.Text) && !strings.ContainsRune(e.Text, 0) && len(e.Text) <= MaxInputBytes && (strings.TrimSpace(e.Text) != "" || len(e.Attachments) > 0) && ValidDeveloperInstructions(e.DeveloperInstructions)
}

func (e NativeEvent) hasInput() bool {
	return e.ClientID != "" || e.Text != "" || e.DeveloperInstructions != nil || e.RefreshSkills || len(e.Attachments) != 0
}

func (s NativeState) Pending() bool { return s.PendingInputID != "" || s.PendingTurnID != "" }
