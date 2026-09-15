package codex

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestApplicationObservationNativeSubmissionAndColdAttribution(t *testing.T) {
	var submitted map[string]any
	server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize", "thread/resume":
			return map[string]any{"thread": map[string]any{"id": "thread"}}, false
		case "turn/start":
			submitted = params
			return map[string]any{"turn": map[string]any{"id": "observed-turn"}}, false
		}
		return nil, true
	})
	defer server.Close()
	p := dialTestProtocol(t, server)
	result, err := p.resumeAndStartTurn(context.Background(), "thread", "/workspace/job", "delivery-1", core.HarnessInput{Text: "Task finished", Observation: true}, "model", "high", "danger-full-access")
	if err != nil || result.ID != "observed-turn" {
		t.Fatalf("submit: %+v %v", result, err)
	}
	if len(submitted["input"].([]any)) != 0 || submitted["clientUserMessageId"] != nil {
		t.Fatal("observation became user input")
	}
	output := submitted["toolOutput"].(map[string]any)
	output["type"] = "functionCallOutput"
	output["id"] = "native-cold-id"
	raw, _ := json.Marshal(output)
	for _, reply := range []bool{false, true} {
		native := []json.RawMessage{raw}
		if reply {
			native = append(native, json.RawMessage(`{"id":"reply","type":"agentMessage","text":"Finished"}`))
		}
		public, err := conversationItems(native)
		if err != nil || len(public) != len(native)-1 {
			t.Fatalf("observation exposed in public timeline: %s %v", public, err)
		}
		items, err := completedConversationItems(native)
		if err != nil || items[0].Kind != "input" || items[0].ClientID != "delivery-1" {
			t.Fatalf("cold attribution: %+v %v", items, err)
		}
	}
	turn := parseTurn(map[string]any{"id": "observed-turn", "status": "completed", "items": []any{output}})
	if !reflect.DeepEqual(turn.AcceptedMessageIDs, []string{"delivery-1"}) {
		t.Fatalf("accepted: %+v", turn)
	}
	if accepted, err := p.steerTurn(context.Background(), "thread", "observed-turn", "other", core.HarnessInput{Observation: true, Text: "update"}); err != nil || accepted != "observed-turn" {
		t.Fatalf("active tool output: %s %v", accepted, err)
	}
	if len(submitted["input"].([]any)) != 0 || submitted["clientUserMessageId"] != nil || submitted["toolOutput"] == nil {
		t.Fatal("active observation became user input")
	}
	output["namespace"] = "external"
	if observationDeliveryID(output) != "" {
		t.Fatal("external output impersonated application input")
	}
}
