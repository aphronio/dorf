package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/coder/websocket"
)

// BenchmarkWarmHandoff measures native history inspection followed by accepted
// submission on an existing server/thread with unchanged workspace instructions.
// The real adapter and authenticated WebSocket transport run against a model-free
// protocol fixture. Provider delays are sensitivity inputs, not measured network
// latency. Provisioning, provider internals, workflow/database work, real native
// history parsing cost, inference, and completion observation are excluded.
//
// Run: go test ./internal/codex -run '^$' -bench '^BenchmarkWarmHandoff$' -benchtime=200ms -count=5
func BenchmarkWarmHandoff(b *testing.B) {
	for _, delay := range []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond} {
		b.Run("provider_delay="+delay.String(), func(b *testing.B) {
			fixture := newWarmHandoffFixture(b, delay)
			fixture.handoff(b, "warmup")
			fixture.agent.Observations.wg.Wait()
			fixture.counts.reset()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				fixture.handoff(b, fmt.Sprintf("run-%d", i))
				b.StopTimer()
				fixture.agent.Observations.wg.Wait()
				b.StartTimer()
			}
			b.StopTimer()
			fixture.counts.report(b)
		})
	}
}

type warmHandoffCounts struct {
	reads, execs, endpoints, connections, rpcs atomic.Int64
}

func (c *warmHandoffCounts) reset() {
	for _, counter := range []*atomic.Int64{&c.reads, &c.execs, &c.endpoints, &c.connections, &c.rpcs} {
		counter.Store(0)
	}
}

func (c *warmHandoffCounts) report(b *testing.B) {
	for unit, counter := range map[string]*atomic.Int64{
		"read/op": &c.reads, "exec/op": &c.execs, "endpoint/op": &c.endpoints,
		"ws/op": &c.connections, "rpc/op": &c.rpcs,
	} {
		b.ReportMetric(float64(counter.Load())/float64(b.N), unit)
	}
}

type warmHandoffFixture struct {
	agent  Agent
	counts *warmHandoffCounts
}

func newWarmHandoffFixture(b *testing.B, delay time.Duration) *warmHandoffFixture {
	b.Helper()
	counts := &warmHandoffCounts{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer benchmark-capability" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			b.Error(err)
			return
		}
		defer conn.CloseNow()
		counts.connections.Add(1)
		serveWarmHandoff(b, r.Context(), conn, counts)
	}))
	b.Cleanup(server.Close)
	observations := NewObservations(context.Background(), nil)
	b.Cleanup(observations.Close)
	return &warmHandoffFixture{
		agent:  Agent{Sandbox: &warmHandoffSandbox{endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), delay: delay, counts: counts}, Timeout: 5 * time.Second, Observations: observations},
		counts: counts,
	}
}

func (f *warmHandoffFixture) handoff(b *testing.B, runID string) {
	b.Helper()
	owner := testOwner("benchmark")
	ctx := telemetry.WithExecution(context.Background(), core.AgentRun{ID: runID, SessionID: owner.SessionID, SandboxID: owner.SandboxID, MessageID: runID})
	history, err := f.agent.ReadTurns(ctx, owner, "retained-thread")
	if err != nil || len(history.Turns) != 1 || !history.Turns[0].Terminal() {
		b.Fatalf("inspect retained history: turns=%d err=%v", len(history.Turns), err)
	}
	binding, err := f.agent.StartTurn(ctx, owner, "/workspace/job", "retained-thread", runID, core.HarnessInput{Text: "benchmark input"}, "fixture-model", "high", false)
	if err != nil || binding.Turn.ID != "native-"+runID {
		b.Fatalf("accept retained-thread submission: turn=%q err=%v", binding.Turn.ID, err)
	}
}

func serveWarmHandoff(b *testing.B, ctx context.Context, conn *websocket.Conn, counts *warmHandoffCounts) {
	write := func(value any) bool {
		data, err := json.Marshal(value)
		return err == nil && conn.Write(ctx, websocket.MessageText, data) == nil
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var request struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(data, &request); err != nil {
			b.Error(err)
			return
		}
		if request.ID == nil {
			continue
		}
		counts.rpcs.Add(1)
		result := map[string]any{}
		turnID := "native-" + stringValue(request.Params["clientUserMessageId"])
		switch request.Method {
		case "initialize", "thread/inject_items":
		case "thread/resume":
			result["thread"] = map[string]any{"id": "retained-thread"}
		case "thread/read":
			result["thread"] = map[string]any{"id": "retained-thread", "turns": []any{map[string]any{"id": "previous-turn", "status": "completed", "items": []any{}}}}
		case "turn/start":
			result["turn"] = map[string]any{"id": turnID}
		default:
			b.Errorf("unsupported fixture RPC %q", request.Method)
			return
		}
		if !write(map[string]any{"id": *request.ID, "result": result}) {
			return
		}
		if request.Method == "turn/start" {
			if !write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "retained-thread", "turn": map[string]any{"id": turnID, "status": "completed"}}}) {
				return
			}
		}
	}
}

type warmHandoffSandbox struct {
	provider.Sandbox
	endpoint string
	delay    time.Duration
	counts   *warmHandoffCounts
}

func (s *warmHandoffSandbox) wait(ctx context.Context, counter *atomic.Int64) error {
	counter.Add(1)
	if s.delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *warmHandoffSandbox) ReadFile(ctx context.Context, _ provider.Ownership, path string) ([]byte, error) {
	if err := s.wait(ctx, &s.counts.reads); err != nil {
		return nil, err
	}
	switch path {
	case "AGENTS.md", "SOUL.md":
		return []byte("Stable benchmark instructions."), nil
	default:
		return nil, os.ErrNotExist
	}
}

func (s *warmHandoffSandbox) ReadFiles(ctx context.Context, _ provider.Ownership, names []string, _ int) (map[string][]byte, error) {
	if err := s.wait(ctx, &s.counts.reads); err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(names))
	for _, name := range names {
		if name == "AGENTS.md" || name == "SOUL.md" {
			files[name] = []byte("Stable benchmark instructions.")
		}
	}
	return files, nil
}

func (s *warmHandoffSandbox) Endpoint(ctx context.Context, _ provider.Ownership, _ int) (provider.Endpoint, error) {
	err := s.wait(ctx, &s.counts.endpoints)
	return provider.NewEndpoint(s.endpoint, s.endpoint, nil), err
}

func (s *warmHandoffSandbox) Exec(ctx context.Context, _ provider.Ownership, _ []byte, args ...string) (provider.Result, error) {
	if err := s.wait(ctx, &s.counts.execs); err != nil {
		return provider.Result{}, err
	}
	if len(args) != 3 || args[0] != "bash" || args[1] != "-lc" || args[2] != probeServerScript(s.endpoint) {
		return provider.Result{}, fmt.Errorf("benchmark permits only existing-server inspection")
	}
	return provider.Result{Stdout: "1\n1\nbenchmark-capability\n"}, nil
}
