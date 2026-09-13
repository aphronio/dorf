package codex

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

const steeredFinalTurnJSON = `{
  "id":"turn-final", "status":"completed", "items":[
    {"type":"userMessage","clientId":"request-original"},
    {"type":"agentMessage","phase":"commentary","text":"Creating the report."},
    {"type":"agentMessage","phase":"final_answer","text":"[PDF](/tmp/report.pdf)\n[Preview](/tmp/preview.png)"},
    {"type":"userMessage","clientId":"request-steer"},
    {"type":"agentMessage","phase":"final_answer","text":"Acknowledged the follow-up."}
  ]
}`

const steeredFinalOutput = "[PDF](/tmp/report.pdf)\n[Preview](/tmp/preview.png)\n\nAcknowledged the follow-up."

func TestParseTurnPreservesFinalRepliesAroundSteer(t *testing.T) {
	var turn map[string]any
	if err := json.Unmarshal([]byte(steeredFinalTurnJSON), &turn); err != nil {
		t.Fatal(err)
	}
	got := parseTurn(turn)
	want := TurnOutcome{ID: "turn-final", Status: "completed", Output: steeredFinalOutput,
		AcceptedMessageIDs: []string{"request-original", "request-steer"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed turn=%#v, want %#v", got, want)
	}
}

func TestParseTurnFinalMessageText(t *testing.T) {
	for _, test := range []struct {
		name, items, output string
	}{
		{"trailing commentary", `[{"type":"agentMessage","phase":"final_answer","text":"Finished."},{"type":"agentMessage","phase":"commentary","text":"Internal progress."}]`, "Finished."},
		{"commentary only", `[{"type":"agentMessage","phase":"commentary","text":"Still working."}]`, ""},
		{"content blocks", `[{"type":"agentMessage","phase":"final_answer","content":[{"type":"text","text":"Hello "},{"type":"text","text":"world."}]}]`, "Hello world."},
		{"direct text takes precedence", `[{"type":"agentMessage","phase":"final_answer","text":"Finished.","content":[{"type":"text","text":"Fallback."}]}]`, "Finished."},
		{"repeated authored replies", `[{"type":"agentMessage","phase":"final_answer","text":"Done."},{"type":"agentMessage","phase":"final_answer","text":"Done."}]`, "Done.\n\nDone."},
	} {
		t.Run(test.name, func(t *testing.T) {
			var items []any
			if err := json.Unmarshal([]byte(test.items), &items); err != nil {
				t.Fatal(err)
			}
			if got := parseTurn(map[string]any{"items": items}).Output; got != test.output {
				t.Fatalf("output=%q, want %q", got, test.output)
			}
		})
	}
}

func TestProtocolRecoversCompleteFinalRepliesWithoutResubmission(t *testing.T) {
	var turn map[string]any
	if err := json.Unmarshal([]byte(steeredFinalTurnJSON), &turn); err != nil {
		t.Fatal(err)
	}
	server, requests := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/list":
			requireProtocolParams(t, method, params, map[string]any{"cwd": "/workspace/job"})
			return map[string]any{"data": []any{map[string]any{"id": "thread-final"}}}, false
		case "thread/read":
			requireProtocolParams(t, method, params, map[string]any{"threadId": "thread-final", "includeTurns": true})
			return map[string]any{"thread": map[string]any{"id": "thread-final", "turns": []any{turn}}}, false
		default:
			t.Errorf("unexpected native operation %s", method)
			return nil, true
		}
	})
	defer server.Close()
	first := dialTestProtocol(t, server)
	turns, err := first.readTurns(context.Background(), "thread-final")
	if err != nil || len(turns) != 1 {
		t.Fatalf("first read turns=%#v error=%v", turns, err)
	}
	if turns[0].Output != steeredFinalOutput {
		t.Errorf("first read output=%q, want %q", turns[0].Output, steeredFinalOutput)
	}
	_ = first.connection.CloseNow()
	reconnected := dialTestProtocol(t, server)
	thread, recovered, err := reconnected.reconcileInitialTurn(context.Background(), "/workspace/job", "request-original", core.HarnessInput{Text: "Create the report."}, "model", "low", "danger-full-access")
	if err != nil || thread != "thread-final" || recovered.Output != steeredFinalOutput || !reflect.DeepEqual(recovered.AcceptedMessageIDs, []string{"request-original", "request-steer"}) {
		t.Errorf("recovered thread=%q turn=%#v error=%v", thread, recovered, err)
	}
	if methods := reviewProtocolMethods(requests); !reflect.DeepEqual(methods, []string{"thread/read", "thread/list", "thread/read"}) {
		t.Fatalf("native methods=%v", methods)
	}
}
