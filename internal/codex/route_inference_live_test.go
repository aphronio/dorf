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
	"github.com/coder/websocket"
)

// Exercises stock Codex and the production launcher, not real Gateway routing
// or a model. The synthetic Responses stream deliberately contains no tool calls.
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
			Type  string `json:"type"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(payload, &request); err != nil || request.Type != "response.create" || request.Model != "synthetic-model" {
			t.Errorf("unexpected request type=%q model=%q decode=%v", request.Type, request.Model, err)
			return
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
	contents, err := os.ReadFile(configPath)
	if err != nil || string(contents) != clientRouteConfig {
		t.Fatal("client configuration changed during inference")
	}
	t.Log("stock Codex completed a turn through the installed route with the scoped credential; client configuration unchanged")
}

const syntheticRouteResponse = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_synthetic","type":"message","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_synthetic","output_index":0,"content_index":0,"delta":"synthetic-route-reply"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_synthetic","type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic-route-reply","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_synthetic","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}

`
