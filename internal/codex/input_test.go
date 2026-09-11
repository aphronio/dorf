package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestNativeStartAndSteerSendOrderedInlineImages(t *testing.T) {
	input := core.HarnessInput{Text: "Keep this text verbatim.\n", Images: []core.HarnessImage{
		{MediaType: "image/png", Bytes: []byte{137, 'P', 'N', 'G', '\r', '\n', 0, 255}},
		{MediaType: "image/webp", Bytes: []byte("RIFF\x00\x00\x00\x00WEBP")},
	}}
	for _, route := range []string{"initial", "follow", "steer"} {
		t.Run(route, func(t *testing.T) {
			var submissions atomic.Int32
			server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
				switch method {
				case "initialize":
					return map[string]any{}, false
				case "thread/list":
					return map[string]any{"data": []any{}}, false
				case "thread/start", "thread/resume":
					return map[string]any{"thread": map[string]any{"id": "thread"}}, false
				case "turn/start", "turn/steer":
					submissions.Add(1)
					assertNativeImageInput(t, params, input)
					if method == "turn/steer" {
						if params["expectedTurnId"] != "active" {
							t.Error("steer did not bind the exact active turn")
						}
						return map[string]any{"turnId": "active"}, false
					}
					return map[string]any{"turn": map[string]any{"id": "active"}}, false
				default:
					return nil, true
				}
			})
			defer server.Close()
			protocol := dialTestProtocol(t, server)
			var err error
			switch route {
			case "initial":
				_, _, err = protocol.reconcileInitialTurn(context.Background(), "/workspace/job", "run", input, "model", "high", "danger-full-access")
			case "follow":
				_, err = protocol.resumeAndStartTurn(context.Background(), "thread", "/workspace/job", "run", input, "model", "high", "danger-full-access")
			case "steer":
				_, err = protocol.steerTurn(context.Background(), "thread", "active", "run", input)
			}
			if err != nil || submissions.Load() != 1 {
				t.Fatalf("native submissions=%d, error=%v", submissions.Load(), err)
			}
		})
	}
}

func assertNativeImageInput(t *testing.T, params map[string]any, want core.HarnessInput) {
	t.Helper()
	content, ok := params["input"].([]any)
	if !ok || len(content) != len(want.Images)+1 {
		t.Fatalf("native content=%#v", params["input"])
	}
	text, ok := content[0].(map[string]any)
	if !ok || text["type"] != "text" || text["text"] != want.Text {
		t.Fatalf("native text=%#v", content[0])
	}
	for index, image := range want.Images {
		part, ok := content[index+1].(map[string]any)
		if !ok || part["type"] != "image" {
			t.Fatalf("native image=%#v", content[index+1])
		}
		url, _ := part["url"].(string)
		prefix := "data:" + image.MediaType + ";base64,"
		if !strings.HasPrefix(url, prefix) {
			t.Fatalf("native image does not use inline %s bytes", image.MediaType)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, prefix))
		if err != nil || !bytes.Equal(decoded, image.Bytes) {
			t.Fatalf("native image changed bytes: %v", err)
		}
	}
}
