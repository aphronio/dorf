package core

import (
	"context"
	"encoding/hex"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

const (
	MaxClientReferenceLength           = 255
	MaxMessageInputBytes               = 1 << 20
	MaxMessageAttachments              = 4
	MaxMessageAttachmentFilenameLength = 255
	// MaxMessageImagePixels bounds decoder memory before Dorf fully decodes an
	// accepted raster image. Compressed byte limits alone do not bound that use.
	MaxMessageImagePixels = 25_000_000
)

func ValidClientReference(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= MaxClientReferenceLength && !strings.ContainsRune(value, 0)
}

// MessageInput is the complete accepted input. Attachment bytes have
// already entered Dorf's immutable blob store; Core retains only their exact
// ordered metadata.
type MessageInput struct {
	Observation           bool
	DeveloperInstructions *string
	Text                  string
	Attachments           []MessageAttachment
}

func ValidMessageAttachment(attachment MessageAttachment) bool {
	if !validMessageAttachmentDigest(attachment.Digest) {
		return false
	}
	if attachment.Kind != MessageAttachmentImage && attachment.Kind != MessageAttachmentFile {
		return false
	}
	if !validMessageAttachmentFilename(attachment.Filename) {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(attachment.MediaType)
	if err != nil || mediaType == "" || attachment.ByteSize < 0 || attachment.ByteSize > provider.MaxFileWriteBytes {
		return false
	}
	return attachment.Kind != MessageAttachmentImage || validMessageImageMediaType(mediaType)
}

func validMessageAttachmentDigest(value string) bool {
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == 32 && strings.ToLower(value) == value
}

func validMessageAttachmentFilename(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "/\\") &&
		utf8.RuneCountInString(value) <= MaxMessageAttachmentFilenameLength && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validMessageImageMediaType(mediaType string) bool {
	return mediaType == "image/png" || mediaType == "image/jpeg" || mediaType == "image/webp"
}

func ValidMessageAttachments(attachments []MessageAttachment) bool {
	if len(attachments) > MaxMessageAttachments {
		return false
	}
	for _, attachment := range attachments {
		if !ValidMessageAttachment(attachment) {
			return false
		}
	}
	return true
}

func ValidMessageInput(input MessageInput) bool {
	return (!input.Observation || len(input.Attachments) == 0) && ValidDeveloperInstructions(input.DeveloperInstructions) && utf8.ValidString(input.Text) && !strings.ContainsRune(input.Text, 0) && len(input.Text) <= MaxMessageInputBytes &&
		(strings.TrimSpace(input.Text) != "" || len(input.Attachments) != 0) && ValidMessageAttachments(input.Attachments)
}

// SessionAdmission is the complete Core input shared by workflow and direct-client
// admission. Workflow packages extend it with their own typed input; a direct
// client leaves both workflow identity fields empty.
type SessionAdmission struct {
	KeepRunning        bool
	CreatedByClientID  string
	ClientReference    string
	AdmissionKey       string
	Workflow           WorkflowName
	WorkflowRevision   string
	AgentsMD           string
	SandboxProfile     string
	ProviderConnection string
	Model              string
	ReasoningEffort    string
}

// MessageAdmission is one client input admitted to its exact Agent lane.
type MessageAdmission struct {
	Observation           bool
	DeveloperInstructions *string
	RefreshSkills         bool
	SessionID             string
	SandboxID             string
	FromKind              MessageFromKind
	FromID                string
	Input                 string
	Attachments           []MessageAttachment
	Intent                MessageDeliveryIntent
}

// MessageAdmissionResult is the immutable durable admission acknowledged by
// the typed execution-envelope transaction. SandboxID is repeated independently of Message
// because Sandbox ownership belongs to the atomically admitted AgentRun.
type MessageAdmissionResult struct {
	Message   Message
	SandboxID string
	Created   bool
}

// AgentMessageAdmission is the provider-neutral composition seam behind an
// Agent handle. The deployment selects a typed execution-envelope adapter;
// Follow and Steer semantics remain invariant beneath it.
type AgentMessageAdmission interface {
	AdmitAgentMessage(context.Context, MessageAdmission) (MessageAdmissionResult, error)
}

// ValidDeveloperInstructions validates an optional complete application instruction snapshot.
func ValidDeveloperInstructions(value *string) bool {
	return value == nil || (utf8.ValidString(*value) && !strings.ContainsRune(*value, 0) && len(*value) <= MaxMessageInputBytes)
}

func SameDeveloperInstructions(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

// ValidObservationDelivery keeps application observations text-only. Auto may
// join active work; explicit Steer still requires a native exact-turn precondition.
func ValidObservationDelivery(observation bool, intent MessageDeliveryIntent, attachments int) bool {
	return !observation || (intent == MessageFollow || intent == MessageAuto) && attachments == 0
}
