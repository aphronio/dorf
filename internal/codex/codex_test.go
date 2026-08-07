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

type probeRunner struct{ result incus.Result }

func (r probeRunner) Run(_ context.Context, _ string, _ []byte, _ ...string) (incus.Result, error) {
	return r.result, nil
}

func TestAppServerLifecycleDetachesAndTracksTheNativeGuestProcess(t *testing.T) {
	launch := appServerScript("ws://10.0.0.2:4500")
	if !strings.Contains(launch, `printf '%s\n' "$!" > `+serverPIDPath) {
		t.Fatalf("launch does not retain the native PID: %s", launch)
	}
	if !strings.Contains(launch, "nohup codex app-server ") {
		t.Fatalf("app-server is not detached from the executor: %s", launch)
	}

}

func TestLiveServerProbeRetainsItsAuthenticatedCapability(t *testing.T) {
	agent := Agent{Sandbox: incus.Sandbox{Runner: probeRunner{result: incus.Result{Stdout: "1\nprivate-capability\n"}}}}
	probe, err := agent.probeServer(context.Background(), "sandbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if !probe.running || probe.token != "private-capability" {
		t.Fatalf("probe=%#v", probe)
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
	if outcome != (TurnOutcome{ID: "turn-native-1", Status: "running"}) {
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
