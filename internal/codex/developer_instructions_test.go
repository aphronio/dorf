package codex

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestDeveloperSnapshotOnlyInjectsBeforeFreshSubmission(t *testing.T) {
	value, empty := "Application generation A", ""
	for _, route := range []string{"initial", "follow"} {
		for _, snapshot := range []*string{nil, &value, &empty} {
			t.Run(route, func(t *testing.T) {
				var injected atomic.Int32
				server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
					if _, exists := params["baseInstructions"]; exists {
						t.Error("overrode native base instructions")
					}
					switch method {
					case "initialize":
						return map[string]any{}, false
					case "thread/list":
						return map[string]any{"data": []any{}}, false
					case "thread/start", "thread/resume":
						return map[string]any{"thread": map[string]any{"id": "thread"}}, false
					case "thread/inject_items":
						injected.Add(1)
						item := params["items"].([]any)[0].(map[string]any)
						text := item["content"].([]any)[0].(map[string]any)["text"].(string)
						if snapshot == nil || item["role"] != "developer" || !strings.Contains(text, "<application_developer_instructions>\n"+*snapshot+"\n</application_developer_instructions>") {
							t.Error("incorrect developer snapshot")
						}
						return map[string]any{}, false
					case "turn/start":
						want := int32(0)
						if snapshot != nil {
							want = 1
						}
						if injected.Load() != want {
							t.Error("snapshot not applied before submission")
						}
						return map[string]any{"turn": map[string]any{"id": "active"}}, false
					case "turn/steer":
						return map[string]any{"turnId": "active"}, false
					}
					return nil, true
				})
				defer server.Close()
				p := dialTestProtocol(t, server)
				input := core.HarnessInput{Text: "hello", DeveloperInstructions: snapshot}
				var err error
				switch route {
				case "initial":
					_, _, err = p.initialFixture(context.Background(), "/workspace", "run", input, "model", "high", "danger-full-access")
				case "follow":
					_, err = p.resumeFixture(context.Background(), "thread", "/workspace", "run", input, "model", "high", "danger-full-access")
				}
				if err != nil {
					t.Fatal(err)
				}
				if route == "steer" && injected.Load() != 0 {
					t.Fatal("steer altered developer instructions")
				}
			})
		}
	}
}
