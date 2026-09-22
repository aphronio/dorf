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

func TestNativeStartAndSteerSendOrderedInlineMedia(t *testing.T) {
	input := core.HarnessInput{Text: "Keep this text verbatim.\n", Images: []core.HarnessImage{
		{MediaType: "image/png", Bytes: []byte{137, 'P', 'N', 'G', '\r', '\n', 0, 255}},
		{MediaType: "image/webp", Bytes: []byte("RIFF\x00\x00\x00\x00WEBP")},
	}, Audio: []core.HarnessAudio{{MediaType: "audio/ogg", Bytes: []byte("OggS synthetic recording")}}}
	for _, route := range []string{"initial", "follow"} {
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
				_, _, err = protocol.initialFixture(context.Background(), "/workspace", "run", input, "model", "high", "danger-full-access")
			case "follow":
				_, err = protocol.resumeFixture(context.Background(), "thread", "/workspace", "run", input, "model", "high", "danger-full-access")
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
	if !ok || len(content) != len(want.Images)+len(want.Audio)+1 {
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
	for index, audio := range want.Audio {
		part, ok := content[1+len(want.Images)+index].(map[string]any)
		if !ok || part["type"] != "audio" {
			t.Fatalf("native audio=%#v", part)
		}
		wantURL := "data:" + audio.MediaType + ";base64," + base64.StdEncoding.EncodeToString(audio.Bytes)
		if part["url"] != wantURL {
			t.Fatal("native audio changed format or bytes")
		}
	}
}

func TestInputCapabilitiesUsesExactNativeModelAcrossPages(t *testing.T) {
	for _, test := range []struct {
		name       string
		modalities []any
		known      bool
		supported  bool
	}{
		{"audio", []any{"text", "image", "audio"}, true, true},
		{"text-image", []any{"text", "image"}, true, false},
		{"missing-metadata", nil, true, false},
		{"unknown", []any{"audio"}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
				if method == "initialize" {
					return map[string]any{}, false
				}
				if method != "model/list" {
					return nil, true
				}
				if params["includeHidden"] != true {
					t.Error("hidden models excluded")
				}
				if params["cursor"] == nil {
					return map[string]any{"data": []any{map[string]any{"model": "other-model", "inputModalities": []any{"audio"}}}, "nextCursor": "next"}, false
				}
				model := "selected-model"
				if !test.known {
					model = "different-model"
				}
				return map[string]any{"data": []any{map[string]any{"model": model, "inputModalities": test.modalities}}}, false
			})
			defer server.Close()
			p := dialTestProtocol(t, server)
			capabilities, err := p.inputCapabilities(context.Background(), "selected-model")
			if err != nil || capabilities.Model != "selected-model" || (len(capabilities.AudioMediaTypes) > 0) != test.supported {
				t.Fatalf("capabilities=%+v err=%v", capabilities, err)
			}
		})
	}
}
