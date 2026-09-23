package codex

import (
	"context"
	"encoding/json"
	"github.com/aphronio/dorf/internal/core"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCompletedConversationKeepsCommentaryAndFinalRepliesAcrossColdReads(t *testing.T) {
	active := []json.RawMessage{
		json.RawMessage(`{"id":"item-1","type":"userMessage","clientId":"run-1"}`),
		json.RawMessage(`{"id":"item-2","type":"agentMessage","phase":"commentary","text":"Working"}`),
		json.RawMessage(`{"id":"item-3","type":"agentMessage","phase":"final_answer","text":"[Report](sandbox:/report.pdf)"}`),
		json.RawMessage(`{"id":"item-4","type":"userMessage","clientId":"run-2"}`),
		json.RawMessage(`{"id":"item-5","type":"agentMessage","phase":"final_answer","text":"Confirmed"}`),
		json.RawMessage(`{"id":"item-6","type":"agentMessage","phase":null,"text":"","content":[{"text":"one"},{"text":"two"}]}`),
		json.RawMessage(`{"id":"item-7","type":"agentMessage","phase":"final_answer","text":"Confirmed"}`),
	}
	items, err := completedConversationItems(active)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 7 {
		t.Fatalf("items=%+v", items)
	}
	if items[0].Kind != "input" || items[0].ClientID != "run-1" || items[1].Kind != "assistant_message" || items[1].Phase != "commentary" || items[1].Text != "Working" || items[2].Phase != "final_answer" || items[2].Text != "[Report](sandbox:/report.pdf)" || items[3].ClientID != "run-2" || items[4].Text != "Confirmed" || items[5].Phase != "" || items[5].Text != "onetwo" || items[6].Text != "Confirmed" {
		t.Fatalf("items=%+v", items)
	}
	cold := make([]json.RawMessage, len(active))
	for i, raw := range active {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		value["id"] = "cold-" + value["id"].(string)
		cold[i], _ = json.Marshal(value)
	}
	restored, err := completedConversationItems(cold)
	if err != nil {
		t.Fatal(err)
	}
	for i := range restored {
		if items[i].Index != i || restored[i].Index != i || restored[i].NativeItemID == items[i].NativeItemID {
			t.Fatalf("unstable ordinal or fixture IDs: %+v / %+v", items, restored)
		}
		restored[i].NativeItemID = items[i].NativeItemID
	}
	if !reflect.DeepEqual(items, restored) {
		t.Fatalf("cold read changed completed entries: %+v / %+v", items, restored)
	}
}

func TestCompletedConversationProtocolMatchesNativeActiveAndColdSnapshots(t *testing.T) {
	raw, err := os.ReadFile("testdata/completed-conversation-0.154.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var modes map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &modes); err != nil {
		t.Fatal(err)
	}
	for mode, snapshots := range modes {
		t.Run(mode, func(t *testing.T) {
			results := make(map[string][]core.HarnessConversationItem)
			for _, state := range []string{"active", "terminal", "reconnect"} {
				var turn map[string]any
				if err := json.Unmarshal(snapshots[state], &turn); err != nil {
					t.Fatal(err)
				}
				server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
					if method == "initialize" {
						return map[string]any{}, false
					}
					if method != "thread/turns/list" || params["itemsView"] != "full" {
						t.Errorf("unexpected request %s %+v", method, params)
						return nil, true
					}
					return map[string]any{"data": []any{turn}}, false
				})
				p := dialTestProtocol(t, server)
				result, err := p.readTimeline(context.Background(), "bound", turn["id"].(string))
				server.Close()
				if err != nil {
					t.Fatal(err)
				}
				if state == "active" && result.Status != "inProgress" {
					t.Fatalf("status=%s", result.Status)
				}
				results[state] = result.CompletedItems
			}
			active, terminal, cold := results["active"], results["terminal"], results["reconnect"]
			if len(active) != 2 || len(terminal) != 5 || len(cold) != 5 || active[1].Kind != "assistant_message" || !strings.Contains(active[1].Text, "FIRST_REPLY") || !strings.Contains(cold[4].Text, "SECOND_REPLY") {
				t.Fatalf("entry counts=%d/%d/%d", len(active), len(terminal), len(cold))
			}
			for i := range cold {
				if cold[i].Index != i || terminal[i].Index != i {
					t.Fatal("ordinal changed")
				}
				cold[i].NativeItemID = terminal[i].NativeItemID
				if i < len(active) {
					active[i].NativeItemID = terminal[i].NativeItemID
				}
			}
			if !reflect.DeepEqual(terminal, cold) || !reflect.DeepEqual(active, terminal[:len(active)]) {
				t.Fatal("completed prefix changed between active, terminal, and cold reads")
			}
		})
	}
}

func TestRawTimelinePreservesItemsUnsupportedByCompletedProjection(t *testing.T) {
	for _, original := range []json.RawMessage{
		json.RawMessage(`{"id":"native","type":"agentMessage","phase":{"future":"classification"},"text":"original"}`),
		json.RawMessage(`{"id":"native","type":"agentMessage","phase":"final_answer","text":123}`),
		json.RawMessage(`{"id":"native","type":"userMessage","clientId":123}`),
		json.RawMessage(`{"id":"native","type":"agentMessage","content":"invalid"}`),
		json.RawMessage(`{"id":"native","type":"agentMessage","content":[{"text":123}]}`),
		json.RawMessage(`{"id":"native","type":"agentMessage","content":["invalid"]}`),
		json.RawMessage(`{"id":"native","type":"agentMessage","content":[null]}`),
	} {
		t.Run(string(original), func(t *testing.T) {
			server, _ := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
				if method == "initialize" {
					return map[string]any{}, false
				}
				return map[string]any{"data": []any{map[string]any{"id": "turn", "status": "inProgress", "itemsView": "full", "items": []any{original}}}}, false
			})
			defer server.Close()
			result, err := dialTestProtocol(t, server).readTimeline(context.Background(), "thread", "turn")
			if err != nil || len(result.Items) != 1 || string(result.Items[0]) != string(original) || result.CompletedItems != nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestCompletedConversationSkipsEmptyRepliesAndKeepsWhitespace(t *testing.T) {
	native := []json.RawMessage{
		json.RawMessage(`{"id":"empty","type":"agentMessage","phase":"final_answer","text":""}`),
		json.RawMessage(`{"id":"empty-block","type":"agentMessage","content":[{"text":""}]}`),
		json.RawMessage(`{"id":"null","type":"agentMessage","phase":null,"text":null,"content":null}`),
		json.RawMessage(`{"id":"absent","type":"agentMessage"}`),
		json.RawMessage(`{"id":"input","type":"userMessage","clientId":null}`),
		json.RawMessage(`{"id":"space","type":"agentMessage","text":" "}`),
		json.RawMessage(`{"id":"legacy","type":"agentMessage","content":[{"text":"one"},{"text":"two"}]}`),
	}
	items, err := completedConversationItems(native)
	if err != nil || len(items) != 3 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if items[0].Index != 0 || items[0].Kind != "input" || items[0].ClientID != "" || items[1].Index != 1 || items[1].Text != " " || items[2].Index != 2 || items[2].Text != "onetwo" {
		t.Fatalf("items=%+v", items)
	}
}

func TestCompletedConversationPublishesCommentaryWhenFinalIsEmpty(t *testing.T) {
	native := []json.RawMessage{
		json.RawMessage(`{"id":"input","type":"userMessage","clientId":"timed-task"}`),
		json.RawMessage(`{"id":"progress","type":"agentMessage","phase":"commentary","text":"Check the oven now."}`),
		json.RawMessage(`{"id":"empty-final","type":"agentMessage","phase":"final_answer","text":""}`),
		json.RawMessage(`{"id":"internal","type":"agentMessage","phase":"future_internal","text":"hidden"}`),
	}
	items, err := completedConversationItems(native)
	if err != nil || len(items) != 2 || items[0].ClientID != "timed-task" || items[1].Kind != "assistant_message" || items[1].Phase != "commentary" || items[1].Text != "Check the oven now." {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}
