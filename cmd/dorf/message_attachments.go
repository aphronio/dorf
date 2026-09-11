package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"golang.org/x/image/webp"
)

type messageImageCapability interface {
	SupportsMessageImages(context.Context, string) (bool, error)
}

type classifiedMessageAttachment struct {
	filename  string
	contents  []byte
	kind      core.MessageAttachmentKind
	mediaType string
}

func (a controlAPIJobs) retainMessageAttachments(ctx context.Context, profile string, raw []controlapi.SendMessageAttachment) ([]core.MessageAttachment, error) {
	classified, hasImage, err := classifyMessageAttachments(raw)
	if err != nil {
		return nil, err
	}
	if len(classified) == 0 {
		return nil, nil
	}
	if hasImage {
		if a.messageImages == nil {
			return nil, fmt.Errorf("message image capability resolution is not configured")
		}
		supported, err := a.messageImages.SupportsMessageImages(ctx, profile)
		if err != nil {
			return nil, fmt.Errorf("resolve image attachment support for profile %q: %w", profile, err)
		}
		if !supported {
			return nil, controlapi.ErrMessageImageUnsupported
		}
	}
	if a.blobs.Root == "" {
		return nil, fmt.Errorf("message attachment blob store is not configured")
	}
	attachments := make([]core.MessageAttachment, 0, len(classified))
	for _, item := range classified {
		ref, err := a.blobs.Put(item.contents)
		if err != nil {
			return nil, fmt.Errorf("retain Message attachment %q: %w", item.filename, err)
		}
		if ref.ByteSize != int64(len(item.contents)) {
			return nil, fmt.Errorf("retained Message attachment %q returned a foreign byte size", item.filename)
		}
		if err := a.blobs.Verify(ref.Digest, ref.ByteSize); err != nil {
			return nil, fmt.Errorf("verify retained Message attachment %q: %w", item.filename, err)
		}
		attachment := core.MessageAttachment{
			Kind: item.kind, Filename: item.filename, MediaType: item.mediaType,
			Digest: ref.Digest, ByteSize: ref.ByteSize,
		}
		if !core.ValidMessageAttachment(attachment) {
			return nil, fmt.Errorf("%w: attachment metadata is invalid", controlapi.ErrInvalidInput)
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}

func classifyMessageAttachments(raw []controlapi.SendMessageAttachment) ([]classifiedMessageAttachment, bool, error) {
	if len(raw) > core.MaxMessageAttachments {
		return nil, false, controlapi.ErrInvalidInput
	}
	classified := make([]classifiedMessageAttachment, 0, len(raw))
	hasImage := false
	for _, attachment := range raw {
		if len(attachment.Contents) > provider.MaxFileWriteBytes {
			return nil, false, fmt.Errorf("%w: attachment exceeds the byte limit", controlapi.ErrInvalidInput)
		}
		filename, err := messageAttachmentFilename(attachment.Filename)
		if err != nil {
			return nil, false, err
		}
		mediaType := http.DetectContentType(attachment.Contents)
		kind := core.MessageAttachmentFile
		switch mediaType {
		case "image/png", "image/jpeg", "image/webp":
			if err := validateMessageImage(attachment.Contents, mediaType); err != nil {
				return nil, false, err
			}
			kind, hasImage = core.MessageAttachmentImage, true
		}
		classified = append(classified, classifiedMessageAttachment{
			filename: filename, contents: attachment.Contents, kind: kind, mediaType: mediaType,
		})
	}
	return classified, hasImage, nil
}

func messageAttachmentFilename(filename string) (string, error) {
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	if !utf8.ValidString(filename) || strings.TrimSpace(filename) == "" || filename == "." ||
		utf8.RuneCountInString(filename) > core.MaxMessageAttachmentFilenameLength || strings.IndexFunc(filename, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: attachment filename is invalid", controlapi.ErrInvalidInput)
	}
	return filename, nil
}

func validateMessageImage(contents []byte, mediaType string) error {
	if mediaType == "image/webp" && animatedWebP(contents) {
		return controlapi.ErrAttachmentAnimationUnsupported
	}
	config, err := decodeMessageImageConfig(contents, mediaType)
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return fmt.Errorf("%w: attachment is not a complete %s image", controlapi.ErrInvalidInput, mediaType)
	}
	if int64(config.Width) > int64(core.MaxMessageImagePixels)/int64(config.Height) {
		return fmt.Errorf("%w: image dimensions are %dx%d", controlapi.ErrAttachmentImageTooLarge, config.Width, config.Height)
	}
	decoded, err := decodeMessageImage(contents, mediaType)
	if err != nil {
		return fmt.Errorf("%w: attachment is not a complete %s image", controlapi.ErrInvalidInput, mediaType)
	}
	if decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return fmt.Errorf("%w: decoded image dimensions do not match its header", controlapi.ErrInvalidInput)
	}
	return nil
}

func decodeMessageImageConfig(contents []byte, mediaType string) (image.Config, error) {
	reader := bytes.NewReader(contents)
	switch mediaType {
	case "image/png":
		return png.DecodeConfig(reader)
	case "image/jpeg":
		return jpeg.DecodeConfig(reader)
	case "image/webp":
		return webp.DecodeConfig(reader)
	default:
		return image.Config{}, fmt.Errorf("unsupported image media type %q", mediaType)
	}
}

func decodeMessageImage(contents []byte, mediaType string) (image.Image, error) {
	reader := bytes.NewReader(contents)
	switch mediaType {
	case "image/png":
		return png.Decode(reader)
	case "image/jpeg":
		return jpeg.Decode(reader)
	case "image/webp":
		return webp.Decode(reader)
	default:
		return nil, fmt.Errorf("unsupported image media type %q", mediaType)
	}
}

func animatedWebP(contents []byte) bool {
	return len(contents) >= 21 && bytes.Equal(contents[:4], []byte("RIFF")) &&
		bytes.Equal(contents[8:16], []byte("WEBPVP8X")) && contents[20]&0x02 != 0
}
