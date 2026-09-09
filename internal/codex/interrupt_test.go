package codex

import (
	"context"
	"sync"
	"testing"
)

func TestInterruptReconcilesLostAcknowledgementWithoutStoppingSuccessor(t *testing.T) {
	var mu sync.Mutex
	status, interrupts := "inProgress", 0
	server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/resume":
			requireProtocolParams(t, method, params, map[string]any{"threadId": "retained-thread"})
			return map[string]any{"thread": map[string]any{"id": "retained-thread"}}, false
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": "retained-thread", "turns": []any{
				map[string]any{"id": "old-turn", "status": status},
				map[string]any{"id": "successor", "status": "inProgress"},
			}}}, false
		case "turn/interrupt":
			requireProtocolParams(t, method, params, map[string]any{"threadId": "retained-thread", "turnId": "old-turn"})
			interrupts++
			status = "interrupted"
			return nil, true
		default:
			t.Errorf("unexpected native operation %s", method)
			return nil, true
		}
	})
	defer server.Close()
	if _, err := dialTestProtocol(t, server).interruptTurn(context.Background(), "retained-thread", "old-turn"); err == nil {
		t.Fatal("first interrupt should have an uncertain acknowledgement")
	}
	turn, err := dialTestProtocol(t, server).interruptTurn(context.Background(), "retained-thread", "old-turn")
	if err != nil || turn.ID != "old-turn" || turn.Status != "interrupted" {
		t.Fatalf("recovered outcome=%+v err=%v", turn, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if interrupts != 1 {
		t.Fatalf("recovery sent %d interrupts instead of observing the stopped turn", interrupts)
	}
}
