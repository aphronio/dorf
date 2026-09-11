package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"testing"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
)

type fixedMessageImageCapability struct {
	supported bool
	calls     int
}

func (c *fixedMessageImageCapability) SupportsMessageImages(context.Context, string) (bool, error) {
	c.calls++
	return c.supported, nil
}

func TestClassifyMessageAttachmentsFullyDecodesSupportedImagesAndKeepsOtherFiles(t *testing.T) {
	webpBytes, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	raw := []controlapi.SendMessageAttachment{
		{Filename: `C:\\incoming\\diagram.png`, Contents: encodeTestPNG(t, 2, 3)},
		{Filename: "photo.jpg", Contents: encodeTestJPEG(t)},
		{Filename: "pixel.webp", Contents: webpBytes},
		{Filename: "/tmp/animation.gif", Contents: encodeTestGIF(t)},
	}
	classified, hasImage, err := classifyMessageAttachments(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !hasImage || len(classified) != 4 {
		t.Fatalf("classified=%#v hasImage=%t", classified, hasImage)
	}
	wantNames := []string{"diagram.png", "photo.jpg", "pixel.webp", "animation.gif"}
	wantKinds := []core.MessageAttachmentKind{core.MessageAttachmentImage, core.MessageAttachmentImage, core.MessageAttachmentImage, core.MessageAttachmentFile}
	wantMediaTypes := []string{"image/png", "image/jpeg", "image/webp", "image/gif"}
	for index := range classified {
		if classified[index].filename != wantNames[index] || classified[index].kind != wantKinds[index] || classified[index].mediaType != wantMediaTypes[index] ||
			!bytes.Equal(classified[index].contents, raw[index].Contents) {
			t.Fatalf("attachment %d=%#v", index, classified[index])
		}
	}
}

func TestClassifyMessageAttachmentsRejectsTruncatedBombAndAnimatedImages(t *testing.T) {
	valid := encodeTestPNG(t, 1, 1)
	oversized := append([]byte(nil), valid...)
	width, height := uint32(5_001), uint32(5_000)
	putBigEndianUint32(oversized[16:20], width)
	putBigEndianUint32(oversized[20:24], height)
	putBigEndianUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	animated := []byte("RIFF\x12\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x02")

	tests := []struct {
		name     string
		contents []byte
		want     error
	}{
		{name: "truncated PNG", contents: valid[:20], want: controlapi.ErrInvalidInput},
		{name: "decoded pixel bomb", contents: oversized, want: controlapi.ErrAttachmentImageTooLarge},
		{name: "animated WebP", contents: animated, want: controlapi.ErrAttachmentAnimationUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := classifyMessageAttachments([]controlapi.SendMessageAttachment{{Filename: "image", Contents: test.contents}})
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestRetainMessageAttachmentsChecksProfileBeforePublishingVerifiedBlobs(t *testing.T) {
	contents := encodeTestPNG(t, 2, 2)
	unsupported := &fixedMessageImageCapability{}
	root := t.TempDir()
	jobs := controlAPIJobs{blobs: blob.Store{Root: root}, messageImages: unsupported}
	if _, err := jobs.retainMessageAttachments(context.Background(), "pi", []controlapi.SendMessageAttachment{{Filename: "image.png", Contents: contents}}); !errors.Is(err, controlapi.ErrMessageImageUnsupported) {
		t.Fatalf("unsupported image error=%v", err)
	}
	if unsupported.calls != 1 {
		t.Fatalf("capability calls=%d", unsupported.calls)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unsupported image published blob entries=%v err=%v", entries, err)
	}

	supported := &fixedMessageImageCapability{supported: true}
	jobs.messageImages = supported
	attachments, err := jobs.retainMessageAttachments(context.Background(), "codex", []controlapi.SendMessageAttachment{
		{Filename: "image.png", Contents: contents},
		{Filename: "notes.txt", Contents: []byte("notes")},
	})
	if err != nil || len(attachments) != 2 || attachments[0].Kind != core.MessageAttachmentImage || attachments[1].Kind != core.MessageAttachmentFile {
		t.Fatalf("attachments=%#v err=%v", attachments, err)
	}
	if supported.calls != 1 {
		t.Fatalf("supported capability calls=%d", supported.calls)
	}
	for index, attachment := range attachments {
		if err := jobs.blobs.Verify(attachment.Digest, attachment.ByteSize); err != nil {
			t.Fatalf("attachment %d did not retain verified bytes: %v", index, err)
		}
	}
}

func encodeTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func encodeTestJPEG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	value := image.NewRGBA(image.Rect(0, 0, 2, 1))
	value.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := jpeg.Encode(&encoded, value, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func encodeTestGIF(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	value := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Black})
	if err := gif.Encode(&encoded, value, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func putBigEndianUint32(destination []byte, value uint32) {
	destination[0] = byte(value >> 24)
	destination[1] = byte(value >> 16)
	destination[2] = byte(value >> 8)
	destination[3] = byte(value)
}
