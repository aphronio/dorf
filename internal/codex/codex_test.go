package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/coder/websocket"
)

func testSandbox(runner incustest.Runner, owner provider.Ownership) incus.Adapter {
	return incus.Adapter{Sandbox: incustest.OwnedSandbox(runner, incus.Config{}, owner)}
}

func testReviewSandbox(runner incustest.Runner) incus.Adapter {
	return incus.Adapter{Sandbox: incustest.Sandbox(runner, incus.Config{})}
}

func testOwner(sandboxID string) provider.Ownership {
	return provider.Ownership{JobID: "job-" + sandboxID, SandboxID: sandboxID, OwnershipNonce: strings.Repeat("a", 64)}
}

func reviewOwner(sandboxID string, review provider.ReviewMetadata) provider.Ownership {
	return provider.Ownership{JobID: review.JobID, SandboxID: sandboxID, OwnershipNonce: review.OwnershipNonce}
}

type probeRunner struct {
	result incus.Result
	calls  [][]string
	inputs [][]byte
}

type reviewBoundaryRunner struct {
	name    string
	review  provider.ReviewMetadata
	token   string
	stopped bool
	attests int
	calls   [][]string
	inputs  [][]byte
}

func (r *reviewBoundaryRunner) Run(_ context.Context, command string, input []byte, args ...string) (incus.Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	r.inputs = append(r.inputs, append([]byte(nil), input...))
	joined := strings.Join(args, " ")
	if strings.HasPrefix(joined, "list --format=json") {
		r.attests++
		config := map[string]string{
			"user.dorf.owner": "sandbox", "user.dorf.job": r.review.JobID, "user.dorf.sandbox": r.name,
			"user.dorf.agent_run": r.review.AgentRunID, "user.dorf.revision": r.review.Revision,
			"user.dorf.ownership_nonce": r.review.OwnershipNonce,
		}
		payload, _ := json.Marshal([]map[string]any{{"name": r.name, "config": config}})
		return incus.Result{Stdout: string(payload)}, nil
	}
	if strings.Contains(joined, "running=0; tracked=0") {
		if r.stopped {
			return incus.Result{Stdout: "0\n0\n"}, nil
		}
		return incus.Result{Stdout: "1\n1\n" + r.token + "\n"}, nil
	}
	return incus.Result{}, nil
}

func (r *probeRunner) Run(_ context.Context, command string, input []byte, args ...string) (incus.Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	r.inputs = append(r.inputs, append([]byte(nil), input...))
	return r.result, nil
}

func TestCodexCommandBoundaryKeepsFixedPolicyAndScopedCapability(t *testing.T) {
	const token = "private-control-capability"
	digest := tokenSHA256(token)
	implementation := appServerScript("ws://10.0.0.2:4500", digest, false)
	review := appServerScript("ws://10.0.0.3:4500", digest, true)
	if !strings.Contains(implementation, `-c 'approval_policy="never"'`) || strings.Contains(implementation, `sandbox_mode=`) {
		t.Fatalf("implementation launch policy = %s", implementation)
	}
	if !strings.Contains(review, `-c 'approval_policy="never"' -c 'sandbox_mode="read-only"'`) {
		t.Fatalf("review launch policy = %s", review)
	}
	for _, launch := range []string{implementation, review} {
		if !strings.Contains(launch, "--ws-auth capability-token --ws-token-sha256 "+digest) || strings.Contains(launch, token) {
			t.Fatalf("launch did not use digest-only websocket authentication: %s", launch)
		}
		if !strings.Contains(launch, "nohup codex app-server") || !strings.Contains(launch, "rm -f "+serverPIDPath) || !strings.Contains(launch, `printf '%s\n' "$!" > `+serverPIDPath) {
			t.Fatalf("launch did not detach and retain the exact process ID: %s", launch)
		}
		if strings.Contains(launch, controlTokenPath) {
			t.Fatalf("launch argv references the retained control-token path: %s", launch)
		}
	}
	custody := controlCapabilityScript()
	for _, want := range []string{"install -d -m 700 " + serverControlDir, controlTokenPath + ".new", "chmod 600 " + controlTokenPath + ".new", "mv -f " + controlTokenPath + ".new " + controlTokenPath} {
		if !strings.Contains(custody, want) {
			t.Fatalf("capability custody missing %q: %s", want, custody)
		}
	}
	probe := probeServerScript("ws://10.0.0.3:4500")
	for _, want := range []string{serverPIDPath, "/proc/$pid/cmdline", "--listen ws://10.0.0.3:4500", "--ws-auth capability-token", "sha256sum", "--ws-token-sha256 $digest"} {
		if !strings.Contains(probe, want) {
			t.Fatalf("exact process probe missing %q: %s", want, probe)
		}
	}

	runner := &probeRunner{}
	owner := testOwner("dorf-job")
	agent := Agent{Sandbox: testSandbox(runner, owner)}
	if err := agent.InstallRoute(context.Background(), owner, "http://10.42.0.1:8317/v1", "scoped-key", "unused"); err != nil {
		t.Fatal(err)
	}
	if len(runner.inputs) != 1 || !strings.Contains(string(runner.inputs[0]), "supports_websockets = true") || !strings.HasSuffix(string(runner.inputs[0]), "scoped-key\n") {
		t.Fatalf("InstallRoute input = %q", runner.inputs)
	}
}

func TestLiveExactServerReconnectUsesRetainedCapability(t *testing.T) {
	const token = "retained-control-capability"
	server := testAppServer(t, token, false)
	defer server.Close()
	runner := &probeRunner{result: incus.Result{Stdout: "1\n1\n" + token + "\n"}}
	agent := Agent{Sandbox: testSandbox(runner, testOwner("sandbox-1"))}
	called := false
	if err := agent.withServerEndpoint(context.Background(), testOwner("sandbox-1"), "ws"+strings.TrimPrefix(server.URL, "http"), func(_ *protocol) error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called || len(runner.calls) != 1 {
		t.Fatalf("reconnect called=%v guest calls=%d", called, len(runner.calls))
	}
}

func TestRemoteEndpointSeparatesGuestBindFromAuthenticatedDial(t *testing.T) {
	const token = "retained-control-capability"
	const trafficToken = "provider-traffic-capability"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("e2b-traffic-access-token") != trafficToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		kind, payload, err := conn.Read(r.Context())
		if err != nil || kind != websocket.MessageText {
			t.Errorf("read initialize: kind=%v error=%v", kind, err)
			return
		}
		var request map[string]any
		if err := json.Unmarshal(payload, &request); err != nil {
			t.Error(err)
			return
		}
		response, _ := json.Marshal(map[string]any{"id": request["id"], "result": map[string]any{}})
		if err := conn.Write(r.Context(), websocket.MessageText, response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	runner := &probeRunner{result: incus.Result{Stdout: "1\n1\n" + token + "\n"}}
	agent := Agent{Sandbox: testSandbox(runner, testOwner("sandbox-1"))}
	headers := http.Header{"e2b-traffic-access-token": []string{trafficToken}}
	endpoint := endpointAccess{
		listen:  "ws://0.0.0.0:4500",
		dial:    "ws" + strings.TrimPrefix(server.URL, "http"),
		headers: headers,
	}
	if err := agent.withServerEndpointController(context.Background(), testOwner("sandbox-1"), endpoint, false, nil, func(_ *protocol) error { return nil }); err != nil {
		t.Fatal(err)
	}
	probeCommand := strings.Join(runner.calls[0], " ")
	if !strings.Contains(probeCommand, endpoint.listen) || strings.Contains(probeCommand, endpoint.dial) {
		t.Fatalf("probe did not use only the guest bind endpoint: %s", probeCommand)
	}
	if headers.Get("Authorization") != "" {
		t.Fatal("dial mutated the provider-owned header set")
	}
}

func TestProtocolUsesFreshProviderStreamForEveryDialAttempt(t *testing.T) {
	const token = "retained-control-capability"
	server := testAppServer(t, token, false)
	defer server.Close()
	serverAddress := strings.TrimPrefix(server.URL, "http://")

	var calls atomic.Int32
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		calls.Add(1)
		if network != "tcp" || address != "incus.invalid:4500" {
			t.Fatalf("dial target = %s %s", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", serverAddress)
	}
	for range 2 {
		protocol, err := dialProtocol(context.Background(), "ws://incus.invalid:4500", token, nil, dial)
		if err != nil {
			t.Fatal(err)
		}
		_ = protocol.connection.CloseNow()
	}
	if calls.Load() != 2 {
		t.Fatalf("provider streams=%d, want one fresh stream per attempt", calls.Load())
	}
}

func TestProtocolProviderDialHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := dialProtocol(ctx, "ws://incus.invalid:4500", "capability", nil, dial)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error=%v, want canceled context", err)
	}
}

func TestLiveServerMissingOrRejectedCapabilityStopsWithoutReplacement(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		runner := &probeRunner{result: incus.Result{Stdout: "1\n1\n"}}
		agent := Agent{Sandbox: testSandbox(runner, testOwner("sandbox-1"))}
		err := agent.withServerEndpoint(context.Background(), testOwner("sandbox-1"), "ws://127.0.0.1:1", func(_ *protocol) error { return nil })
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
		agent := Agent{Sandbox: testSandbox(runner, testOwner("sandbox-1"))}
		err := agent.withServerEndpoint(context.Background(), testOwner("sandbox-1"), "ws"+strings.TrimPrefix(server.URL, "http"), func(_ *protocol) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "could not be authenticated or inspected") || !strings.Contains(err.Error(), "rejected its scoped control capability") {
			t.Fatalf("rejected capability error=%v", err)
		}
		assertNoServerReplacement(t, runner)
	})
}

func TestStrictReviewRejectsForeignOwnerAndReattestsAfterCapabilityRotation(t *testing.T) {
	const token = "retained-review-capability"
	review := provider.ReviewMetadata{
		JobID: "job-review", AgentRunID: "run-review", Revision: strings.Repeat("b", 40),
		OwnershipNonce: strings.Repeat("c", 64),
	}
	runner := &reviewBoundaryRunner{name: "dorf-review-owned", review: review, token: token}
	agent := Agent{Sandbox: testReviewSandbox(runner)}
	foreign := reviewOwner(runner.name, review)
	foreign.OwnershipNonce = strings.Repeat("d", 64)
	if _, err := agent.StartStrictReviewTurn(context.Background(), foreign, "/workspace/job", review, strings.Repeat("a", 64), "input", "gpt-5.6-sol", "high"); err == nil {
		t.Fatal("foreign owner reached the strict-review controller")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("foreign owner reached Incus: %v", runner.calls)
	}

	server, _ := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
		return map[string]any{}, method != "initialize"
	})
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	owner := reviewOwner(runner.name, review)
	connect := func() {
		if err := agent.withReviewServerEndpoint(context.Background(), owner, endpoint, review, func(*protocol) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	connect()
	runner.stopped = true
	connect()
	if runner.attests != 6 {
		t.Fatalf("review ownership attestations=%d, want one before every Incus execution", runner.attests)
	}
	rotated := false
	for index, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, " exec ") && !strings.Contains(joined, "exec "+runner.name+" --") {
			t.Fatalf("strict review escaped its review Sandbox: %v", call)
		}
		if len(runner.inputs[index]) > 0 && strings.TrimSpace(string(runner.inputs[index])) != token {
			rotated = true
		}
	}
	if !rotated {
		t.Fatal("replacement process did not receive a rotated capability")
	}
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

func requireProtocolParams(t *testing.T, method string, params map[string]any, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if !reflect.DeepEqual(params[key], value) {
			t.Errorf("%s %s=%#v, want %#v", method, key, params[key], value)
		}
	}
}

func requireStrictResumeParams(t *testing.T, params map[string]any, sessionID string) {
	t.Helper()
	requireProtocolParams(t, "thread/resume", params, map[string]any{
		"threadId": sessionID, "cwd": "/workspace/job",
		"approvalPolicy": "never", "sandbox": "read-only",
	})
	if len(params) != 4 {
		t.Fatalf("strict thread/resume sent extra parameters: %#v", params)
	}
}

func TestProtocolBindsResumeStartAndSteerToExactIdentity(t *testing.T) {
	const sessionID = "session-bound"
	const turnID = "turn-bound"
	const messageID = "agent-run-bound"

	t.Run("resume and start", func(t *testing.T) {
		server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
			switch method {
			case "initialize":
				return map[string]any{}, false
			case "thread/resume":
				requireProtocolParams(t, method, params, map[string]any{"threadId": sessionID})
				return map[string]any{"thread": map[string]any{"id": sessionID}}, false
			case "turn/start":
				requireProtocolParams(t, method, params, map[string]any{
					"threadId": sessionID, "clientUserMessageId": messageID, "cwd": "/workspace/job",
					"model": "gpt-5.6-sol", "effort": "high", "approvalPolicy": "never",
					"sandboxPolicy": map[string]any{"type": "dangerFullAccess"},
				})
				return map[string]any{"turn": map[string]any{"id": turnID}}, false
			default:
				return nil, true
			}
		})
		defer server.Close()
		outcome, err := dialTestProtocol(t, server).resumeAndStartTurn(context.Background(), sessionID, "/workspace/job", messageID, "input", "gpt-5.6-sol", "high", "danger-full-access")
		if err != nil || outcome.ID != turnID {
			t.Fatalf("resume and start outcome=%#v err=%v", outcome, err)
		}
	})

	t.Run("steer", func(t *testing.T) {
		server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
			switch method {
			case "initialize":
				return map[string]any{}, false
			case "thread/resume":
				requireProtocolParams(t, method, params, map[string]any{"threadId": sessionID})
				return map[string]any{"thread": map[string]any{"id": sessionID}}, false
			case "turn/steer":
				requireProtocolParams(t, method, params, map[string]any{"threadId": sessionID, "expectedTurnId": turnID, "clientUserMessageId": messageID})
				return map[string]any{"turnId": turnID}, false
			default:
				return nil, true
			}
		})
		defer server.Close()
		accepted, err := dialTestProtocol(t, server).steerTurn(context.Background(), sessionID, turnID, messageID, "correction")
		if err != nil || accepted != turnID {
			t.Fatalf("steer accepted=%q err=%v", accepted, err)
		}
	})

	t.Run("substitute resume", func(t *testing.T) {
		server, _ := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
			switch method {
			case "initialize":
				return map[string]any{}, false
			case "thread/resume":
				return map[string]any{"thread": map[string]any{"id": "session-substitute"}}, false
			case "turn/start":
				t.Fatal("turn/start followed a substitute resume")
			}
			return nil, true
		})
		defer server.Close()
		_, err := dialTestProtocol(t, server).resumeAndStartTurn(context.Background(), sessionID, "/workspace/job", messageID, "input", "gpt-5.6-sol", "high", "danger-full-access")
		var definite interface{ DefiniteNoSubmit() bool }
		if !errors.As(err, &definite) || !definite.DefiniteNoSubmit() {
			t.Fatalf("substitute resume error=%T %v", err, err)
		}
	})
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
	lostSession, err := firstConnection.startThread(context.Background(), "/workspace/job", "gpt-5.6-sol", "danger-full-access")
	if err != nil {
		t.Fatal(err)
	}
	if err := firstConnection.connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	if lostSession != "session-empty-1" || persisted.Load() {
		t.Fatalf("empty thread unexpectedly durable: thread=%s persisted=%v", lostSession, persisted.Load())
	}

	inspectionConnection := dialTestProtocol(t, server)
	inspectedSession, inspectedTurns, err := inspectionConnection.inspectInitialTurns(context.Background(), "/workspace/job")
	if err != nil {
		t.Fatal(err)
	}
	if err := inspectionConnection.connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	if inspectedSession != "" || len(inspectedTurns) != 0 || threadStarts.Load() != 1 || turnStarts.Load() != 0 {
		t.Fatalf("read-only initial inspection thread=%s turns=%#v thread starts=%d turn starts=%d", inspectedSession, inspectedTurns, threadStarts.Load(), turnStarts.Load())
	}

	secondConnection := dialTestProtocol(t, server)
	sessionID, turn, err := secondConnection.reconcileInitialTurn(context.Background(), "/workspace/job", "agent-run-stable", "initial input", "gpt-5.6-sol", "high", "danger-full-access")
	if err != nil {
		t.Fatal(err)
	}
	if err := secondConnection.connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	if sessionID != "session-empty-2" || !reflect.DeepEqual(turn, TurnOutcome{ID: "turn-native-1", Status: "running"}) {
		t.Fatalf("accepted binding thread=%s turn=%#v", sessionID, turn)
	}

	thirdConnection := dialTestProtocol(t, server)
	// The fake app-server implements no clientUserMessageId deduplication. A
	// deliberately different hint still adopts by isolated thread history.
	recoveredSession, recoveredTurn, err := thirdConnection.reconcileInitialTurn(context.Background(), "/workspace/job", "different-native-hint", "initial input", "gpt-5.6-sol", "high", "danger-full-access")
	if err != nil {
		t.Fatal(err)
	}
	if recoveredSession != sessionID || !reflect.DeepEqual(recoveredTurn, TurnOutcome{ID: "turn-native-1", Status: "inProgress"}) {
		t.Fatalf("recovered binding thread=%s turn=%#v", recoveredSession, recoveredTurn)
	}
	if threadStarts.Load() != 2 || turnStarts.Load() != 1 {
		t.Fatalf("thread starts=%d turn starts=%d", threadStarts.Load(), turnStarts.Load())
	}
}

func TestStrictReviewRecoveryRejectsUnattestedNativeState(t *testing.T) {
	nonce := strings.Repeat("a", 64)
	input := "bounded exact review contract"
	validTurn := strictReviewTestTurn("turn-review", nonce, input)
	for _, test := range []struct {
		name    string
		threads []any
		turns   []any
		mutate  func(map[string]any)
		cursor  any
		want    string
	}{
		{name: "missing client identity", threads: strictReviewTestThreads("session-review"), turns: []any{strictReviewTestTurn("turn-review", "", input)}, want: "missing or wrong client message identity"},
		{name: "wrong client identity", threads: strictReviewTestThreads("session-review"), turns: []any{strictReviewTestTurn("turn-review", strings.Repeat("b", 64), input)}, want: "missing or wrong client message identity"},
		{name: "wrong prompt", threads: strictReviewTestThreads("session-review"), turns: []any{strictReviewTestTurn("turn-review", nonce, "forged prompt")}, want: "prompt differs"},
		{name: "extra turn", threads: strictReviewTestThreads("session-review"), turns: []any{validTurn, strictReviewTestTurn("turn-extra", nonce, input)}, want: "contains 2 turns"},
		{name: "competing thread", threads: append(strictReviewTestThreads("session-review"), strictReviewTestThreads("session-forged")...), turns: []any{validTurn}, want: "competing threads"},
		{name: "unbounded discovery", threads: strictReviewTestThreads("session-review"), turns: []any{validTurn}, cursor: "more", want: "exceeded its bound"},
		{name: "wrong model", threads: strictReviewTestThreads("session-review"), turns: []any{validTurn}, mutate: func(result map[string]any) { result["model"] = "forged-model" }, want: "model, effort, cwd, approval, or read-only policy"},
		{name: "null effort", threads: strictReviewTestThreads("session-review"), turns: []any{validTurn}, mutate: func(result map[string]any) { result["reasoningEffort"] = nil }, want: "model, effort, cwd, approval, or read-only policy"},
		{name: "different effort", threads: strictReviewTestThreads("session-review"), turns: []any{validTurn}, mutate: func(result map[string]any) { result["reasoningEffort"] = "medium" }, want: "model, effort, cwd, approval, or read-only policy"},
		{name: "wrong approval", threads: strictReviewTestThreads("session-review"), turns: []any{validTurn}, mutate: func(result map[string]any) { result["approvalPolicy"] = "on-request" }, want: "model, effort, cwd, approval, or read-only policy"},
		{name: "wrong sandbox", threads: strictReviewTestThreads("session-review"), turns: []any{validTurn}, mutate: func(result map[string]any) { result["sandbox"] = map[string]any{"type": "dangerFullAccess"} }, want: "model, effort, cwd, approval, or read-only policy"},
		{name: "network enabled", threads: strictReviewTestThreads("session-review"), turns: []any{validTurn}, mutate: func(result map[string]any) {
			result["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": true}
		}, want: "unexpectedly exposes network access"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
				switch method {
				case "initialize":
					return map[string]any{}, false
				case "thread/list":
					return map[string]any{"data": test.threads, "nextCursor": test.cursor}, false
				case "thread/resume":
					requireStrictResumeParams(t, params, "session-review")
					result := strictReviewTestSettings(map[string]any{"id": "session-review", "cwd": "/workspace/job", "turns": test.turns})
					if test.mutate != nil {
						test.mutate(result)
					}
					return result, false
				default:
					return nil, true
				}
			})
			defer server.Close()
			p := dialTestProtocol(t, server)
			_, _, err := p.strictReviewHistory(context.Background(), "/workspace/job", "session-review", nonce, input, "gpt-5.6-sol", "high")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("strict review error=%v want %q", err, test.want)
			}
			if attention, ok := err.(interface{ AttentionNeeded() bool }); !ok || !attention.AttentionNeeded() {
				t.Fatalf("strict review mismatch was not attention: %T %v", err, err)
			}
		})
	}
}

func TestStrictReviewTrustedSubmissionConvergesOnOneSessionAndTurn(t *testing.T) {
	nonce := strings.Repeat("c", 64)
	input := "exact trusted review input"
	sessionID, turnID := "session-review", "turn-review"
	var sessionCreated, turnCreated bool
	var turnStarts atomic.Int32
	server, requests := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/list":
			if !sessionCreated {
				return map[string]any{"data": []any{}, "nextCursor": nil}, false
			}
			return map[string]any{"data": strictReviewTestThreads(sessionID), "nextCursor": nil}, false
		case "thread/start":
			sessionCreated = true
			requireProtocolParams(t, method, params, map[string]any{
				"cwd": "/workspace/job", "model": "gpt-5.6-sol",
				"approvalPolicy": "never", "sandbox": "read-only",
			})
			if _, sent := params["effort"]; sent {
				t.Errorf("thread/start sent unsupported turn effort: %v", params["effort"])
			}
			result := strictReviewTestSettings(map[string]any{"id": sessionID, "cwd": "/workspace/job", "turns": []any{}})
			result["reasoningEffort"] = nil
			return result, false
		case "thread/resume":
			if !turnCreated {
				t.Error("thread/resume attempted before the fresh review turn was submitted")
				return nil, true
			}
			requireStrictResumeParams(t, params, sessionID)
			return strictReviewTestSettings(map[string]any{"id": sessionID, "cwd": "/workspace/job", "turns": []any{strictReviewTestTurn(turnID, nonce, input)}}), false
		case "turn/start":
			requireProtocolParams(t, method, params, map[string]any{
				"threadId": sessionID, "clientUserMessageId": nonce, "cwd": "/workspace/job",
				"model": "gpt-5.6-sol", "effort": "high", "approvalPolicy": "never",
				"sandboxPolicy": map[string]any{"type": "readOnly"},
			})
			turnStarts.Add(1)
			turnCreated = true
			return map[string]any{"turn": map[string]any{"id": turnID}}, false
		default:
			return nil, true
		}
	})
	defer server.Close()

	first := dialTestProtocol(t, server)
	gotSession, gotTurn, err := first.reconcileStrictReviewTurn(context.Background(), "/workspace/job", "", nonce, input, "gpt-5.6-sol", "high", true)
	if err != nil || gotSession != sessionID || gotTurn.ID != turnID {
		t.Fatalf("first strict binding thread=%s turn=%#v err=%v", gotSession, gotTurn, err)
	}
	if methods := reviewProtocolMethods(requests); !reflect.DeepEqual(methods, []string{"thread/list", "thread/start", "turn/start"}) {
		t.Fatalf("fresh strict review methods=%v", methods)
	}
	_ = first.connection.CloseNow()
	second := dialTestProtocol(t, server)
	gotSession, gotTurn, err = second.reconcileStrictReviewTurn(context.Background(), "/workspace/job", "", nonce, input, "gpt-5.6-sol", "high", true)
	if err != nil || gotSession != sessionID || gotTurn.ID != turnID || gotTurn.Output != `{"material":false}` || turnStarts.Load() != 1 {
		t.Fatalf("recovered strict binding thread=%s turn=%#v starts=%d err=%v", gotSession, gotTurn, turnStarts.Load(), err)
	}
	if methods := reviewProtocolMethods(requests); !reflect.DeepEqual(methods, []string{"thread/list", "thread/resume"}) {
		t.Fatalf("persisted strict review methods=%v", methods)
	}
}

func TestStrictReviewLostAfterTrustedSubmissionAdoptsPersistedTurnWithoutDuplicate(t *testing.T) {
	nonce := strings.Repeat("d", 64)
	input := "exact retry input"
	sessionID, turnID := "session-lost-response", "turn-lost-response"
	var sessionCreated, turnCreated bool
	var turnStarts atomic.Int32
	server, requests := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/list":
			if !turnCreated {
				return map[string]any{"data": []any{}, "nextCursor": nil}, false
			}
			return map[string]any{"data": strictReviewTestThreads(sessionID), "nextCursor": nil}, false
		case "thread/start":
			sessionCreated = true
			result := strictReviewTestSettings(map[string]any{"id": sessionID, "cwd": "/workspace/job", "turns": []any{}})
			result["reasoningEffort"] = nil
			return result, false
		case "turn/start":
			if !sessionCreated || params["clientUserMessageId"] != nonce || params["threadId"] != sessionID {
				return nil, true
			}
			turnStarts.Add(1)
			turnCreated = true
			return map[string]any{"turn": map[string]any{"id": turnID}}, false
		case "thread/resume":
			if !turnCreated {
				t.Error("retry resumed an empty strict review thread")
				return nil, true
			}
			return strictReviewTestSettings(map[string]any{"id": sessionID, "cwd": "/workspace/job", "turns": []any{strictReviewTestTurn(turnID, nonce, input)}}), false
		default:
			return nil, true
		}
	})
	defer server.Close()

	first := dialTestProtocol(t, server)
	threads, err := first.listStrictReviewThreads(context.Background(), "/workspace/job")
	if err != nil || len(threads) != 0 {
		t.Fatalf("initial discovery=%v err=%v", threads, err)
	}
	startedSession, err := first.startStrictReviewThread(context.Background(), "/workspace/job", "gpt-5.6-sol", "high")
	if err != nil || startedSession != sessionID {
		t.Fatalf("fresh thread=%s err=%v", startedSession, err)
	}
	if _, err := first.startTurn(context.Background(), sessionID, "/workspace/job", nonce, input, "gpt-5.6-sol", "high", "read-only"); err != nil {
		t.Fatal(err)
	}
	_ = first.connection.CloseNow() // controller response is lost before strict readback/binding
	if methods := reviewProtocolMethods(requests); !reflect.DeepEqual(methods, []string{"thread/list", "thread/start", "turn/start"}) {
		t.Fatalf("pre-loss methods=%v", methods)
	}

	retry := dialTestProtocol(t, server)
	gotSession, gotTurn, err := retry.reconcileStrictReviewTurn(context.Background(), "/workspace/job", "", nonce, input, "gpt-5.6-sol", "high", false)
	if err != nil || gotSession != sessionID || gotTurn.ID != turnID || turnStarts.Load() != 1 {
		t.Fatalf("retry binding thread=%s turn=%#v starts=%d err=%v", gotSession, gotTurn, turnStarts.Load(), err)
	}
	if methods := reviewProtocolMethods(requests); !reflect.DeepEqual(methods, []string{"thread/list", "thread/resume"}) {
		t.Fatalf("retry methods=%v", methods)
	}
}

func TestStrictReviewDirectBindingToleratesDelayedNativeVisibility(t *testing.T) {
	nonce := strings.Repeat("e", 64)
	input := "exact delayed visibility input"
	sessionID, turnID := "session-delayed", "turn-delayed"
	var sessionCreated, turnCreated bool
	var postSubmitLists atomic.Int32
	var threadStarts, turnStarts atomic.Int32
	server, requests := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/list":
			if !sessionCreated {
				return map[string]any{"data": []any{}, "nextCursor": nil}, false
			}
			if turnCreated && postSubmitLists.Add(1) == 1 {
				return map[string]any{"data": []any{}, "nextCursor": nil}, false
			}
			return map[string]any{"data": strictReviewTestThreads(sessionID), "nextCursor": nil}, false
		case "thread/start":
			threadStarts.Add(1)
			sessionCreated = true
			result := strictReviewTestSettings(map[string]any{"id": sessionID, "cwd": "/workspace/job", "turns": []any{}})
			result["reasoningEffort"] = nil
			return result, false
		case "turn/start":
			turnStarts.Add(1)
			turnCreated = true
			return map[string]any{"turn": map[string]any{"id": turnID, "status": "running"}}, false
		case "thread/resume":
			return strictReviewTestSettings(map[string]any{"id": sessionID, "cwd": "/workspace/job", "turns": []any{strictReviewTestTurn(turnID, nonce, input)}}), false
		default:
			return nil, true
		}
	})
	defer server.Close()

	p := dialTestProtocol(t, server)
	gotSession, gotTurn, err := p.reconcileStrictReviewTurn(context.Background(), "/workspace/job", "", nonce, input, "gpt-5.6-sol", "high", true)
	if err != nil || gotSession != sessionID || gotTurn.ID != turnID {
		t.Fatalf("direct binding thread=%s turn=%#v err=%v", gotSession, gotTurn, err)
	}
	if methods := reviewProtocolMethods(requests); !reflect.DeepEqual(methods, []string{"thread/list", "thread/start", "turn/start"}) {
		t.Fatalf("direct binding waited for readback: %v", methods)
	}
	if _, _, err := p.strictReviewHistory(context.Background(), "/workspace/job", sessionID, nonce, input, "gpt-5.6-sol", "high"); err == nil || !isRetryableReviewVisibility(err) {
		t.Fatalf("first delayed discovery error=%T %v", err, err)
	}
	observedSession, turns, err := p.strictReviewHistory(context.Background(), "/workspace/job", sessionID, nonce, input, "gpt-5.6-sol", "high")
	if err != nil || observedSession != sessionID || len(turns) != 1 || turns[0].ID != turnID || threadStarts.Load() != 1 || turnStarts.Load() != 1 {
		t.Fatalf("converged thread=%s turns=%#v starts=%d/%d err=%v", observedSession, turns, threadStarts.Load(), turnStarts.Load(), err)
	}
}

func TestStrictReviewReconciliationOnlyEmptyDiscoveryNeverSubmits(t *testing.T) {
	nonce := strings.Repeat("f", 64)
	server, requests := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/list":
			return map[string]any{"data": []any{}, "nextCursor": nil}, false
		default:
			return nil, true
		}
	})
	defer server.Close()

	p := dialTestProtocol(t, server)
	_, _, err := p.reconcileStrictReviewTurn(context.Background(), "/workspace/job", "", nonce, "exact input", "gpt-5.6-sol", "high", false)
	if err == nil || !isRetryableReviewVisibility(err) {
		t.Fatalf("empty reconciliation error=%T %v", err, err)
	}
	if methods := reviewProtocolMethods(requests); !reflect.DeepEqual(methods, []string{"thread/list"}) {
		t.Fatalf("reconciliation-only methods=%v", methods)
	}
}

func isRetryableReviewVisibility(err error) bool {
	var missing interface{ RetryableReviewVisibility() bool }
	return errors.As(err, &missing) && missing.RetryableReviewVisibility()
}

func reviewProtocolMethods(requests <-chan map[string]any) []string {
	var methods []string
	for {
		select {
		case request := <-requests:
			method, _ := request["method"].(string)
			if method != "initialize" && method != "initialized" {
				methods = append(methods, method)
			}
		default:
			return methods
		}
	}
}

func strictReviewTestThreads(ids ...string) []any {
	threads := make([]any, 0, len(ids))
	for _, id := range ids {
		threads = append(threads, map[string]any{"id": id, "cwd": "/workspace/job"})
	}
	return threads
}

func strictReviewTestSettings(thread map[string]any) map[string]any {
	return map[string]any{
		"thread": thread, "cwd": "/workspace/job", "model": "gpt-5.6-sol", "reasoningEffort": "high",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false},
	}
}

func strictReviewTestTurn(id, nonce, input string) map[string]any {
	item := map[string]any{"type": "userMessage", "clientId": nonce, "content": []any{map[string]any{"type": "text", "text": input}}}
	return map[string]any{"id": id, "status": "completed", "items": []any{item, map[string]any{"type": "agentMessage", "text": `{"material":false}`}}}
}
