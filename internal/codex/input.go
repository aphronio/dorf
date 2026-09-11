package codex

import (
	"encoding/base64"

	"github.com/aphronio/dorf/internal/core"
)

func nativeUserInput(input core.HarnessInput) []map[string]string {
	content := make([]map[string]string, 0, 1+len(input.Images))
	if input.Text != "" {
		content = append(content, map[string]string{"type": "text", "text": input.Text})
	}
	for _, image := range input.Images {
		content = append(content, map[string]string{
			"type": "image",
			"url":  "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Bytes),
		})
	}
	return content
}
