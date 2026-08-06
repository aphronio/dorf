package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/incus"
	"github.com/coder/websocket"
)

type capturingRunner struct {
	command string
	args    []string
}

func (r *capturingRunner) Run(_ context.Context, command string, _ []byte, args ...string) (incus.Result, error) {
	r.command = command
	r.args = append([]string(nil), args...)
	return incus.Result{}, nil
}

func TestAppServerLifecycleTracksAndStopsTheNativeGuestProcess(t *testing.T) {
	launch := appServerScript("ws://10.0.0.2:4500")
	if !strings.Contains(launch, `printf '%s\n' "$$" > `+serverPIDPath) {
		t.Fatalf("launch does not retain the native PID: %s", launch)
	}
	if !strings.Contains(launch, "; exec codex app-server ") {
		t.Fatalf("native PID would not survive exec: %s", launch)
	}

	runner := &capturingRunner{}
	agent := Agent{Sandbox: incus.Sandbox{Runner: runner}}
	if err := agent.stopServer(context.Background(), "sandbox-1"); err != nil {
		t.Fatal(err)
	}
	if runner.command != "incus" || len(runner.args) < 6 {
		t.Fatalf("unexpected stop command: %s %v", runner.command, runner.args)
	}
	script := runner.args[len(runner.args)-1]
	for _, required := range []string{serverPIDPath, "/proc/$pid/cmdline", `kill "$pid"`, "[c]odex app-server"} {
		if !strings.Contains(script, required) {
			t.Fatalf("stop script is missing %q: %s", required, script)
		}
	}
}

func TestProtocolSendsStableAgentRunIdentityAndKeepsOnlyNativeOutcome(t *testing.T) {
	requests := make(chan map[string]any, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		for {
			kind, payload, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if kind != websocket.MessageText {
				t.Error("non-text request")
				return
			}
			var request map[string]any
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Error(err)
				return
			}
			requests <- request
			method, _ := request["method"].(string)
			id, hasID := request["id"]
			if !hasID {
				continue
			}
			var result map[string]any
			switch method {
			case "initialize":
				result = map[string]any{}
			case "turn/start":
				result = map[string]any{"turn": map[string]any{"id": "turn-native-1"}}
			default:
				t.Errorf("unexpected method %s", method)
				return
			}
			response, _ := json.Marshal(map[string]any{"id": id, "result": result})
			if err := conn.Write(ctx, websocket.MessageText, response); err != nil {
				return
			}
			if method == "turn/start" {
				completed, _ := json.Marshal(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "session-native-1", "turn": map[string]any{"id": "turn-native-1", "status": "completed", "items": []any{map[string]any{"type": "agentMessage", "text": "transcript stays native"}}}}})
				_ = conn.Write(ctx, websocket.MessageText, completed)
			}
		}
	}))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	p := &protocol{connection: conn}
	if err := p.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	outcome, err := p.startTurn(context.Background(), "session-native-1", "/workspace/job", "action-stable-agent-run", "complete goal", "gpt-5.6-sol", "high")
	if err != nil {
		t.Fatal(err)
	}
	if outcome != (TurnOutcome{ID: "turn-native-1", Status: "completed"}) {
		t.Fatalf("outcome=%#v", outcome)
	}
	<-requests // initialize
	<-requests // initialized
	turnRequest := <-requests
	params := turnRequest["params"].(map[string]any)
	if params["clientUserMessageId"] != "action-stable-agent-run" {
		t.Fatalf("clientUserMessageId=%v", params["clientUserMessageId"])
	}
}
