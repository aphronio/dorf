package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/coder/websocket"
)

func TestObservationsRetainExactTurnAfterSubmissionReturns(t *testing.T) {
	events := make(chan telemetry.Event, 32)
	observations := NewObservations(context.Background(), func(event telemetry.Event) { events <- event })
	defer observations.Close()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		send := func(value any) bool {
			data, _ := json.Marshal(value)
			return conn.Write(r.Context(), websocket.MessageText, data) == nil
		}
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request map[string]any
			if json.Unmarshal(data, &request) != nil {
				return
			}
			if request["method"] == "skills/list" {
				send(map[string]any{"id": request["id"], "result": map[string]any{}})
				continue
			}
			if request["method"] != "turn/start" {
				continue
			}
			params := request["params"].(map[string]any)
			runID := params["clientUserMessageId"].(string)
			turnID := "native-" + runID
			// This event can arrive before the acknowledgement establishes ownership.
			send(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": turnID, "status": "inProgress"}}})
			send(map[string]any{"id": request["id"], "result": map[string]any{"turn": map[string]any{"id": turnID}}})
			<-release
			for _, id := range []string{"foreign-turn", turnID} {
				send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread", "turnId": id, "item": map[string]any{"id": "nested-command", "type": "commandExecution", "command": "printf test", "exitCode": 0, "durationMs": 4}}})
				send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "thread", "turnId": id, "tokenUsage": map[string]any{"total": map[string]any{"inputTokens": 100}, "last": map[string]any{"inputTokens": 10}}}})
			}
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": turnID, "status": "completed"}}})
		}
	}))
	defer server.Close()
	for _, runID := range []string{"run-a", "run-b"} {
		conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		p := &protocol{connection: conn, observations: observations, execution: core.AgentRun{ID: runID, JobID: "job", MessageID: "message-" + runID}}
		turn, err := p.startTurn(context.Background(), "thread", "/tmp", runID, core.HarnessInput{Text: "same prompt"}, "model", "high", "danger-full-access")
		if err != nil {
			t.Fatal(err)
		}
		if turn.ID != "native-"+runID {
			t.Fatal(turn)
		}
		p.finish()
	}
	close(release)
	counts := map[string]int{}
	for range 8 {
		select {
		case event := <-events:
			runID := event.Attributes["dorf.agent_run_id"].(string)
			if event.Attributes["dorf.message_id"] != "message-"+runID || event.Attributes["native.turn_id"] != "native-"+runID {
				t.Fatalf("incorrect ownership: %#v", event)
			}
			counts[runID]++
		case <-time.After(5 * time.Second):
			t.Fatal("missing attributed event")
		}
	}
	observations.Close()
	if counts["run-a"] != 4 || counts["run-b"] != 4 || len(events) != 0 {
		t.Fatalf("counts=%v extra=%d", counts, len(events))
	}
}

func TestObservationsRecoverOnlyDurablyBoundTurn(t *testing.T) {
	events := make(chan telemetry.Event, 16)
	observations := NewObservations(context.Background(), func(event telemetry.Event) { events <- event })
	defer observations.Close()
	reads := 0
	server, _ := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
		if method == "thread/resume" {
			return map[string]any{"thread": map[string]any{"id": "thread"}}, false
		}
		if method == "thread/read" {
			reads++
			status := "inProgress"
			if reads > 1 {
				status = "interrupted"
			}
			return map[string]any{"thread": map[string]any{"id": "thread", "turns": []any{
				map[string]any{"id": "foreign", "status": "failed", "items": []any{map[string]any{"id": "foreign-tool", "type": "commandExecution", "exitCode": 2}}},
				map[string]any{"id": "bound", "status": status, "items": []any{map[string]any{"id": "failed-tool", "type": "commandExecution", "exitCode": 1}}},
			}}}, false
		}
		return map[string]any{}, false
	})
	defer server.Close()
	p := dialTestProtocol(t, server)
	p.observations = observations
	p.execution = core.AgentRun{ID: "run", MessageID: "message", JobID: "job", TurnID: "bound"}
	turns, err := p.readTurns(context.Background(), "thread")
	if err != nil || len(turns) != 2 {
		t.Fatalf("turns=%v err=%v", turns, err)
	}
	p.finish()
	select {
	case settled := <-events:
		if settled.Name != "codex.turn.snapshot" || settled.Attributes["native.turn_id"] != "bound" || settled.Attributes["status"] != "interrupted" {
			t.Fatalf("settled=%#v", settled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing exact-turn recovery snapshot")
	}
	observations.Close()
	if len(events) != 0 {
		t.Fatalf("got %d events", len(events))
	}
}
