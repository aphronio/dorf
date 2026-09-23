package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/coder/websocket"
)

// Exercises stock Codex and the production launcher, not real Gateway routing
// or a model. A synthetic shell call proves SDK tools inherit the installed route.
func TestLiveCodexRouteCompletesSyntheticInference(t *testing.T) {
	if os.Getenv("DORF_CODEX_ROUTE_OVERRIDE_LIVE") != "1" {
		t.Skip("set DORF_CODEX_ROUTE_OVERRIDE_LIVE=1 to run isolated local Codex")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "unsupported", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-route-key" {
			t.Error("provider did not receive the installed scoped credential")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Logf("non-upgrade %s %s: %v", r.Method, r.URL.Path, err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(4 << 20)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		_, payload, err := conn.Read(ctx)
		if err != nil {
			t.Errorf("read provider request: %v", err)
			return
		}
		var request struct {
			Type     string `json:"type"`
			Model    string `json:"model"`
			Generate *bool  `json:"generate"`
		}
		if err := json.Unmarshal(payload, &request); err != nil || request.Type != "response.create" || request.Model != "synthetic-model" {
			t.Errorf("unexpected request type=%q model=%q decode=%v", request.Type, request.Model, err)
			return
		}
		// Native connection warmup is not a model turn and must not call a tool.
		if request.Generate != nil && !*request.Generate {
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_warmup","status":"completed"}}`)); err != nil {
				t.Error(err)
				return
			}
			if _, _, err := conn.Read(ctx); err != nil {
				t.Error(err)
				return
			}
		}
		arguments, _ := json.Marshal(map[string]any{"cmd": `test "$OPENAI_BASE_URL" = 'http://` + r.Host + `/v1' && test -n "$OPENAI_API_KEY" && test "$OPENAI_API_KEY" = "$DORF_PROVIDER_ROUTE_KEY" && printf sdk-route-ready`, "login": false})
		toolCall, _ := json.Marshal(map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{"type": "function_call", "call_id": "sdk-env", "name": "exec_command", "arguments": string(arguments)},
		})
		for _, event := range [][]byte{toolCall, []byte(`{"type":"response.completed","response":{"id":"resp_tool","status":"completed"}}`)} {
			if err := conn.Write(ctx, websocket.MessageText, event); err != nil {
				t.Error(err)
				return
			}
		}
		_, payload, err = conn.Read(ctx)
		if err != nil {
			t.Errorf("read shell result: %v", err)
			return
		}
		var followup struct {
			Input []struct{ Type, Output string } `json:"input"`
		}
		if err := json.Unmarshal(payload, &followup); err != nil {
			t.Error(err)
			return
		}
		found := false
		for _, item := range followup.Input {
			if item.Type == "function_call_output" && strings.Contains(item.Output, "sdk-route-ready") {
				found = true
			}
		}
		if !found {
			t.Errorf("native shell did not inherit SDK route environment: %+v", followup.Input)
		}
		for _, line := range strings.Split(syntheticRouteResponse, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if err := conn.Write(ctx, websocket.MessageText, []byte(strings.TrimPrefix(line, "data: "))); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}))
	defer mock.Close()
	home := t.TempDir()
	nativeHome := filepath.Join(home, ".codex")
	if err := os.Mkdir(nativeHome, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(nativeHome, "config.toml")
	if err := os.WriteFile(configPath, []byte(clientRouteConfig), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	endpoint, stop := startOverrideServer(t, ctx, executable, home, nativeHome, mock.URL+"/v1")
	defer stop()
	p := connectOverrideServer(t, ctx, endpoint)
	defer p.connection.CloseNow()
	thread, err := p.startThread(ctx, home, "synthetic-model", "read-only")
	if err != nil {
		t.Fatal(err)
	}
	turn, err := p.startTurn(ctx, thread, home, "synthetic-run", core.HarnessInput{Text: "Reply with a short greeting."}, "synthetic-model", "low", "read-only")
	if err != nil {
		t.Fatal(err)
	}
	for !terminal(turn.Status) {
		if err := p.pollTurn(ctx, thread, turn.ID, &turn); err != nil {
			t.Fatal(err)
		}
	}
	if turn.Status != "completed" || !strings.Contains(turn.Output, "synthetic-route-reply") {
		t.Fatalf("synthetic inference did not complete: %#v", turn)
	}
	// No subscription survives reconnect or process restart. Usage must come
	// from native persisted history, including a second Turn in the same Thread.
	second, err := p.startTurn(ctx, thread, home, "synthetic-followup", core.HarnessInput{Text: "Reply again."}, "synthetic-model", "low", "read-only")
	if err != nil {
		t.Fatal(err)
	}
	for !terminal(second.Status) {
		if err := p.pollTurn(ctx, thread, second.ID, &second); err != nil {
			t.Fatal(err)
		}
	}
	_ = p.connection.CloseNow()
	stop()
	endpoint, stop = startOverrideServer(t, ctx, executable, home, nativeHome, mock.URL+"/v1")
	defer stop()
	for i := 0; i < 2; i++ {
		recovered := connectOverrideServer(t, ctx, endpoint)
		threadState, err := recovered.readThread(ctx, thread)
		if err != nil {
			t.Fatal(err)
		}
		turns, err := recovered.parseReadTurns(thread, threadState)
		if err != nil {
			t.Fatal(err)
		}
		agent := Agent{Sandbox: continuitySandbox{root: home}}
		if err := agent.readUsage(ctx, provider.Ownership{}, thread, stringValue(threadState["path"]), turns); err != nil {
			t.Fatal(err)
		}
		if len(turns) != 2 {
			t.Fatalf("retained Turns=%d", len(turns))
		}
		for _, got := range turns {
			if got.Usage == nil || *got.Usage.InputTokens != 1200 || *got.Usage.OutputTokens != 100 || *got.Usage.TotalTokens != 1300 || *got.Usage.InputTokensDetails.CachedTokens != 800 || *got.Usage.OutputTokensDetails.ReasoningTokens != 40 {
				t.Fatalf("recovered usage=%+v", got.Usage)
			}
			if got.Execution == nil || got.Execution.Model != "synthetic-model" || got.Execution.Reasoning != "low" || len(got.RequestUsage) != 1 {
				t.Fatalf("recovered execution=%+v requests=%d", got.Execution, len(got.RequestUsage))
			}
		}
		_ = recovered.connection.CloseNow()
	}
	stop()
	// Copy only the synthetic native home and workspace marker into another
	// isolated home. The original Thread ID must resume there without edits to
	// opaque history, and subsequent native Turns must diverge independently.
	sourceMarker := filepath.Join(home, "synthetic-state.txt")
	if err := os.WriteFile(sourceMarker, []byte("checkpoint-state"), 0600); err != nil {
		t.Fatal(err)
	}
	branchHome := t.TempDir()
	branchNativeHome := filepath.Join(branchHome, ".codex")
	if err := os.Mkdir(branchNativeHome, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("cp", "-a", nativeHome+"/.", branchNativeHome).CombinedOutput(); err != nil {
		t.Fatalf("copy isolated native home: %v: %s", err, output)
	}
	branchMarker := filepath.Join(branchHome, "synthetic-state.txt")
	if err := os.WriteFile(branchMarker, []byte("checkpoint-state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceMarker, []byte("source-after-branch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(branchMarker, []byte("branch-after-branch"), 0600); err != nil {
		t.Fatal(err)
	}
	branchEndpoint, stopBranch := startOverrideServer(t, ctx, executable, branchHome, branchNativeHome, mock.URL+"/v1")
	defer stopBranch()
	branched := connectOverrideServer(t, ctx, branchEndpoint)
	if err := branched.resumeThread(ctx, thread); err != nil {
		t.Fatalf("native Thread did not resume in independent home: %v", err)
	}
	branchTurns, err := branched.readTurns(ctx, thread)
	if err != nil || len(branchTurns) != 2 {
		t.Fatalf("independent home did not retain source Turns: %d / %v", len(branchTurns), err)
	}
	continuation, err := branched.startTurn(ctx, thread, branchHome, "synthetic-branch-continuation",
		core.HarnessInput{Text: "Continue the synthetic Thread."}, "synthetic-model", "low", "read-only")
	if err != nil {
		t.Fatal(err)
	}
	for !terminal(continuation.Status) {
		if err := branched.pollTurn(ctx, thread, continuation.ID, &continuation); err != nil {
			t.Fatal(err)
		}
	}
	branchTurns, err = branched.readTurns(ctx, thread)
	if err != nil || len(branchTurns) != 3 || continuation.Status != "completed" {
		t.Fatalf("independent native continuation: turns=%d status=%q err=%v", len(branchTurns), continuation.Status, err)
	}
	_ = branched.connection.CloseNow()
	sourceEndpoint, stopSource := startOverrideServer(t, ctx, executable, home, nativeHome, mock.URL+"/v1")
	defer stopSource()
	sourceAgain := connectOverrideServer(t, ctx, sourceEndpoint)
	sourceTurns, err := sourceAgain.readTurns(ctx, thread)
	_ = sourceAgain.connection.CloseNow()
	if err != nil || len(sourceTurns) != 2 {
		t.Fatalf("destination continuation changed source native history: %d / %v", len(sourceTurns), err)
	}
	if sourceBytes, err := os.ReadFile(sourceMarker); err != nil || string(sourceBytes) != "source-after-branch" {
		t.Fatalf("destination changed source workspace marker: %v", err)
	}
	if branchBytes, err := os.ReadFile(branchMarker); err != nil || string(branchBytes) != "branch-after-branch" {
		t.Fatalf("source changed destination workspace marker: %v", err)
	}
	contents, err := os.ReadFile(configPath)
	if err != nil || string(contents) != clientRouteConfig {
		t.Fatal("client configuration changed during inference")
	}
	t.Log("stock Codex completed native Turns through the scoped route, then resumed the same Thread in an independent home with divergent history and files")
}

const syntheticRouteResponse = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_synthetic","type":"message","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_synthetic","output_index":0,"content_index":0,"delta":"synthetic-route-reply"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_synthetic","type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic-route-reply","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_synthetic","status":"completed","usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":800},"output_tokens":100,"output_tokens_details":{"reasoning_tokens":40},"total_tokens":1300}}}

`
