package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	terminalapp "github.com/aphronio/dorf/internal/terminal"
	"github.com/coder/websocket"
)

func TestInterruptUsesOnePostMutationHistoryRead(t *testing.T) {
	status, interrupts, reads := "inProgress", 0, 0
	server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/resume":
			requireProtocolParams(t, method, params, map[string]any{"threadId": "thread"})
			return map[string]any{"thread": map[string]any{"id": "thread"}}, false
		case "turn/interrupt":
			interrupts++
			status = "interrupted"
			return map[string]any{}, false
		case "thread/read":
			reads++
			return interruptHistory("thread", map[string]any{"id": "turn", "status": status}), false
		default:
			t.Fatalf("unexpected native operation %s", method)
			return nil, true
		}
	})
	defer server.Close()

	turn, attempted, err := dialTestProtocol(t, server).interruptTurn(context.Background(), "thread", "turn")
	if err != nil || !attempted || turn.ID != "turn" || turn.Status != "interrupted" {
		t.Fatalf("outcome=%+v attempted=%t err=%v", turn, attempted, err)
	}
	if interrupts != 1 || reads != 1 {
		t.Fatalf("interrupts=%d history_reads=%d, want 1 and 1", interrupts, reads)
	}
	t.Logf("healthy interrupt_attempts=%d accepted_effects=%d history_reads=%d", interrupts, interrupts, reads)
}

func TestInterruptNativeRejectionUsesExactPostRead(t *testing.T) {
	tests := []struct {
		name       string
		threadID   string
		turnID     string
		turns      []map[string]any
		wantStatus string
		wantError  bool
		wantReject bool
	}{
		{name: "terminal", threadID: "thread", turnID: "target", turns: []map[string]any{{"id": "target", "status": "completed"}}, wantStatus: "completed"},
		{name: "missing", threadID: "thread", turnID: "missing", turns: []map[string]any{{"id": "active", "status": "inProgress"}}, wantError: true},
		{name: "old with successor", threadID: "thread", turnID: "old", turns: []map[string]any{{"id": "old", "status": "completed"}, {"id": "successor", "status": "inProgress"}}, wantStatus: "completed"},
		{name: "rejected active", threadID: "thread", turnID: "target", turns: []map[string]any{{"id": "target", "status": "inProgress"}}, wantStatus: "inProgress", wantError: true, wantReject: true},
		{name: "foreign thread", threadID: "thread-a", turnID: "thread-b-turn", turns: []map[string]any{{"id": "thread-a-turn", "status": "inProgress"}}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			interrupts, reads := 0, 0
			server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
				switch method {
				case "initialize":
					return map[string]any{}, false
				case "thread/resume":
					return map[string]any{"thread": map[string]any{"id": test.threadID}}, false
				case "turn/interrupt":
					interrupts++
					requireProtocolParams(t, method, params, map[string]any{"threadId": test.threadID, "turnId": test.turnID})
					return nil, true
				case "thread/read":
					reads++
					values := make([]any, len(test.turns))
					for i := range test.turns {
						values[i] = test.turns[i]
					}
					return map[string]any{"thread": map[string]any{"id": test.threadID, "turns": values}}, false
				default:
					t.Fatalf("unexpected native operation %s", method)
					return nil, true
				}
			})
			defer server.Close()

			turn, attempted, err := dialTestProtocol(t, server).interruptTurn(context.Background(), test.threadID, test.turnID)
			var rejected *RejectedError
			if test.wantError != (err != nil) || test.wantReject != errors.As(err, &rejected) {
				t.Fatalf("outcome=%+v err=%v, want error=%t rejection=%t", turn, err, test.wantError, test.wantReject)
			}
			if !attempted || turn.Status != test.wantStatus {
				t.Fatalf("outcome=%+v attempted=%t", turn, attempted)
			}
			if interrupts != 1 || reads != 1 {
				t.Fatalf("interrupts=%d history_reads=%d, want 1 and 1", interrupts, reads)
			}
			t.Logf("native_rejections=1 history_reads=%d outcome=%s error=%t", reads, turn.Status, err != nil)
		})
	}
}

func TestInterruptLostAcknowledgementUsesFreshReadWithoutReplay(t *testing.T) {
	for _, cancelAfterMutation := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "caller cancelled"}[cancelAfterMutation], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			agent, sandbox, interrupts, reads := interruptAgentFixture(t, false, func() {
				if cancelAfterMutation {
					cancel()
				}
			})
			owner := testOwner("interrupt-recovery")
			var outcomeStatus string
			err := agent.WithOperation(ctx, owner, "thread", func(ctx context.Context, bound terminalapp.Harness) error {
				binding, err := bound.(terminalapp.InterruptibleHarness).InterruptTurn(ctx, owner, "thread", "turn")
				outcomeStatus = binding.Turn.Status
				return err
			})
			if cancelAfterMutation {
				if !errors.Is(err, context.Canceled) || sandbox.instructionSandbox.endpoints != 1 || reads.Load() != 0 {
					t.Fatalf("err=%v endpoints=%d reads=%d", err, sandbox.instructionSandbox.endpoints, reads.Load())
				}
			} else if err != nil || outcomeStatus != "interrupted" || sandbox.instructionSandbox.endpoints != 2 || reads.Load() != 1 {
				t.Fatalf("err=%v status=%s endpoints=%d reads=%d", err, outcomeStatus, sandbox.instructionSandbox.endpoints, reads.Load())
			}
			if interrupts.Load() != 1 {
				t.Fatalf("interrupt attempts=%d, want one accepted effect and no replay", interrupts.Load())
			}
			t.Logf("connections=%d interrupt_attempts=%d accepted_effects=1 recovery_reads=%d", sandbox.instructionSandbox.endpoints, interrupts.Load(), reads.Load())
		})
	}
}

func TestInterruptRejectedThenPostReadDisconnectsUsesFreshRead(t *testing.T) {
	agent, sandbox, interrupts, reads := interruptAgentFixture(t, true, func() {})
	owner := testOwner("interrupt-rejected-read-loss")
	var outcomeStatus string
	err := agent.WithOperation(context.Background(), owner, "thread", func(ctx context.Context, bound terminalapp.Harness) error {
		binding, err := bound.(terminalapp.InterruptibleHarness).InterruptTurn(ctx, owner, "thread", "turn")
		outcomeStatus = binding.Turn.Status
		return err
	})
	if err != nil || outcomeStatus != "interrupted" || sandbox.instructionSandbox.endpoints != 2 || interrupts.Load() != 1 || reads.Load() != 2 {
		t.Fatalf("err=%v status=%s endpoints=%d interrupts=%d reads=%d", err, outcomeStatus, sandbox.instructionSandbox.endpoints, interrupts.Load(), reads.Load())
	}
	t.Logf("connections=%d rejected_interrupts=%d history_attempts=%d final=%s", sandbox.instructionSandbox.endpoints, interrupts.Load(), reads.Load(), outcomeStatus)
}

func interruptHistory(threadID string, turns ...map[string]any) map[string]any {
	values := make([]any, len(turns))
	for i := range turns {
		values[i] = turns[i]
	}
	return map[string]any{"thread": map[string]any{"id": threadID, "turns": values}}
}

func interruptAgentFixture(t *testing.T, rejectInterrupt bool, afterMutation func()) (Agent, *operationSandbox, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var interrupts, reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer scoped-test-capability" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		rejectedOnConnection := false
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request map[string]any
			if json.Unmarshal(data, &request) != nil || request["id"] == nil {
				continue
			}
			result := map[string]any{}
			switch request["method"] {
			case "initialize":
			case "thread/resume":
				result["thread"] = map[string]any{"id": "thread"}
			case "turn/interrupt":
				interrupts.Add(1)
				afterMutation()
				if rejectInterrupt {
					response, _ := json.Marshal(map[string]any{"id": request["id"], "error": map[string]any{"code": -32600, "message": "native rejection"}})
					if conn.Write(r.Context(), websocket.MessageText, response) != nil {
						return
					}
					rejectedOnConnection = true
					continue
				}
				return
			case "thread/read":
				reads.Add(1)
				if rejectedOnConnection {
					return
				}
				result = interruptHistory("thread", map[string]any{"id": "turn", "status": "interrupted"})
			default:
				t.Errorf("unexpected native method %v", request["method"])
				return
			}
			response, _ := json.Marshal(map[string]any{"id": request["id"], "result": result})
			if conn.Write(r.Context(), websocket.MessageText, response) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	sandbox := &operationSandbox{instructionSandbox: &instructionSandbox{endpoint: endpoint}}
	return Agent{Sandbox: sandbox, Timeout: 2 * time.Second}, sandbox, &interrupts, &reads
}
