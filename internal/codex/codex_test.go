package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"

	"strings"
	"sync/atomic"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/coder/websocket"
)

func testSandbox(runner incustest.Runner, owner provider.Ownership) incus.Adapter {
	return incus.Adapter{Sandbox: incustest.OwnedSandbox(runner, incus.Config{}, owner)}
}

func testOwner(sandboxID string) provider.Ownership {
	return provider.Ownership{SessionID: "session-" + sandboxID, SandboxID: sandboxID, OwnershipNonce: strings.Repeat("a", 64)}
}

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

func TestCodexCommandBoundaryKeepsFixedPolicyAndScopedCapability(t *testing.T) {
	const token = "private-control-capability"
	digest := tokenSHA256(token)
	implementation := appServerScript("ws://10.0.0.2:4500", digest)

	if !strings.Contains(implementation, `-c 'approval_policy="never"'`) || strings.Contains(implementation, `sandbox_mode=`) {
		t.Fatalf("implementation launch policy = %s", implementation)
	}
	for _, launch := range []string{implementation} {
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
	if err := agent.withServerEndpointController(context.Background(), testOwner("sandbox-1"), endpoint, func(_ *protocol) error { return nil }); err != nil {
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
		"threadId": sessionID, "cwd": "/workspace",
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
			case "initialize", "skills/list":
				return map[string]any{}, false
			case "thread/resume":
				requireProtocolParams(t, method, params, map[string]any{"threadId": sessionID})
				return map[string]any{"thread": map[string]any{"id": sessionID}}, false
			case "turn/start":
				requireProtocolParams(t, method, params, map[string]any{
					"threadId": sessionID, "clientUserMessageId": messageID, "cwd": "/workspace",
					"model": "gpt-5.6-sol", "effort": "high", "approvalPolicy": "never",
					"sandboxPolicy": map[string]any{"type": "dangerFullAccess"},
				})
				return map[string]any{"turn": map[string]any{"id": turnID}}, false
			default:
				return nil, true
			}
		})
		defer server.Close()
		outcome, err := dialTestProtocol(t, server).resumeFixture(context.Background(), sessionID, "/workspace", messageID, core.HarnessInput{Text: "input"}, "gpt-5.6-sol", "high", "danger-full-access")
		if err != nil || outcome.ID != turnID {
			t.Fatalf("resume and start outcome=%#v err=%v", outcome, err)
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
		_, err := dialTestProtocol(t, server).resumeFixture(context.Background(), sessionID, "/workspace", messageID, core.HarnessInput{Text: "input"}, "gpt-5.6-sol", "high", "danger-full-access")
		var definite interface{ DefiniteNoSubmit() bool }
		if !errors.As(err, &definite) || !definite.DefiniteNoSubmit() {
			t.Fatalf("substitute resume error=%T %v", err, err)
		}
	})
}

// The fake app-server implements no clientUserMessageId deduplication. A
// deliberately different hint still adopts by isolated thread history.

func protocolMethods(requests <-chan map[string]any) []string {
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
