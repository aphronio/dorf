package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/incus"
	"github.com/coder/websocket"
)

const maxMessageBytes = 16 << 20

const (
	serverPIDPath = "/tmp/dorf/codex-app-server.pid"
	tokenPath     = "/tmp/dorf/codex-app-server.token"
)

type Agent struct {
	Sandbox incus.Sandbox
	Port    int
	Timeout time.Duration
}

type TurnOutcome struct {
	ID     string
	Status string
}

func (a Agent) ReconcileRun(ctx context.Context, sandboxName, workspace, boundSessionID, agentRunID, goal, model, effort string) (string, TurnOutcome, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	sessionID := boundSessionID
	var outcome TurnOutcome
	err := a.withServer(ctx, sandboxName, workspace, func(protocol *protocol) error {
		newSession := false
		if sessionID == "" {
			threads, err := protocol.listThreads(ctx, workspace)
			if err != nil {
				return err
			}
			if len(threads) > 1 {
				return fmt.Errorf("Codex reconciliation is ambiguous: isolated Sandbox contains %d native Sessions", len(threads))
			}
			if len(threads) == 1 {
				sessionID = threads[0]
			} else {
				sessionID, err = protocol.startThread(ctx, workspace, model)
				if err != nil {
					return err
				}
				newSession = true
			}
		}
		if !newSession {
			turns, err := protocol.readTurns(ctx, sessionID)
			if err != nil {
				return err
			}
			if len(turns) > 1 {
				return fmt.Errorf("Codex reconciliation is ambiguous: demonstrated Session contains %d native turns", len(turns))
			}
			if len(turns) == 1 {
				outcome = turns[0]
				if terminal(outcome.Status) {
					return nil
				}
				return protocol.pollTurn(ctx, sessionID, outcome.ID, &outcome)
			}
		}
		var err error
		outcome, err = protocol.startTurn(ctx, sessionID, workspace, agentRunID, goal, model, effort)
		return err
	})
	return sessionID, outcome, err
}

func (a Agent) timeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.Timeout > 0 {
		return context.WithTimeout(ctx, a.Timeout)
	}
	return context.WithCancel(ctx)
}

func (a Agent) withServer(ctx context.Context, sandboxName, workspace string, fn func(*protocol) error) error {
	address, err := a.Sandbox.PrivateIPv4(ctx, sandboxName)
	if err != nil {
		return err
	}
	if err := a.stopServer(ctx, sandboxName); err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	write, err := a.Sandbox.Exec(ctx, sandboxName, []byte(token+"\n"), "bash", "-lc", "umask 077; mkdir -p /tmp/dorf; cat > "+tokenPath)
	if err != nil {
		return err
	}
	if write.ExitCode != 0 {
		return fmt.Errorf("write Codex app-server capability: %s", strings.TrimSpace(write.Stderr))
	}
	defer a.Sandbox.Exec(context.WithoutCancel(ctx), sandboxName, nil, "rm", "-f", tokenPath)

	endpoint := "ws://" + address + ":" + strconv.Itoa(a.Port)
	script := appServerScript(endpoint)
	serverCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(serverCtx, "incus", "exec", sandboxName, "--cwd", workspace, "--", "bash", "-lc", script)
	var diagnostic boundedBuffer
	command.Stdout, command.Stderr = &diagnostic, &diagnostic
	if err := command.Start(); err != nil {
		return fmt.Errorf("launch Codex app-server: %w", err)
	}
	exited := make(chan struct{})
	var serverErr error
	go func() {
		serverErr = command.Wait()
		close(exited)
	}()
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = a.stopServer(stopCtx, sandboxName)
		stopCancel()
		cancel()
		<-exited
	}()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport}
	deadline := time.Now().Add(20 * time.Second)
	for {
		requestCtx, requestCancel := context.WithTimeout(ctx, 2*time.Second)
		headers := http.Header{"Authorization": []string{"Bearer " + token}}
		conn, response, dialErr := websocket.Dial(requestCtx, endpoint, &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: headers})
		requestCancel()
		if dialErr == nil {
			conn.SetReadLimit(maxMessageBytes)
			defer conn.Close(websocket.StatusNormalClosure, "done")
			p := &protocol{connection: conn}
			if err := p.initialize(ctx); err != nil {
				return err
			}
			return fn(p)
		}
		if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("Codex app-server rejected its scoped control capability")
		}
		select {
		case <-exited:
			return fmt.Errorf("Codex app-server exited before readiness (%v): %s", serverErr, diagnostic.String())
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Codex app-server did not become ready: %s", diagnostic.String())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func appServerScript(endpoint string) string {
	return "umask 077; mkdir -p /tmp/dorf; printf '%s\\n' \"$$\" > " + serverPIDPath + "; IFS= read -r DORF_PROVIDER_ROUTE_KEY < /root/.config/dorf/provider-route.key; export DORF_PROVIDER_ROUTE_KEY; exec codex app-server --listen " + endpoint + " --ws-auth capability-token --ws-token-file " + tokenPath
}

func (a Agent) stopServer(ctx context.Context, sandboxName string) error {
	script := "if test -f " + serverPIDPath + "; then IFS= read -r pid < " + serverPIDPath + "; case \"$pid\" in ''|*[!0-9]*) exit 1;; esac; if test -r /proc/$pid/cmdline && tr '\\000' ' ' < /proc/$pid/cmdline | grep -Fq 'codex app-server'; then kill \"$pid\" 2>/dev/null || true; fi; else pkill -TERM -f '[c]odex app-server --listen ws://' 2>/dev/null || true; fi; rm -f " + serverPIDPath + " " + tokenPath
	result, err := a.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-lc", script)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("stop prior Codex app-server: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
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
	if err := p.waitForTurn(ctx, sessionID, id, &outcome); err != nil {
		return TurnOutcome{}, err
	}
	return outcome, nil
}

func (p *protocol) waitForTurn(ctx context.Context, sessionID, turnID string, outcome *TurnOutcome) error {
	for {
		message, err := p.receive(ctx)
		if err != nil {
			return err
		}
		method, _ := message["method"].(string)
		if method != "turn/completed" {
			continue
		}
		params, _ := message["params"].(map[string]any)
		if params == nil || params["threadId"] != sessionID {
			continue
		}
		turn, _ := params["turn"].(map[string]any)
		if turn == nil || turn["id"] != turnID {
			continue
		}
		status, _ := turn["status"].(string)
		if !terminal(status) {
			return fmt.Errorf("turn/completed has unsupported native outcome %q", status)
		}
		outcome.ID, outcome.Status = turnID, status
		return nil
	}
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
			return nil, fmt.Errorf("Codex app-server rejected %s", method)
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

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if b.Len() < 16<<10 {
		remaining := (16 << 10) - b.Len()
		_, _ = b.Buffer.Write(value[:min(len(value), remaining)])
	}
	return len(value), nil
}
func (b *boundedBuffer) String() string { return strings.TrimSpace(b.Buffer.String()) }
