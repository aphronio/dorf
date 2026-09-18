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

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
	terminalapp "github.com/aphronio/dorf/internal/terminal"
	"github.com/coder/websocket"
)

type operationSandbox struct {
	*instructionSandbox
	scopes, probes, batches int
	accessErr               error
}
type operationAccess struct{ *operationSandbox }

func (s *operationSandbox) WithAccess(_ context.Context, _ provider.Ownership, fn func(provider.Sandbox) error) error {
	s.scopes++
	if s.accessErr != nil {
		return s.accessErr
	}
	return fn(operationAccess{s})
}

// Nested access stays within the same callback-owned provider view.
func (s operationAccess) WithAccess(ctx context.Context, owner provider.Ownership, fn func(provider.Sandbox) error) error {
	return fn(s)
}
func (s *operationSandbox) Workspace() string { return "/workspace/job" }
func (s *operationSandbox) Exec(ctx context.Context, owner provider.Ownership, input []byte, args ...string) (provider.Result, error) {
	s.probes++
	return s.instructionSandbox.Exec(ctx, owner, input, args...)
}
func (s *operationSandbox) ReadFiles(context.Context, provider.Ownership, []string, int) (map[string][]byte, error) {
	s.batches++
	return map[string][]byte{}, nil
}

type operationFixture struct {
	closed              chan struct{}
	agent               Agent
	sandbox             *operationSandbox
	owner               provider.Ownership
	initializes, starts atomic.Int32
	drop                bool
}

func newOperationFixture(t *testing.T, drop bool) *operationFixture {
	t.Helper()
	f := &operationFixture{owner: testOwner("operation"), drop: drop, closed: make(chan struct{}, 8)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer scoped-test-capability" {
			http.Error(w, "forbidden", 403)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		defer func() { f.closed <- struct{}{} }()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request map[string]any
			if json.Unmarshal(data, &request) != nil {
				return
			}
			if request["id"] == nil {
				continue
			}
			result := map[string]any{}
			switch request["method"] {
			case "initialize":
				f.initializes.Add(1)
			case "thread/read":
				turns := []any{map[string]any{"id": "baseline", "status": "completed", "items": []any{}}}
				if f.starts.Load() > 0 {
					turns = append(turns, map[string]any{"id": "accepted", "status": "inProgress", "items": []any{}})
				}
				result["thread"] = map[string]any{"id": "thread", "turns": turns}
			case "thread/resume":
				result["thread"] = map[string]any{"id": "thread"}
			case "thread/inject_items":
			case "turn/start":
				f.starts.Add(1)
				if f.drop {
					return
				}
				result["turn"] = map[string]any{"id": "accepted"}
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
	f.sandbox = &operationSandbox{instructionSandbox: &instructionSandbox{endpoint: "ws" + strings.TrimPrefix(server.URL, "http")}}
	f.agent = Agent{Sandbox: f.sandbox, Timeout: 3 * time.Second}
	return f
}
func (f *operationFixture) operation(t *testing.T) (terminalapp.AgentRunOperation, core.AgentRun) {
	t.Helper()
	run := core.AgentRun{ID: "run", State: core.AgentRunPending, MessageID: "message", SessionID: f.owner.SessionID, SandboxID: f.owner.SandboxID, ThreadID: "thread"}
	op, err := terminalapp.NewAgentRunOperation(terminalapp.Externals{Agent: f.agent, Sandbox: f.sandbox, Ownership: func(context.Context, string) (provider.Ownership, error) { return f.owner, nil }}, core.AgentMessageExecution{
		Session: core.Session{ID: f.owner.SessionID}, Sandbox: core.Sandbox{ID: f.owner.SandboxID, SessionID: f.owner.SessionID}, AgentRun: run, Message: core.Message{ID: "message", Intent: core.MessageFollow},
	})
	if err != nil {
		t.Fatal(err)
	}
	return op, run
}
func TestNativeOperationProductionCompositionCounts(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "per-call", true: "operation"}[scoped], func(t *testing.T) {
			f := newOperationFixture(t, false)
			op, run := f.operation(t)
			follow := func(ctx context.Context, bound core.AgentRunOperation) error {
				history, err := bound.History(ctx, run)
				if err != nil {
					return err
				}
				if len(history.Turns) != 1 || history.Turns[0].ID != "baseline" {
					t.Fatal("missing baseline")
				}
				binding, err := bound.Submit(ctx, run, "follow")
				if err == nil && binding.Turn.ID != "accepted" {
					t.Fatal("wrong binding")
				}
				return err
			}
			var err error
			if scoped {
				err = op.WithScope(context.Background(), run, follow)
			} else {
				err = follow(context.Background(), op)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if scoped {
				want = 1
			}
			if f.sandbox.scopes != want || f.sandbox.probes != want || int(f.initializes.Load()) != want || f.sandbox.batches != 1 || f.starts.Load() != 1 {
				t.Fatalf("scopes=%d probes=%d initialize=%d batches=%d starts=%d", f.sandbox.scopes, f.sandbox.probes, f.initializes.Load(), f.sandbox.batches, f.starts.Load())
			}
			t.Logf("scopes=%d probes=%d initialize=%d batches=%d starts=%d", f.sandbox.scopes, f.sandbox.probes, f.initializes.Load(), f.sandbox.batches, f.starts.Load())
		})
	}
}
func TestNativeOperationLostAcknowledgementUsesFreshHistoryWithoutReplay(t *testing.T) {
	f := newOperationFixture(t, true)
	op, run := f.operation(t)
	err := op.WithScope(context.Background(), run, func(ctx context.Context, bound core.AgentRunOperation) error {
		if _, err := bound.History(ctx, run); err != nil {
			return err
		}
		if _, err := bound.Submit(ctx, run, "follow"); err == nil {
			t.Fatal("expected lost acknowledgement")
		}
		history, err := bound.History(ctx, run)
		if err != nil {
			return err
		}
		if len(history.Turns) != 2 || history.Turns[1].ID != "accepted" {
			t.Fatal("lost accepted turn")
		}
		if _, err := bound.Submit(ctx, run, "follow"); err == nil {
			t.Fatal("invalid scope replayed submission")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.sandbox.scopes != 2 || f.sandbox.probes != 2 || f.initializes.Load() != 2 || f.starts.Load() != 1 {
		t.Fatal("reconciliation did not use one fresh authenticated read")
	}
}
func TestNativeOperationRejectsCancellationForeignBindingAndEscapedScope(t *testing.T) {
	f := newOperationFixture(t, false)
	var escaped terminalapp.Harness
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := f.agent.WithOperation(ctx, f.owner, "thread", func(ctx context.Context, bound terminalapp.Harness) error {
		escaped = bound
		foreign := f.owner
		foreign.SessionID = "other"
		if _, err := bound.ReadTurns(ctx, foreign, "thread"); err == nil {
			t.Fatal("foreign owner accepted")
		}
		if _, err := bound.ReadTurns(ctx, f.owner, "other"); err == nil {
			t.Fatal("foreign thread accepted")
		}
		if _, err := bound.ReadTurns(ctx, f.owner, "thread"); err != nil {
			return err
		}
		cancel()
		select {
		case <-f.closed:
		case <-time.After(time.Second):
			t.Fatal("cancelled idle transport stayed open inside scope")
		}
		_, err := bound.StartTurn(context.Background(), f.owner, "/workspace/job", "thread", "run", core.HarnessInput{Text: "follow"}, "model", "high", false)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := escaped.ReadTurns(context.Background(), f.owner, "thread"); err == nil {
		t.Fatal("escaped scope accepted")
	}
	if f.starts.Load() != 0 {
		t.Fatal("cancelled operation submitted")
	}
}
func TestNativeOperationAccessFailureReachesContractCallback(t *testing.T) {
	f := newOperationFixture(t, false)
	failure := errors.New("access unavailable")
	f.sandbox.accessErr = failure
	op, run := f.operation(t)
	called := false
	err := op.WithScope(context.Background(), run, func(ctx context.Context, bound core.AgentRunOperation) error {
		called = true
		_, err := bound.History(ctx, run)
		return err
	})
	if !called || !errors.Is(err, failure) || f.initializes.Load() != 0 {
		t.Fatal("acquisition failure bypassed History")
	}
}

func TestNativeOperationHandsAcceptedObservationOffAfterScope(t *testing.T) {
	f := newInstructionFixture(t)
	owner := testOwner("operation-observation")
	release := make(chan struct{})
	session := &instructionSession{runID: "run", closed: make(chan struct{}), release: release}
	f.sessions <- session
	ctx, cancel := context.WithCancel(telemetry.WithExecution(context.Background(), core.AgentRun{ID: "run", SessionID: owner.SessionID, SandboxID: owner.SandboxID, MessageID: "message"}))
	defer cancel()
	err := f.agent.WithOperation(ctx, owner, "retained-thread", func(ctx context.Context, bound terminalapp.Harness) error {
		if _, err := bound.ReadTurns(ctx, owner, "retained-thread"); err != nil {
			return err
		}
		_, err := bound.StartTurn(ctx, owner, "/workspace/job", "retained-thread", "run", core.HarnessInput{Text: "follow"}, "model", "high", false)
		return err
	})
	cancel()
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("observation did not survive scope cancellation and settle")
	}
	f.agent.Observations.wg.Wait()
	key := instructionScope{sessionID: owner.SessionID, sandboxID: owner.SandboxID, threadID: "retained-thread"}
	f.agent.Observations.mu.Lock()
	_, known := f.agent.Observations.instructions[key]
	f.agent.Observations.mu.Unlock()
	if !known {
		t.Fatal("accepted instruction knowledge was lost at scope handoff")
	}
	if f.sandbox.endpoints != 1 {
		t.Fatal("observation handoff redialed native transport")
	}
}
