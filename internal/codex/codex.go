package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/coder/websocket"
)

const maxMessageBytes = 16 << 20

const (
	serverPIDPath    = "/tmp/dorf/codex-app-server.pid"
	controlTokenPath = "/tmp/dorf/codex-app-server.control-token"
	serverLogPath    = "/tmp/dorf/codex-app-server.log"
	serverControlDir = "/tmp/dorf"
	serverAuthMode   = "capability-token"
)

type Agent struct {
	Sandbox incus.Sandbox
	Port    int
	Timeout time.Duration
}

type TurnOutcome = spine.NativeTurn

type RejectedError struct{ Method string }

func (e *RejectedError) Error() string          { return "Codex app-server rejected " + e.Method }
func (e *RejectedError) DefiniteNoSubmit() bool { return true }

type resumeBindingError struct{}

func (e *resumeBindingError) Error() string {
	return "thread/resume did not return the exact bound native Session"
}
func (e *resumeBindingError) DefiniteNoSubmit() bool { return true }

type attentionError struct{ reason string }

func (e *attentionError) Error() string         { return e.reason }
func (e *attentionError) AttentionNeeded() bool { return true }

func (a Agent) StartInitialTurn(ctx context.Context, sandboxName, workspace, agentRunID, input, model, effort string) (string, TurnOutcome, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	var sessionID string
	var outcome TurnOutcome
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		var err error
		sessionID, outcome, err = protocol.reconcileInitialTurn(ctx, workspace, agentRunID, input, model, effort)
		return err
	})
	return sessionID, outcome, err
}

func (a Agent) ReadTurns(ctx context.Context, sandboxName, workspace, sessionID string) ([]TurnOutcome, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	var turns []TurnOutcome
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		var err error
		turns, err = protocol.readTurns(ctx, sessionID)
		return err
	})
	return turns, err
}

func (a Agent) StartTurn(ctx context.Context, sandboxName, workspace, sessionID, agentRunID, input, model, effort string) (TurnOutcome, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	var outcome TurnOutcome
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		var err error
		outcome, err = protocol.resumeAndStartTurn(ctx, sessionID, workspace, agentRunID, input, model, effort)
		return err
	})
	return outcome, err
}

func (a Agent) WaitTurn(ctx context.Context, sandboxName, workspace, sessionID, turnID string) (TurnOutcome, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	outcome := TurnOutcome{ID: turnID, Status: "running"}
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		return protocol.pollTurn(ctx, sessionID, turnID, &outcome)
	})
	return outcome, err
}

func (a Agent) timeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.Timeout > 0 {
		return context.WithTimeout(ctx, a.Timeout)
	}
	return context.WithCancel(ctx)
}

func (a Agent) withServer(ctx context.Context, sandboxName string, fn func(*protocol) error) error {
	address, err := a.Sandbox.PrivateIPv4(ctx, sandboxName)
	if err != nil {
		return err
	}
	endpoint := "ws://" + address + ":" + strconv.Itoa(a.Port)
	return a.withServerEndpoint(ctx, sandboxName, endpoint, fn)
}

func (a Agent) withServerEndpoint(ctx context.Context, sandboxName, endpoint string, fn func(*protocol) error) error {
	probe, err := a.probeServer(ctx, sandboxName, endpoint)
	if err != nil {
		return err
	}
	// Prefer the exact authenticated process left by a dead executor. A live
	// process that cannot be inspected is attention, never permission to kill it.
	if probe.running && probe.tracked && probe.token != "" {
		protocol, dialErr := dialProtocol(ctx, endpoint, probe.token)
		if dialErr == nil {
			defer protocol.connection.Close(websocket.StatusNormalClosure, "done")
			return fn(protocol)
		}
		if probe.running {
			return fmt.Errorf("exact live Codex app-server could not be authenticated or inspected: %w", dialErr)
		}
	}
	if probe.running {
		return fmt.Errorf("exact live Codex app-server has no recoverable scoped capability; refusing to kill it before inspection")
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	write, err := a.Sandbox.Exec(ctx, sandboxName, []byte(token+"\n"), "bash", "-lc", controlCapabilityScript())
	if err != nil {
		return err
	}
	if write.ExitCode != 0 {
		return fmt.Errorf("write Codex app-server capability: %s", strings.TrimSpace(write.Stderr))
	}
	launch := appServerScript(endpoint, tokenSHA256(token))
	result, err := a.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-lc", launch)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("launch Codex app-server: %s", strings.TrimSpace(result.Stderr))
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		protocol, dialErr := dialProtocol(ctx, endpoint, token)
		if dialErr == nil {
			defer protocol.connection.Close(websocket.StatusNormalClosure, "done")
			return fn(protocol)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Codex app-server did not become ready: %w", dialErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func controlCapabilityScript() string {
	return "umask 077; install -d -m 700 " + serverControlDir + "; cat > " + controlTokenPath + ".new; chmod 600 " + controlTokenPath + ".new; mv -f " + controlTokenPath + ".new " + controlTokenPath
}

func appServerScript(endpoint, tokenDigest string) string {
	return "umask 077; install -d -m 700 " + serverControlDir + "; rm -f " + serverPIDPath + "; IFS= read -r DORF_PROVIDER_ROUTE_KEY < /root/.config/dorf/provider-route.key; export DORF_PROVIDER_ROUTE_KEY; nohup codex app-server --listen " + endpoint + " --ws-auth " + serverAuthMode + " --ws-token-sha256 " + tokenDigest + " </dev/null >" + serverLogPath + " 2>&1 & printf '%s\\n' \"$!\" > " + serverPIDPath
}

type serverProbe struct {
	running bool
	tracked bool
	token   string
}

func (a Agent) probeServer(ctx context.Context, sandboxName, endpoint string) (serverProbe, error) {
	script := probeServerScript(endpoint)
	result, err := a.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-lc", script)
	if err != nil {
		return serverProbe{}, err
	}
	if result.ExitCode != 0 {
		return serverProbe{}, fmt.Errorf("inspect exact Codex app-server: %s", strings.TrimSpace(result.Stderr))
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	probe := serverProbe{running: len(lines) > 0 && lines[0] == "1", tracked: len(lines) > 1 && lines[1] == "1"}
	if probe.tracked && len(lines) > 2 {
		probe.token = strings.TrimSpace(lines[2])
	}
	return probe, nil
}

func probeServerScript(endpoint string) string {
	return "running=0; tracked=0; pid=; if test -f " + serverPIDPath + "; then IFS= read -r pid < " + serverPIDPath + "; case \"$pid\" in ''|*[!0-9]*) pid=;; esac; fi; " +
		"if test -n \"$pid\" && test -r /proc/$pid/cmdline && tr '\\000' ' ' < /proc/$pid/cmdline | grep -Fq 'codex app-server' && tr '\\000' ' ' < /proc/$pid/cmdline | grep -Fq -- '--listen " + endpoint + "' && tr '\\000' ' ' < /proc/$pid/cmdline | grep -Fq -- '--ws-auth " + serverAuthMode + "'; then running=1; tracked=1; fi; " +
		"if test \"$running\" = 0 && pgrep -f '[c]odex app-server --listen ws://' >/dev/null; then running=1; fi; printf '%s\\n' \"$running\" \"$tracked\"; " +
		"if test \"$tracked\" = 1 && test -r " + controlTokenPath + "; then IFS= read -r token < " + controlTokenPath + "; if test -n \"$token\"; then digest=$(printf '%s' \"$token\" | sha256sum); digest=${digest%% *}; if tr '\\000' ' ' < /proc/$pid/cmdline | grep -Fq -- \"--ws-token-sha256 $digest\"; then printf '%s\\n' \"$token\"; fi; fi; fi"
}

func tokenSHA256(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func dialProtocol(ctx context.Context, endpoint, token string) (*protocol, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport}
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	conn, response, err := websocket.Dial(requestCtx, endpoint, &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: headers})
	if err != nil {
		if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("Codex app-server rejected its scoped control capability")
		}
		return nil, err
	}
	conn.SetReadLimit(maxMessageBytes)
	p := &protocol{connection: conn}
	if err := p.initialize(ctx); err != nil {
		conn.CloseNow()
		return nil, err
	}
	return p, nil
}

type protocol struct {
	connection *websocket.Conn
	nextID     int
	pending    []map[string]any
}

func (p *protocol) initialize(ctx context.Context) error {
	if _, err := p.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "dorf", "title": "Dorf", "version": "0.1.0"}}); err != nil {
		return err
	}
	return p.send(ctx, map[string]any{"method": "initialized", "params": map[string]any{}})
}

func (p *protocol) listThreads(ctx context.Context, workspace string) ([]string, error) {
	result, err := p.call(ctx, "thread/list", map[string]any{"limit": 100, "cwd": workspace})
	if err != nil {
		return nil, err
	}
	data, ok := result["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("thread/list response is missing result.data")
	}
	ids := make([]string, 0, len(data))
	for _, value := range data {
		thread, ok := value.(map[string]any)
		if !ok {
			continue
		}
		id, _ := thread["id"].(string)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (p *protocol) startThread(ctx context.Context, workspace, model string) (string, error) {
	result, err := p.call(ctx, "thread/start", map[string]any{"cwd": workspace, "model": model, "approvalPolicy": "never", "sandbox": "danger-full-access"})
	if err != nil {
		return "", err
	}
	thread, _ := result["thread"].(map[string]any)
	id, _ := thread["id"].(string)
	if id == "" {
		return "", fmt.Errorf("thread/start response is missing result.thread.id")
	}
	return id, nil
}

func (p *protocol) reconcileInitialTurn(ctx context.Context, workspace, agentRunID, goal, model, effort string) (string, TurnOutcome, error) {
	threads, err := p.listThreads(ctx, workspace)
	if err != nil {
		return "", TurnOutcome{}, err
	}
	if len(threads) > 1 {
		return "", TurnOutcome{}, &attentionError{reason: fmt.Sprintf("Codex reconciliation is ambiguous: isolated Sandbox contains %d native Sessions", len(threads))}
	}
	if len(threads) == 0 {
		sessionID, err := p.startThread(ctx, workspace, model)
		if err != nil {
			return "", TurnOutcome{}, err
		}
		turn, err := p.startTurn(ctx, sessionID, workspace, agentRunID, goal, model, effort)
		return sessionID, turn, err
	}
	sessionID := threads[0]
	turns, err := p.readTurns(ctx, sessionID)
	if err != nil {
		return "", TurnOutcome{}, fmt.Errorf("inspect isolated native Session before initial submit: %v", err)
	}
	if len(turns) > 1 {
		return "", TurnOutcome{}, &attentionError{reason: fmt.Sprintf("Codex reconciliation is ambiguous: initial native Session contains %d turns", len(turns))}
	}
	if len(turns) == 1 {
		return sessionID, turns[0], nil
	}
	turn, err := p.startTurn(ctx, sessionID, workspace, agentRunID, goal, model, effort)
	return sessionID, turn, err
}

func (p *protocol) readTurns(ctx context.Context, sessionID string) ([]TurnOutcome, error) {
	result, err := p.call(ctx, "thread/read", map[string]any{"threadId": sessionID, "includeTurns": true})
	if err != nil {
		return nil, err
	}
	thread, _ := result["thread"].(map[string]any)
	if thread == nil || thread["id"] != sessionID {
		return nil, fmt.Errorf("thread/read did not return the bound native Session")
	}
	values, ok := thread["turns"].([]any)
	if !ok {
		return nil, fmt.Errorf("thread/read response is missing native turn identities")
	}
	turns := make([]TurnOutcome, 0, len(values))
	for _, value := range values {
		turn, ok := value.(map[string]any)
		if !ok {
			continue
		}
		id, _ := turn["id"].(string)
		status, _ := turn["status"].(string)
		if id != "" {
			turns = append(turns, TurnOutcome{ID: id, Status: status})
		}
	}
	return turns, nil
}

func (p *protocol) resumeThread(ctx context.Context, sessionID string) error {
	result, err := p.call(ctx, "thread/resume", map[string]any{"threadId": sessionID})
	if err != nil {
		return err
	}
	thread, _ := result["thread"].(map[string]any)
	if thread == nil || thread["id"] != sessionID {
		return &resumeBindingError{}
	}
	return nil
}

func (p *protocol) resumeAndStartTurn(ctx context.Context, sessionID, workspace, agentRunID, goal, model, effort string) (TurnOutcome, error) {
	if err := p.resumeThread(ctx, sessionID); err != nil {
		return TurnOutcome{}, err
	}
	return p.startTurn(ctx, sessionID, workspace, agentRunID, goal, model, effort)
}

func (p *protocol) startTurn(ctx context.Context, sessionID, workspace, agentRunID, goal, model, effort string) (TurnOutcome, error) {
	result, err := p.call(ctx, "turn/start", map[string]any{"threadId": sessionID, "clientUserMessageId": agentRunID, "input": []map[string]string{{"type": "text", "text": goal}}, "cwd": workspace, "model": model, "effort": effort, "approvalPolicy": "never", "sandboxPolicy": map[string]string{"type": "dangerFullAccess"}})
	if err != nil {
		return TurnOutcome{}, err
	}
	turn, _ := result["turn"].(map[string]any)
	id, _ := turn["id"].(string)
	if id == "" {
		return TurnOutcome{}, fmt.Errorf("turn/start response is missing result.turn.id")
	}
	outcome := TurnOutcome{ID: id, Status: "running"}
	return outcome, nil
}

func (p *protocol) pollTurn(ctx context.Context, sessionID, turnID string, outcome *TurnOutcome) error {
	for {
		turns, err := p.readTurns(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, turn := range turns {
			if turn.ID == turnID && terminal(turn.Status) {
				*outcome = turn
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (p *protocol) call(ctx context.Context, method string, params any) (map[string]any, error) {
	id := p.nextID
	p.nextID++
	if err := p.send(ctx, map[string]any{"method": method, "id": id, "params": params}); err != nil {
		return nil, err
	}
	for {
		message, err := p.receiveRaw(ctx)
		if err != nil {
			return nil, err
		}
		responseID, exists := numericID(message["id"])
		if !exists {
			p.pending = append(p.pending, message)
			continue
		}
		if responseID != id {
			return nil, fmt.Errorf("unexpected app-server response id %d while waiting for %d", responseID, id)
		}
		if nativeError := message["error"]; nativeError != nil {
			return nil, &RejectedError{Method: method}
		}
		result, ok := message["result"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s response is missing result", method)
		}
		return result, nil
	}
}

func (p *protocol) send(ctx context.Context, message map[string]any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err := p.connection.Write(ctx, websocket.MessageText, payload); err != nil {
		return fmt.Errorf("send Codex app-server message: %w", err)
	}
	return nil
}

func (p *protocol) receive(ctx context.Context) (map[string]any, error) {
	if len(p.pending) > 0 {
		message := p.pending[0]
		p.pending = p.pending[1:]
		return message, nil
	}
	return p.receiveRaw(ctx)
}

func (p *protocol) receiveRaw(ctx context.Context) (map[string]any, error) {
	messageType, payload, err := p.connection.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("receive Codex app-server message: %w", err)
	}
	if messageType != websocket.MessageText {
		return nil, fmt.Errorf("Codex app-server sent a non-text protocol message")
	}
	var message map[string]any
	if err := json.Unmarshal(payload, &message); err != nil {
		return nil, fmt.Errorf("decode Codex app-server message: %w", err)
	}
	return message, nil
}

func terminal(status string) bool {
	return status == "completed" || status == "interrupted" || status == "failed"
}

func numericID(value any) (int, bool) {
	switch id := value.(type) {
	case float64:
		return int(id), true
	case int:
		return id, true
	default:
		return 0, false
	}
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
