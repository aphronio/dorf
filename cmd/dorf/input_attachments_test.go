package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
)

func TestValidateInputAttachmentsFullyDecodesSupportedImagesAndKeepsOtherFiles(t *testing.T) {
	webpBytes, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	raw := []core.NativeAttachment{
		{Filename: `C:\\incoming\\diagram.png`, Contents: encodeTestPNG(t, 2, 3)},
		{Filename: "photo.jpg", Contents: encodeTestJPEG(t)},
		{Filename: "pixel.webp", Contents: webpBytes},
		{Filename: "/tmp/animation.gif", Contents: encodeTestGIF(t)},
	}
	if err := validateInputAttachments(raw); err != nil {
		t.Fatal(err)
	}

}

func TestValidateInputAttachmentsRejectsTruncatedBombAndAnimatedImages(t *testing.T) {
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
			err := validateInputAttachments([]core.NativeAttachment{{Filename: "image", Contents: test.contents}})
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
		})
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
