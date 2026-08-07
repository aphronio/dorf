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

type probeRunner struct {
	result incus.Result
	calls  [][]string
	inputs [][]byte
}

func (r *probeRunner) Run(_ context.Context, command string, input []byte, args ...string) (incus.Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	r.inputs = append(r.inputs, append([]byte(nil), input...))
	return r.result, nil
}

func TestAppServerLaunchRetainsCapabilitySeparatelyAndExposesOnlyItsDigest(t *testing.T) {
	rawToken := "private-control-capability"
	digest := tokenSHA256(rawToken)
	launch := appServerScript("ws://10.0.0.2:4500", digest)
	if !strings.Contains(launch, `printf '%s\n' "$!" > `+serverPIDPath) {
		t.Fatalf("launch does not retain the native PID: %s", launch)
	}
	if !strings.Contains(launch, "nohup codex app-server ") {
		t.Fatalf("app-server is not detached from the executor: %s", launch)
	}
	if !strings.Contains(launch, "rm -f "+serverPIDPath) {
		t.Fatalf("dead-process launch cannot replace stale PID state: %s", launch)
	}
	if !strings.Contains(launch, "--ws-token-sha256 "+digest) || strings.Contains(launch, rawToken) {
		t.Fatalf("app-server argv does not contain only the capability digest: %s", launch)
	}
	if strings.Contains(launch, "--ws-token-file") || strings.Contains(launch, controlTokenPath) {
		t.Fatalf("retained reconnect capability was confused with one-shot startup input: %s", launch)
	}
	store := controlCapabilityScript()
	if !strings.Contains(store, "install -d -m 700 "+serverControlDir) || !strings.Contains(store, controlTokenPath+".new") || !strings.Contains(store, "chmod 600 "+controlTokenPath+".new") || !strings.Contains(store, "mv -f "+controlTokenPath+".new "+controlTokenPath) {
		t.Fatalf("control capability is not atomically retained root-only: %s", store)
	}
}

func TestLiveServerProbeReadsOnlyRetainedCapabilityForExactTrackedProcess(t *testing.T) {
	runner := &probeRunner{result: incus.Result{Stdout: "1\n1\nprivate-capability\n"}}
	agent := Agent{Sandbox: incus.Sandbox{Runner: runner}}
	endpoint := "ws://10.0.0.2:4500"
	probe, err := agent.probeServer(context.Background(), "sandbox-1", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !probe.running || !probe.tracked || probe.token != "private-capability" {
		t.Fatalf("probe=%#v", probe)
	}
	command := strings.Join(runner.calls[0], " ")
	for _, required := range []string{serverPIDPath, controlTokenPath, endpoint, "--ws-auth capability-token", "--ws-token-sha256 $digest", "sha256sum", "/proc/$pid/cmdline"} {
		if !strings.Contains(command, required) {
			t.Fatalf("probe command is missing %q: %s", required, command)
		}
	}
	if strings.Contains(command, "/tmp/dorf/codex-app-server.token") {
		t.Fatalf("probe still reads the consumed startup token input: %s", command)
	}
}

func TestLiveExactServerReconnectUsesRetainedCapability(t *testing.T) {
	const token = "retained-control-capability"
	server := testAppServer(t, token, false)
	defer server.Close()
	runner := &probeRunner{result: incus.Result{Stdout: "1\n1\n" + token + "\n"}}
	agent := Agent{Sandbox: incus.Sandbox{Runner: runner}}
	called := false
	if err := agent.withServerEndpoint(context.Background(), "sandbox-1", "ws"+strings.TrimPrefix(server.URL, "http"), func(_ *protocol) error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called || len(runner.calls) != 1 {
		t.Fatalf("reconnect called=%v guest calls=%d", called, len(runner.calls))
	}
}

func TestLiveServerMissingOrRejectedCapabilityStopsWithoutReplacement(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		runner := &probeRunner{result: incus.Result{Stdout: "1\n1\n"}}
		agent := Agent{Sandbox: incus.Sandbox{Runner: runner}}
		err := agent.withServerEndpoint(context.Background(), "sandbox-1", "ws://127.0.0.1:1", func(_ *protocol) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "no recoverable scoped capability") {
			t.Fatalf("missing capability error=%v", err)
		}
		assertNoServerReplacement(t, runner)
	})
	t.Run("rejected", func(t *testing.T) {
		const token = "rejected-control-capability"
		server := testAppServer(t, token, true)
		defer server.Close()
		runner := &probeRunner{result: incus.Result{Stdout: "1\n1\n" + token + "\n"}}
		agent := Agent{Sandbox: incus.Sandbox{Runner: runner}}
		err := agent.withServerEndpoint(context.Background(), "sandbox-1", "ws"+strings.TrimPrefix(server.URL, "http"), func(_ *protocol) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "could not be authenticated or inspected") || !strings.Contains(err.Error(), "rejected its scoped control capability") {
			t.Fatalf("rejected capability error=%v", err)
		}
		assertNoServerReplacement(t, runner)
	})
}

func assertNoServerReplacement(t *testing.T, runner *probeRunner) {
	t.Helper()
	for index, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "nohup codex app-server") || len(runner.inputs[index]) > 0 {
			t.Fatalf("live server was replaced: call=%v input-bytes=%d", call, len(runner.inputs[index]))
		}
	}
}

func testAppServer(t *testing.T, token string, reject bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
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
			id, hasID := request["id"]
			if !hasID {
				continue
			}
			response, _ := json.Marshal(map[string]any{"id": id, "result": map[string]any{}})
			if err := conn.Write(ctx, websocket.MessageText, response); err != nil {
				return
			}
		}
	}))
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
