package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
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

func testProtocolServer(t *testing.T, respond func(string, map[string]any) (map[string]any, bool)) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	requests := make(chan map[string]any, 16)
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
			id, hasID := request["id"]
			if !hasID {
				continue
			}
			method, _ := request["method"].(string)
			params, _ := request["params"].(map[string]any)
			result, reject := respond(method, params)
			response := map[string]any{"id": id, "result": result}
			if reject {
				delete(response, "result")
				response["error"] = map[string]any{"code": -32000, "message": "test-only native detail must remain private"}
			}
			encoded, _ := json.Marshal(response)
			if err := conn.Write(ctx, websocket.MessageText, encoded); err != nil {
				return
			}
		}
	}))
	return server, requests
}

func dialTestProtocol(t *testing.T, server *httptest.Server) *protocol {
	t.Helper()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	p := &protocol{connection: conn}
	if err := p.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPersistedThreadRejectsTurnStartUntilResumeWithoutLeakingNativeDetail(t *testing.T) {
	server, requests := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
		if method == "initialize" {
			return map[string]any{}, false
		}
		return nil, method == "turn/start"
	})
	defer server.Close()
	p := dialTestProtocol(t, server)

	_, err := p.startTurn(context.Background(), "session-persisted", "/workspace/job", "agent-run-stable", "input", "gpt-5.6-sol", "high")
	if err == nil || err.Error() != "Codex app-server rejected turn/start" {
		t.Fatalf("safe rejection=%v", err)
	}
	if strings.Contains(err.Error(), "test-only") {
		t.Fatalf("native rejection detail escaped: %v", err)
	}
	for _, want := range []string{"initialize", "initialized", "turn/start"} {
		if got := (<-requests)["method"]; got != want {
			t.Fatalf("method=%v, want %s", got, want)
		}
	}
}

func TestPersistedThreadIsReadBeforeResumeAndSubmittedExactlyOnce(t *testing.T) {
	const sessionID = "session-persisted-1"
	var loaded atomic.Bool
	var acceptedStarts atomic.Int32
	server, requests := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/list":
			return map[string]any{"data": []any{map[string]any{"id": sessionID, "status": map[string]any{"type": "notLoaded"}}}}, false
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": sessionID, "status": map[string]any{"type": "notLoaded"}, "turns": []any{map[string]any{"id": "turn-prior", "status": "completed"}}}}, false
		case "thread/resume":
			if params["threadId"] != sessionID {
				return nil, true
			}
			loaded.Store(true)
			return map[string]any{"thread": map[string]any{"id": sessionID}}, false
		case "turn/start":
			if !loaded.Load() {
				return nil, true
			}
			acceptedStarts.Add(1)
			return map[string]any{"turn": map[string]any{"id": "turn-native-2"}}, false
		default:
			return nil, true
		}
	})
	defer server.Close()
	p := dialTestProtocol(t, server)

	threads, err := p.listThreads(context.Background(), "/workspace/job")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0] != sessionID {
		t.Fatalf("threads=%v", threads)
	}
	turns, err := p.readTurns(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0] != (TurnOutcome{ID: "turn-prior", Status: "completed"}) {
		t.Fatalf("turns=%#v", turns)
	}
	outcome, err := p.resumeAndStartTurn(context.Background(), sessionID, "/workspace/job", "agent-run-stable-2", "next input", "gpt-5.6-sol", "high")
	if err != nil {
		t.Fatal(err)
	}
	if outcome != (TurnOutcome{ID: "turn-native-2", Status: "running"}) || acceptedStarts.Load() != 1 {
		t.Fatalf("outcome=%#v accepted starts=%d", outcome, acceptedStarts.Load())
	}

	wantMethods := []string{"initialize", "initialized", "thread/list", "thread/read", "thread/resume", "turn/start"}
	for index, want := range wantMethods {
		request := <-requests
		if got := request["method"]; got != want {
			t.Fatalf("request %d method=%v, want %s", index, got, want)
		}
		if want == "thread/resume" || want == "turn/start" {
			params := request["params"].(map[string]any)
			if params["threadId"] != sessionID {
				t.Fatalf("%s threadId=%v", want, params["threadId"])
			}
		}
	}
}

func TestResumeRefusesSubstituteThreadBeforeTurnStart(t *testing.T) {
	server, requests := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/resume":
			return map[string]any{"thread": map[string]any{"id": "session-substitute"}}, false
		case "turn/start":
			t.Error("turn/start followed a mismatched thread/resume")
			return nil, true
		default:
			return nil, true
		}
	})
	defer server.Close()
	p := dialTestProtocol(t, server)

	_, err := p.resumeAndStartTurn(context.Background(), "session-bound", "/workspace/job", "agent-run-stable", "input", "gpt-5.6-sol", "high")
	if err == nil || err.Error() != "thread/resume did not return the exact bound native Session" {
		t.Fatalf("resume error=%v", err)
	}
	definite, ok := err.(interface{ DefiniteNoSubmit() bool })
	if !ok || !definite.DefiniteNoSubmit() {
		t.Fatalf("mismatched resume was not classified before-submit: %T %v", err, err)
	}
	for _, want := range []string{"initialize", "initialized", "thread/resume"} {
		if got := (<-requests)["method"]; got != want {
			t.Fatalf("method=%v, want %s", got, want)
		}
	}
	select {
	case request := <-requests:
		t.Fatalf("unexpected request after mismatched resume: %v", request["method"])
	default:
	}
}

func TestInitialRecoveryDropsLostEmptyThreadAndAdoptsAcceptedTurn(t *testing.T) {
	var threadStarts atomic.Int32
	var turnStarts atomic.Int32
	var persisted atomic.Bool
	var durableSession atomic.Value
	server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/list":
			if !persisted.Load() {
				return map[string]any{"data": []any{}}, false
			}
			return map[string]any{"data": []any{map[string]any{"id": durableSession.Load().(string), "status": map[string]any{"type": "notLoaded"}}}}, false
		case "thread/start":
			id := "session-empty-" + strconv.Itoa(int(threadStarts.Add(1)))
			return map[string]any{"thread": map[string]any{"id": id}}, false
		case "turn/start":
			turnStarts.Add(1)
			durableSession.Store(params["threadId"].(string))
			persisted.Store(true)
			return map[string]any{"turn": map[string]any{"id": "turn-native-1"}}, false
		case "thread/read":
			id := durableSession.Load().(string)
			return map[string]any{"thread": map[string]any{"id": id, "turns": []any{map[string]any{"id": "turn-native-1", "status": "inProgress"}}}}, false
		default:
			return nil, true
		}
	})
	defer server.Close()

	firstConnection := dialTestProtocol(t, server)
	lostSession, err := firstConnection.startThread(context.Background(), "/workspace/job", "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if err := firstConnection.connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	if lostSession != "session-empty-1" || persisted.Load() {
		t.Fatalf("empty thread unexpectedly durable: session=%s persisted=%v", lostSession, persisted.Load())
	}

	secondConnection := dialTestProtocol(t, server)
	sessionID, turn, err := secondConnection.reconcileInitialTurn(context.Background(), "/workspace/job", "agent-run-stable", "initial input", "gpt-5.6-sol", "high")
	if err != nil {
		t.Fatal(err)
	}
	if err := secondConnection.connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	if sessionID != "session-empty-2" || turn != (TurnOutcome{ID: "turn-native-1", Status: "running"}) {
		t.Fatalf("accepted binding session=%s turn=%#v", sessionID, turn)
	}

	thirdConnection := dialTestProtocol(t, server)
	// The fake app-server implements no clientUserMessageId deduplication. A
	// deliberately different hint still adopts by isolated Session history.
	recoveredSession, recoveredTurn, err := thirdConnection.reconcileInitialTurn(context.Background(), "/workspace/job", "different-native-hint", "initial input", "gpt-5.6-sol", "high")
	if err != nil {
		t.Fatal(err)
	}
	if recoveredSession != sessionID || recoveredTurn != (TurnOutcome{ID: "turn-native-1", Status: "inProgress"}) {
		t.Fatalf("recovered binding session=%s turn=%#v", recoveredSession, recoveredTurn)
	}
	if threadStarts.Load() != 2 || turnStarts.Load() != 1 {
		t.Fatalf("thread starts=%d turn starts=%d", threadStarts.Load(), turnStarts.Load())
	}
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
