package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/coder/websocket"
)

func TestCompletedControlOperationDoesNotWaitForPeerClose(t *testing.T) {
	for _, operation := range []string{"history", "accepted submission", "rejected submission"} {
		t.Run(operation, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer scoped-test-capability" {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				serveWithoutCloseReply(t, r.Context(), conn, operation, release)
			}))
			defer server.Close()
			agent := Agent{Sandbox: &instructionSandbox{endpoint: "ws" + strings.TrimPrefix(server.URL, "http")}, Timeout: time.Second}
			done := make(chan struct{})
			var err error
			go func() {
				defer close(done)
				err = completedControlOperation(agent, operation)
			}()
			defer func() {
				close(release)
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("control operation did not settle after peer release")
				}
			}()
			select {
			case <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("completed control operation waited for peer close acknowledgement")
			}
		})
	}
}

func completedControlOperation(agent Agent, operation string) error {
	owner := testOwner("close")
	if operation == "history" {
		history, err := agent.ReadTurns(context.Background(), owner, "retained-thread")
		if err != nil {
			return err
		}
		if len(history.Turns) != 1 || !history.Turns[0].Terminal() {
			return fmt.Errorf("completed history changed")
		}
		return nil
	}
	binding, err := agent.SubmitNative(context.Background(), owner, core.Session{ThreadID: "retained-thread", Model: "fixture-model", ReasoningEffort: "high"}, core.NativeEvent{Type: core.InputMessage, ClientID: "run"}, core.HarnessInput{Text: "input"}, fixtureMutation("run", false))
	if operation == "rejected submission" {
		var rejected *RejectedError
		if !errors.As(err, &rejected) || !rejected.DefiniteNoSubmit() {
			return fmt.Errorf("definite submission rejection changed: %v", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if binding.TurnID != "accepted-turn" {
		return fmt.Errorf("acknowledged turn identity changed")
	}
	return nil
}

func serveWithoutCloseReply(t *testing.T, ctx context.Context, conn *websocket.Conn, operation string, release <-chan struct{}) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var request map[string]any
		if err := json.Unmarshal(data, &request); err != nil {
			t.Error(err)
			return
		}
		if request["id"] == nil {
			continue
		}
		method := stringValue(request["method"])
		result := map[string]any{}
		switch method {
		case "initialize", "thread/inject_items":
		case "thread/resume":
			result["thread"] = map[string]any{"id": "retained-thread"}
		case "thread/read":
			result["thread"] = map[string]any{"id": "retained-thread", "turns": []any{map[string]any{"id": "previous-turn", "status": "completed", "items": []any{}}}}
		case "turn/start":
			result["turn"] = map[string]any{"id": "accepted-turn"}
		default:
			t.Errorf("unexpected fixture RPC %q", method)
			return
		}
		response := map[string]any{"id": request["id"], "result": result}
		if method == "turn/start" && operation == "rejected submission" {
			response = map[string]any{"id": request["id"], "error": map[string]any{"code": -32000, "message": "rejected"}}
		}
		payload, _ := json.Marshal(response)
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			return
		}
		if method == "thread/read" || method == "turn/start" {
			// Deliberately stop reading: the result exists, but the peer cannot
			// participate in a WebSocket closing handshake until released.
			<-release
			return
		}
	}
}
