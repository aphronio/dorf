package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	terminalapp "github.com/aphronio/dorf/internal/terminal"
	"github.com/coder/websocket"
)

type steerOperationFixture struct {
	closed                        chan struct{}
	agent                         Agent
	sandbox                       *operationSandbox
	owner                         provider.Ownership
	initializes, reads, mutations atomic.Int32
	dropAcknowledgement, terminal bool
}

func newSteerOperationFixture(t *testing.T, dropAcknowledgement, terminal bool) *steerOperationFixture {
	t.Helper()
	f := &steerOperationFixture{
		owner: testOwner("steer-operation"), dropAcknowledgement: dropAcknowledgement,
		terminal: terminal, closed: make(chan struct{}, 8),
	}
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
				f.reads.Add(1)
				status := "inProgress"
				if f.terminal {
					status = "completed"
				}
				items := []any{}
				if f.mutations.Load() > 0 {
					items = append(items, map[string]any{"type": "userMessage", "clientId": "run"})
				}
				result["thread"] = map[string]any{"id": "thread", "turns": []any{map[string]any{"id": "target", "status": status, "items": items}}}
			case "thread/resume":
				result["thread"] = map[string]any{"id": "thread"}
			case "turn/steer":
				f.mutations.Add(1)
				if f.dropAcknowledgement {
					return
				}
				result["turnId"] = "target"
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

func (f *steerOperationFixture) externals() (terminalapp.Externals, core.Job, core.Delivery) {
	externals := terminalapp.Externals{
		Agent: f.agent, Sandbox: f.sandbox,
		Ownership: func(context.Context, string) (provider.Ownership, error) { return f.owner, nil },
	}
	job := core.Job{ID: f.owner.JobID}
	delivery := core.Delivery{
		AgentRun: core.AgentRun{ID: "run", State: core.AgentRunPending, Harness: Harness, ThreadID: "thread", MessageID: "message", JobID: job.ID, SandboxID: f.owner.SandboxID},
		Message:  core.Message{ID: "message", Intent: core.MessageSteer, TargetTurnID: "target", Input: "correction"},
	}
	return externals, job, delivery
}

func submitSteer(ctx context.Context, externals core.SteerExternals, job core.Job, delivery core.Delivery) error {
	history, err := externals.SteerHistory(ctx, job, delivery.AgentRun.SandboxID, delivery.AgentRun.ThreadID)
	if err != nil {
		return err
	}
	reconciliation := core.ReconcileSteer(delivery.AgentRun.ID, delivery.Message.TargetTurnID, history.Turns)
	switch reconciliation.Classification {
	case "target-terminal", "completed":
		return nil
	case "no-submit":
	default:
		return fmt.Errorf("unexpected steer reconciliation: %s", reconciliation.Classification)
	}
	accepted, err := externals.AgentSteer(ctx, job, delivery)
	if err != nil {
		return err
	}
	if accepted != delivery.Message.TargetTurnID {
		return errors.New("steer acknowledged a foreign target")
	}
	return nil
}

func TestNativeSteerOperationProductionCompositionCounts(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "per-call", true: "operation"}[scoped], func(t *testing.T) {
			f := newSteerOperationFixture(t, false, false)
			externals, job, delivery := f.externals()
			var err error
			if scoped {
				err = externals.WithSteerScope(context.Background(), job, delivery, func(ctx context.Context, bound core.SteerExternals) error {
					return submitSteer(ctx, bound, job, delivery)
				})
			} else {
				err = submitSteer(context.Background(), externals, job, delivery)
			}
			if err != nil {
				t.Fatal(err)
			}
			wantConnections := 2
			if scoped {
				wantConnections = 1
			}
			if f.sandbox.scopes != wantConnections || f.sandbox.probes != wantConnections || int(f.initializes.Load()) != wantConnections || f.reads.Load() != 1 || f.mutations.Load() != 1 {
				t.Fatalf("scopes=%d probes=%d initialize=%d history=%d mutations=%d", f.sandbox.scopes, f.sandbox.probes, f.initializes.Load(), f.reads.Load(), f.mutations.Load())
			}
			t.Logf("scopes=%d probes=%d initialize=%d history=%d mutations=%d", f.sandbox.scopes, f.sandbox.probes, f.initializes.Load(), f.reads.Load(), f.mutations.Load())
		})
	}
}

func TestNativeSteerOperationTerminalTargetDoesNotMutate(t *testing.T) {
	f := newSteerOperationFixture(t, false, true)
	externals, job, delivery := f.externals()
	if err := externals.WithSteerScope(context.Background(), job, delivery, func(ctx context.Context, bound core.SteerExternals) error {
		return submitSteer(ctx, bound, job, delivery)
	}); err != nil {
		t.Fatal(err)
	}
	if f.sandbox.scopes != 1 || f.sandbox.probes != 1 || f.initializes.Load() != 1 || f.reads.Load() != 1 || f.mutations.Load() != 0 {
		t.Fatalf("scopes=%d probes=%d initialize=%d history=%d mutations=%d", f.sandbox.scopes, f.sandbox.probes, f.initializes.Load(), f.reads.Load(), f.mutations.Load())
	}
}

func TestNativeSteerOperationEndsTransportWithoutCreatingObservation(t *testing.T) {
	f := newSteerOperationFixture(t, false, false)
	observations := NewObservations(context.Background(), nil)
	t.Cleanup(observations.Close)
	f.agent.Observations = observations
	externals, job, delivery := f.externals()
	if err := externals.WithSteerScope(context.Background(), job, delivery, func(ctx context.Context, bound core.SteerExternals) error {
		return submitSteer(ctx, bound, job, delivery)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.closed:
	case <-time.After(time.Second):
		t.Fatal("successful steer retained its operation transport")
	}
	observations.mu.Lock()
	active := len(observations.active)
	observations.mu.Unlock()
	if active != 0 {
		t.Fatalf("steer created %d observation lifetimes", active)
	}
}

func TestNativeSteerOperationLostAcknowledgementUsesFreshHistoryWithoutReplay(t *testing.T) {
	f := newSteerOperationFixture(t, true, false)
	externals, job, delivery := f.externals()
	err := externals.WithSteerScope(context.Background(), job, delivery, func(ctx context.Context, bound core.SteerExternals) error {
		if _, err := bound.SteerHistory(ctx, job, delivery.AgentRun.SandboxID, delivery.AgentRun.ThreadID); err != nil {
			return err
		}
		if _, err := bound.AgentSteer(ctx, job, delivery); err == nil {
			t.Fatal("expected lost acknowledgement")
		}
		history, err := bound.SteerHistory(ctx, job, delivery.AgentRun.SandboxID, delivery.AgentRun.ThreadID)
		if err != nil {
			return err
		}
		if got := core.ReconcileSteer(delivery.AgentRun.ID, delivery.Message.TargetTurnID, history.Turns); got.Classification != "completed" {
			t.Fatalf("fresh history reconciliation=%#v", got)
		}
		if _, err := bound.AgentSteer(ctx, job, delivery); err == nil {
			t.Fatal("invalid operation replayed mutation")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.sandbox.scopes != 2 || f.sandbox.probes != 2 || f.initializes.Load() != 2 || f.reads.Load() != 2 || f.mutations.Load() != 1 {
		t.Fatalf("scopes=%d probes=%d initialize=%d history=%d mutations=%d", f.sandbox.scopes, f.sandbox.probes, f.initializes.Load(), f.reads.Load(), f.mutations.Load())
	}
}

func TestNativeSteerOperationRejectsCancellationForeignBindingAndEscapedScope(t *testing.T) {
	f := newSteerOperationFixture(t, false, false)
	var escaped terminalapp.Harness
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := f.agent.WithOperation(ctx, f.owner, "thread", func(ctx context.Context, bound terminalapp.Harness) error {
		escaped = bound
		foreign := f.owner
		foreign.JobID = "other"
		if _, err := bound.SteerTurn(ctx, foreign, "thread", "target", "run", core.HarnessInput{Text: "foreign"}); err == nil {
			t.Fatal("foreign owner accepted")
		}
		if _, err := bound.SteerTurn(ctx, f.owner, "other", "target", "run", core.HarnessInput{Text: "foreign"}); err == nil {
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
		if _, err := bound.SteerTurn(context.Background(), f.owner, "thread", "target", "run", core.HarnessInput{Text: "cancelled"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := escaped.SteerTurn(context.Background(), f.owner, "thread", "target", "run", core.HarnessInput{Text: "escaped"}); err == nil {
		t.Fatal("escaped scope accepted")
	}
	if f.mutations.Load() != 0 {
		t.Fatalf("rejected scoped mutations=%d", f.mutations.Load())
	}
}
