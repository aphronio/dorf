package terminal

import (
	"context"
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

// messageInput restores disposable working files from the accepted immutable
// blobs. Only submission calls it; history and recovery need no file access.
func (e Externals) messageInput(ctx context.Context, owner provider.Ownership, messageID, text string, attachments []core.MessageAttachment) (core.HarnessInput, error) {
	input := core.HarnessInput{Text: text}
	if len(attachments) == 0 {
		return input, nil
	}
	if messageID == "" || messageID == "." || messageID == ".." || path.Base(messageID) != messageID || strings.ContainsAny(messageID, "\\\x00") {
		return core.HarnessInput{}, fmt.Errorf("message attachments require a valid Message identity")
	}
	if e.Blobs.Root == "" {
		return core.HarnessInput{}, fmt.Errorf("message attachment blob store is not configured")
	}
	var index strings.Builder
	index.WriteString("\n\nAttached files (original name and local path):\n")
	for ordinal, attachment := range attachments {
		contents, err := e.Blobs.ReadVerified(attachment.Digest, attachment.ByteSize)
		if err != nil {
			return core.HarnessInput{}, fmt.Errorf("read attachment %q: %w", attachment.Filename, err)
		}
		name := path.Join(e.Sandbox.Workspace(), ".dorf", "attachments", messageID, fmt.Sprintf("%02d", ordinal+1), attachmentBasename(attachment.Filename))
		if err := provider.WriteFileViaExec(ctx, owner, e.Sandbox.Workspace(), name, contents, false, e.Sandbox.Exec); err != nil {
			return core.HarnessInput{}, fmt.Errorf("materialize attachment %q: %w", attachment.Filename, err)
		}
		fmt.Fprintf(&index, "- %q: %q\n", attachment.Filename, name)
		if attachment.Kind == core.MessageAttachmentImage {
			input.Images = append(input.Images, core.HarnessImage{MediaType: attachment.MediaType, Bytes: contents})
		}
	}
	input.Text += index.String()
	return input, nil
}

func attachmentBasename(filename string) string {
	var name strings.Builder
	for _, r := range filename {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			r = '_'
		}
		if name.Len()+len(string(r)) > 255 {
			break
		}
		name.WriteRune(r)
	}
	if name.Len() == 0 || name.String() == "." || name.String() == ".." {
		return "attachment"
	}
	return name.String()
}
