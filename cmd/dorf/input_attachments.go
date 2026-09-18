package main

import (
	"bytes"
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

func validateInputAttachments(raw []core.NativeAttachment) error {
	if len(raw) > core.MaxAttachments {
		return controlapi.ErrInvalidInput
	}
	for _, attachment := range raw {
		if len(attachment.Contents) > provider.MaxFileWriteBytes {
			return fmt.Errorf("%w: attachment exceeds the byte limit", controlapi.ErrInvalidInput)
		}
		if _, err := inputAttachmentFilename(attachment.Filename); err != nil {
			return err
		}
		mediaType := http.DetectContentType(attachment.Contents)
		switch mediaType {
		case "image/png", "image/jpeg", "image/webp":
			if err := validateInputImage(attachment.Contents, mediaType); err != nil {
				return err
			}
		}
	}
	return nil
}

func inputAttachmentFilename(filename string) (string, error) {
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	if !utf8.ValidString(filename) || strings.TrimSpace(filename) == "" || filename == "." ||
		utf8.RuneCountInString(filename) > core.MaxAttachmentFilenameLength || strings.IndexFunc(filename, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: attachment filename is invalid", controlapi.ErrInvalidInput)
	}
	return filename, nil
}

func validateInputImage(contents []byte, mediaType string) error {
	if mediaType == "image/webp" && animatedWebP(contents) {
		return controlapi.ErrAttachmentAnimationUnsupported
	}
	config, err := decodeInputImageConfig(contents, mediaType)
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return fmt.Errorf("%w: attachment is not a complete %s image", controlapi.ErrInvalidInput, mediaType)
	}
	if int64(config.Width) > int64(core.MaxImagePixels)/int64(config.Height) {
		return fmt.Errorf("%w: image dimensions are %dx%d", controlapi.ErrAttachmentImageTooLarge, config.Width, config.Height)
	}
	decoded, err := decodeInputImage(contents, mediaType)
	if err != nil {
		return fmt.Errorf("%w: attachment is not a complete %s image", controlapi.ErrInvalidInput, mediaType)
	}
	if decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return fmt.Errorf("%w: decoded image dimensions do not match its header", controlapi.ErrInvalidInput)
	}
	return nil
}

func decodeInputImageConfig(contents []byte, mediaType string) (image.Config, error) {
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

func decodeInputImage(contents []byte, mediaType string) (image.Image, error) {
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
