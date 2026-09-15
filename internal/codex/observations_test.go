package codex

import (
	"context"
	"encoding/json"
	"errors"
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
		if method == "thread/turns/list" {
			status := "inProgress"
			if reads > 1 {
				status = "interrupted"
			}
			return map[string]any{"data": []any{map[string]any{"id": "bound", "status": status, "itemsView": "full", "items": []any{map[string]any{"id": "input", "type": "userMessage", "content": []any{}}}}}, "nextCursor": nil}, false
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

func TestTerminalWakeIsAsynchronousExactAndCoalesced(t *testing.T) {
	wakes := make(chan core.NativeTerminalWakeTarget, 2)
	release := make(chan struct{})
	observations := NewObservations(context.Background(), nil, func(_ context.Context, target core.NativeTerminalWakeTarget) error {
		<-release
		wakes <- target
		return nil
	})
	p := &protocol{observations: observations, observed: &observedTurn{
		run:      core.AgentRun{ID: "run", JobID: "job", SandboxID: "sandbox"},
		threadID: "thread", turnID: "turn", complete: true,
	}}
	p.signalTerminalWake()
	p.signalTerminalWake()
	close(release)
	select {
	case target := <-wakes:
		want := (core.NativeTerminalWakeTarget{JobID: "job", SandboxID: "sandbox", AgentRunID: "run", ThreadID: "thread", TurnID: "turn"})
		if target != want {
			t.Fatalf("terminal wake target=%+v want=%+v", target, want)
		}
	case <-time.After(time.Second):
		t.Fatal("missing terminal wake")
	}
	observations.Close()
	if len(wakes) != 0 {
		t.Fatal("duplicate terminal wake")
	}
}

func TestTerminalWakeFailureEmitsBoundedDiagnostic(t *testing.T) {
	events := make(chan telemetry.Event, 1)
	observations := NewObservations(context.Background(), func(event telemetry.Event) { events <- event }, func(context.Context, core.NativeTerminalWakeTarget) error {
		return errors.New("database unavailable")
	})
	p := &protocol{observations: observations, observed: &observedTurn{
		run:      core.AgentRun{ID: "run", JobID: "job", SandboxID: "sandbox"},
		threadID: "thread", turnID: "turn", complete: true,
	}}
	p.signalTerminalWake()
	select {
	case event := <-events:
		if event.Name != "codex.native-terminal-wake.failed" || !event.Failed || event.Attributes["dorf.agent_run_id"] != "run" || event.Attributes["native.turn_id"] != "turn" {
			t.Fatalf("terminal wake diagnostic=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("missing terminal wake diagnostic")
	}
	observations.Close()
}
