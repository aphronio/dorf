package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/e2b"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/coder/websocket"
)

// This opt-in diagnostic reads only an existing native thread. Connection
// credentials arrive on stdin and neither credentials nor native content are emitted.
func TestReadOnlyLatencyDiagnostic(t *testing.T) {
	if os.Getenv("DORF_LATENCY_PROBE") != "1" {
		t.Skip("requires explicit read-only diagnostic input")
	}
	var cfg struct {
		APIKey  string
		Owner   provider.Ownership
		Thread  string
		Samples int
		Warmups int
		Mode    string
		Variant string
	}
	if json.NewDecoder(os.Stdin).Decode(&cfg) != nil || cfg.Thread == "" || cfg.Samples < 1 || cfg.Samples > 10 || cfg.Warmups < 0 || cfg.Warmups > 2 || (cfg.Mode != "" && cfg.Mode != "read") || (cfg.Variant != "" && cfg.Variant != "baseline" && cfg.Variant != "scoped") {
		t.Fatal("invalid diagnostic input")
	}
	client := diagnosticHTTP{client: &http.Client{Timeout: 30 * time.Second}}
	adapter := e2b.Adapter{Client: e2b.Client{APIKey: cfg.APIKey, HTTPClient: client}, Config: e2b.AdapterConfig{Workspace: "/workspace", SandboxTimeout: 10 * time.Minute, ProcessTimeout: 30 * time.Second}}
	agent := Agent{Sandbox: adapter, Port: 4500, Timeout: 30 * time.Second}
	ctx := context.Background()
	var historyDigest [32]byte
	stable := func(turns any) {
		body, err := json.Marshal(turns)
		if err != nil {
			t.Fatal("cannot fingerprint history")
		}
		digest := sha256.Sum256(body)
		if historyDigest != [32]byte{} && historyDigest != digest {
			t.Fatal("native history changed during diagnostic")
		}
		historyDigest = digest
	}
	check := func(stage string, err error) {
		diagnosticCheck(t, stage, err)
	}
	if cfg.Mode == "read" {
		diagnosticReadSamples(t, agent, adapter, cfg.Owner, cfg.Thread, cfg.Variant, cfg.Samples, cfg.Warmups, stable)
		return
	}
	for i := 0; i < cfg.Samples; i++ {
		start := time.Now()
		endpoint, err := adapter.Endpoint(ctx, cfg.Owner, 4500)
		check("endpoint", err)
		diagnosticTiming(i, "endpoint", start)
		start = time.Now()
		probe, err := agent.probeServer(ctx, cfg.Owner, endpoint.ListenURL)
		check("probe", err)
		if !probe.running || !probe.tracked || probe.token == "" {
			t.Fatal("fixture requires a retained authenticated server")
		}
		diagnosticTiming(i, "probe", start)
		start = time.Now()
		p, err := dialProtocol(ctx, endpoint.DialURL, probe.token, endpoint.Headers(), endpoint.DialContext())
		check("dial", err)
		diagnosticTiming(i, "dial_initialize", start)
		start = time.Now()
		turns, err := p.readTurns(ctx, cfg.Thread)
		check("read", err)
		if len(turns) == 0 {
			t.Fatal("fixture requires existing native history")
		}
		diagnosticTiming(i, "native_read", start)
		stable(turns)
		start = time.Now()
		p.connection.Close(websocket.StatusNormalClosure, "done")
		diagnosticTiming(i, "finish_graceful", start)
		p, err = dialProtocol(ctx, endpoint.DialURL, probe.token, endpoint.Headers(), endpoint.DialContext())
		check("dial_repeat", err)
		turns, err = p.readTurns(ctx, cfg.Thread)
		check("read_repeat", err)
		stable(turns)
		for j := 0; j < 3; j++ {
			start = time.Now()
			turns, err = p.readTurns(ctx, cfg.Thread)
			check("read_retained", err)
			diagnosticTiming(i, "native_read_retained", start)
			stable(turns)
		}
		start = time.Now()
		p.connection.CloseNow()
		diagnosticTiming(i, "finish_now", start)
		variants := []bool{false, true}
		if i%2 == 1 {
			variants = []bool{true, false}
		}
		for _, scoped := range variants {
			selected := agent
			name := "agent_read_scoped"
			if !scoped {
				selected.Sandbox = struct{ provider.Sandbox }{adapter}
				name = "agent_read_baseline"
			}
			start = time.Now()
			history, err := selected.ReadTurns(ctx, cfg.Owner, cfg.Thread)
			check(name, err)
			diagnosticTiming(i, name, start)
			stable(history.Turns)
		}
		diagnosticInstructions(t, i, agent, adapter, cfg.Owner)
	}
}

// diagnosticReadSamples compares complete adapter reads without unrelated setup
// probes. Negative iterations are explicit warmups, excluded from measured data.
func diagnosticReadSamples(t *testing.T, agent Agent, adapter e2b.Adapter, owner provider.Ownership, thread, variant string, samples, warmups int, stable func(any)) {
	t.Helper()
	for iteration := -warmups; iteration < samples; iteration++ {
		variants := []string{"baseline", "scoped"}
		if iteration%2 != 0 {
			variants[0], variants[1] = variants[1], variants[0]
		}
		if variant != "" {
			variants = []string{variant}
		}
		for _, name := range variants {
			selected := agent
			if name == "baseline" {
				selected.Sandbox = struct{ provider.Sandbox }{adapter}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			start := time.Now()
			history, err := selected.ReadTurns(ctx, owner, thread)
			cancel()
			diagnosticCheck(t, "agent_read_"+name, err)
			diagnosticTiming(iteration, "agent_read_"+name, start)
			if len(history.Turns) == 0 {
				t.Fatal("fixture requires existing native history")
			}
			stable(history.Turns)
			body, err := json.Marshal(history.Turns)
			diagnosticCheck(t, "history_fingerprint", err)
			data, _ := json.Marshal(map[string]any{"iteration": iteration, "stage": "history_fingerprint", "sha256": fmt.Sprintf("%x", sha256.Sum256(body))})
			fmt.Println(string(data))
		}
	}
}

// Error messages can contain remote content or credentials. Emit only finite
// protocol codes, error types, and absence/presence flags when a probe fails.
func diagnosticCheck(t *testing.T, stage string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	cause := err
	var uncertain *e2b.IndeterminateExecError
	if errors.As(err, &uncertain) {
		cause = uncertain.Cause
		t.Logf("indeterminate process: pid_present=%t cause_type=%T", uncertain.PID != 0, cause)
	}
	t.Logf("failure codes: connect=%s cancelled=%t deadline=%t eof=%t unexpected_eof=%t", connect.CodeOf(cause), errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded), errors.Is(cause, io.EOF), errors.Is(cause, io.ErrUnexpectedEOF))
	var api *e2b.APIError
	if errors.As(cause, &api) {
		t.Logf("provider codes: http=%d code=%d", api.StatusCode, api.Code)
	}
	t.Fatalf("%s failed (%T)", stage, err)
}

func diagnosticInstructions(t *testing.T, iteration int, agent Agent, adapter e2b.Adapter, owner provider.Ownership) {
	t.Helper()
	variants := []struct {
		name    string
		sandbox provider.Sandbox
	}{
		{"sequential", struct{ provider.Sandbox }{adapter}},
		{"batch", diagnosticBatchOnly{Sandbox: adapter, batch: adapter}},
		{"batch_scoped", adapter},
	}
	if iteration%2 == 1 {
		variants[0], variants[2] = variants[2], variants[0]
	}
	var expected instructionHashes
	for index, variant := range variants {
		selected := agent
		selected.Sandbox = variant.sandbox
		start := time.Now()
		var hashes instructionHashes
		err := selected.withSandboxAccess(context.Background(), owner, func(bound Agent) error {
			instructions, err := bound.readWorkspaceInstructions(context.Background(), owner, adapter.Workspace())
			if err != nil {
				return err
			}
			hashes = instructions.hashes
			return bound.withServer(context.Background(), owner, func(*protocol) error { return nil })
		})
		if err != nil {
			t.Fatalf("instruction setup %s failed (%T)", variant.name, err)
		}
		diagnosticTiming(iteration, "setup_"+variant.name, start)
		if index > 0 && expected != hashes {
			t.Fatal("instruction files changed across variants")
		}
		expected = hashes
	}
}

type diagnosticBatchOnly struct {
	provider.Sandbox
	batch provider.FileBatchReader
}

func (s diagnosticBatchOnly) ReadFiles(ctx context.Context, owner provider.Ownership, names []string, maxBytes int) (map[string][]byte, error) {
	return s.batch.ReadFiles(ctx, owner, names, maxBytes)
}

type diagnosticHTTP struct{ client *http.Client }

func (c diagnosticHTTP) Do(r *http.Request) (*http.Response, error) {
	stage := "provider_other_headers"
	if r.URL.Path == "/v2/sandboxes" {
		stage = "provider_find_headers"
	}
	if strings.HasSuffix(r.URL.Path, "/connect") {
		stage = "provider_connect_headers"
	}
	if strings.Contains(r.URL.Path, "process.Process") {
		stage = "provider_exec_headers"
	}
	start := time.Now()
	response, err := c.client.Do(r)
	diagnosticTiming(-1, stage, start)
	return response, err
}

func diagnosticTiming(iteration int, stage string, start time.Time) {
	data, _ := json.Marshal(map[string]any{"iteration": iteration, "stage": stage, "seconds": time.Since(start).Seconds()})
	fmt.Println(string(data))
}
