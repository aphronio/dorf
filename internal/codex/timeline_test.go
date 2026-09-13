package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestTimelineReadsWholeNativeTurnWithoutObservingOrResuming(t *testing.T) {
	original := json.RawMessage(`{"id":"assistant","type":"agentMessage","phase":"commentary","text":"Working","nativeCounter":9007199254740993,"future":{"value":true}}`)
	items := make([]any, 150)
	for i := range items {
		kind := "userMessage"
		if i == 1 {
			kind = "reasoning"
		}
		items[i] = map[string]any{"id": fmt.Sprint(i), "type": kind, "content": []any{map[string]any{"type": "text", "text": fmt.Sprint(i)}}}
	}
	items = append(items, original)
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			server, requests := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
				if method == "initialize" {
					return map[string]any{}, false
				}
				if method != "thread/turns/list" {
					t.Errorf("passive read sent %s", method)
					return nil, true
				}
				if params["threadId"] != "owned" || params["sortDirection"] != "desc" || params["itemsView"] != "full" || params["limit"] != float64(1) {
					t.Errorf("turn params=%v", params)
				}
				turnID, status := "latest", "inProgress"
				var next any = "older"
				if params["cursor"] != nil {
					turnID, status, next = "old", "completed", nil
				}
				return map[string]any{"data": []any{map[string]any{"id": turnID, "status": status, "itemsView": "full", "items": items}}, "nextCursor": next}, false
			})
			defer server.Close()
			p := dialTestProtocol(t, server)
			turnID, selected, wantStatus := "", "latest", "inProgress"
			if explicit {
				turnID, selected, wantStatus = "old", "old", "completed"
			}
			timeline, err := p.readTimeline(context.Background(), "owned", turnID)
			if err != nil {
				t.Fatal(err)
			}
			if timeline.ThreadID != "owned" || timeline.TurnID != selected || timeline.Status != wantStatus || timeline.Harness != "codex" || len(timeline.Items) != 150 {
				t.Fatalf("timeline=%+v", timeline)
			}
			if string(timeline.Items[149]) != string(original) {
				t.Fatalf("original native object changed: %s", timeline.Items[149])
			}
			var first, last map[string]any
			_ = json.Unmarshal(timeline.Items[0], &first)
			_ = json.Unmarshal(timeline.Items[148], &last)
			if first["id"] != "0" || last["id"] != "149" {
				t.Fatalf("history order=%v/%v", first, last)
			}
			if p.observed != nil || p.observations != nil {
				t.Fatal("passive read registered observation")
			}
			methods := reviewProtocolMethods(requests)
			want := []string{"thread/turns/list"}
			if explicit {
				want = append(want, "thread/turns/list")
			}
			if !reflect.DeepEqual(methods, want) {
				t.Fatalf("methods=%v", methods)
			}
		})
	}
}

func TestTimelineRejectsUnavailableHistoryAndMalformedNativePages(t *testing.T) {
	for _, test := range []string{"unsupported", "no-turns", "unknown-turn", "missing-data", "bad-cursor", "empty-cursor", "repeated-cursor", "repeated-turn", "duplicate-item", "empty-items", "missing-items", "summary-items", "bad-status", "oversized"} {
		t.Run(test, func(t *testing.T) {
			pageNumber := 0
			server, _ := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
				if method == "initialize" {
					return map[string]any{}, false
				}
				if method != "thread/turns/list" {
					t.Errorf("unexpected method %s", method)
					return nil, true
				}
				if test == "unsupported" {
					return nil, true
				}
				if test == "missing-data" {
					return map[string]any{}, false
				}
				if test == "no-turns" {
					return map[string]any{"data": []any{}}, false
				}
				pageNumber++
				turn := map[string]any{"id": fmt.Sprint(pageNumber), "status": "inProgress", "itemsView": "full", "items": []any{map[string]any{"id": "user", "type": "userMessage"}}}
				result := map[string]any{"data": []any{turn}}
				switch test {
				case "unknown-turn":
					turn["id"] = "other"
				case "empty-items":
					turn["items"] = []any{}
				case "missing-items":
					delete(turn, "items")
				case "summary-items":
					turn["itemsView"] = "summary"
				case "bad-status":
					turn["status"] = "made-up"
				case "duplicate-item":
					turn["items"] = []any{map[string]any{"id": "same", "type": "userMessage"}, map[string]any{"id": "same", "type": "agentMessage"}}
				case "oversized":
					turn["items"] = []any{map[string]any{"id": "huge", "type": "agentMessage", "text": strings.Repeat("x", timelineMaxBytes)}}
				case "bad-cursor":
					result["nextCursor"] = 5
				case "empty-cursor":
					result["nextCursor"] = ""
				case "repeated-cursor":
					result["nextCursor"] = "same"
				case "repeated-turn":
					turn["id"] = "same"
					result["nextCursor"] = fmt.Sprint(pageNumber)
				}
				return result, false
			})
			defer server.Close()
			p := dialTestProtocol(t, server)
			selected := ""
			want := core.ErrTimelineUnavailable
			if test == "unknown-turn" {
				selected = "missing"
				want = core.ErrTurnNotFound
			}
			if test == "repeated-cursor" || test == "repeated-turn" {
				selected = "older"
			}
			result, err := p.readTimeline(context.Background(), "owned", selected)
			if !errors.Is(err, want) || len(result.Items) != 0 {
				t.Fatalf("result=%+v err=%v want=%v", result, err, want)
			}
		})
	}
}
