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
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/coder/websocket"
)

// Native history can include the inline bytes of a full accepted message.
const maxMessageBytes = 16<<20 + ((core.MaxAttachments*provider.MaxFileWriteBytes+2)/3)*4

const (
	serverPIDPath    = "/tmp/dorf/codex-app-server.pid"
	controlTokenPath = "/tmp/dorf/codex-app-server.control-token"
	serverLogPath    = "/tmp/dorf/codex-app-server.log"
	serverControlDir = "/tmp/dorf"
	serverAuthMode   = "capability-token"
)

type Agent struct {
	Sandbox      provider.Sandbox
	Port         int
	Timeout      time.Duration
	Observations *Observations
}

const Harness = "codex"

func (a Agent) Name() string { return Harness }

type TurnOutcome = core.HarnessTurn

type RejectedError struct {
	Method string
}

func (e *RejectedError) Error() string          { return "Codex app-server rejected " + e.Method }
func (e *RejectedError) DefiniteNoSubmit() bool { return true }

type resumeBindingError struct{}

func (e *resumeBindingError) Error() string {
	return "thread/resume did not return the exact bound thread"
}
func (e *resumeBindingError) DefiniteNoSubmit() bool { return true }

type skillsReloadError struct{ err error }

func (e *skillsReloadError) Error() string          { return "refresh Codex skills: " + e.err.Error() }
func (e *skillsReloadError) Unwrap() error          { return e.err }
func (e *skillsReloadError) DefiniteNoSubmit() bool { return true }

type attentionError struct{ reason string }

func (e *attentionError) Error() string         { return e.reason }
func (e *attentionError) AttentionNeeded() bool { return true }

func (a Agent) ReadTurns(ctx context.Context, owner provider.Ownership, threadID string) (core.HarnessHistory, error) {

	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	var turns []TurnOutcome
	err := a.withServer(ctx, owner, func(protocol *protocol) error {
		var err error
		thread, err := protocol.readThread(ctx, threadID)
		if err != nil {
			return err
		}
		turns, err = protocol.parseReadTurns(threadID, thread)
		if err != nil {
			return err
		}
		return a.readUsage(ctx, owner, threadID, stringValue(thread["path"]), turns)
	})
	return core.HarnessHistory{Harness: Harness, ThreadID: threadID, Turns: turns}, err
}

func (a Agent) timeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.Timeout > 0 {
		return context.WithTimeout(ctx, a.Timeout)
	}
	return context.WithCancel(ctx)
}

func (a Agent) withSandboxAccess(ctx context.Context, owner provider.Ownership, fn func(Agent) error) error {

	if scoped, ok := a.Sandbox.(provider.ScopedAccess); ok {
		return scoped.WithAccess(ctx, owner, func(sandbox provider.Sandbox) error {
			a.Sandbox = sandbox
			return fn(a)
		})
	}
	return fn(a)
}

func (a Agent) withServer(ctx context.Context, owner provider.Ownership, fn func(*protocol) error) error {
	return a.openServer(ctx, owner, fn)
}

func (a Agent) openServer(ctx context.Context, owner provider.Ownership, fn func(*protocol) error) error {
	return a.withSandboxAccess(ctx, owner, func(a Agent) error {
		endpoint, err := a.Sandbox.Endpoint(ctx, owner, a.Port)
		if err != nil {
			return err
		}
		return a.withServerEndpointController(ctx, owner, endpointAccess{listen: endpoint.ListenURL, dial: endpoint.DialURL, headers: endpoint.Headers(), dialContext: endpoint.DialContext()}, fn)
	})
}

func (a Agent) withServerEndpoint(ctx context.Context, owner provider.Ownership, endpoint string, fn func(*protocol) error) error {
	return a.withServerEndpointController(ctx, owner, sameEndpoint(endpoint), fn)
}

type endpointAccess struct {
	listen      string
	dial        string
	headers     http.Header
	dialContext provider.DialContextFunc
}

func sameEndpoint(endpoint string) endpointAccess {
	return endpointAccess{listen: endpoint, dial: endpoint}
}

func (a Agent) withServerEndpointController(ctx context.Context, owner provider.Ownership, endpoint endpointAccess, fn func(*protocol) error) error {
	probe, err := a.probeServer(ctx, owner, endpoint.listen)
	if err != nil {
		return err
	}
	// Prefer the exact authenticated process left by a dead executor. A live
	// process that cannot be inspected is attention, never permission to kill it.
	if probe.running && probe.tracked && probe.token != "" {
		protocol, dialErr := dialProtocol(ctx, endpoint.dial, probe.token, endpoint.headers, endpoint.dialContext)
		if dialErr == nil {
			protocol.configureObservations(ctx, a.Observations, owner)
			defer protocol.finish()
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
	write, err := a.Sandbox.Exec(ctx, owner, []byte(token+"\n"), "bash", "-lc", controlCapabilityScript())
	if err != nil {
		return err
	}
	if write.ExitCode != 0 {
		return fmt.Errorf("write Codex app-server capability: %s", strings.TrimSpace(write.Stderr))
	}
	launch := appServerScript(endpoint.listen, tokenSHA256(token))
	result, err := a.Sandbox.Exec(ctx, owner, nil, "bash", "-lc", launch)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("launch Codex app-server: %s", strings.TrimSpace(result.Stderr))
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		protocol, dialErr := dialProtocol(ctx, endpoint.dial, token, endpoint.headers, endpoint.dialContext)
		if dialErr == nil {
			protocol.configureObservations(ctx, a.Observations, owner)
			defer protocol.finish()
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
	configuration := ` -c 'approval_policy="never"'`
	return "set -e; umask 077; install -d -m 700 " + serverControlDir + "; rm -f " + serverPIDPath + "; IFS= read -r DORF_PROVIDER_ROUTE_KEY < " + routeKeyPath + "; export DORF_PROVIDER_ROUTE_KEY; " + loadRouteOptions + "nohup codex app-server \"${route_options[@]}\"" + configuration + " --listen " + endpoint + " --ws-auth " + serverAuthMode + " --ws-token-sha256 " + tokenDigest + " </dev/null >" + serverLogPath + " 2>&1 & printf '%s\\n' \"$!\" > " + serverPIDPath
}

type serverProbe struct {
	running bool
	tracked bool
	token   string
}

func (a Agent) probeServer(ctx context.Context, owner provider.Ownership, endpoint string) (serverProbe, error) {
	script := probeServerScript(endpoint)
	result, err := a.Sandbox.Exec(ctx, owner, nil, "bash", "-lc", script)
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

func dialProtocol(ctx context.Context, endpoint, token string, headers http.Header, providerDial provider.DialContextFunc) (*protocol, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if providerDial != nil {
		transport.DialContext = providerDial
	}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport}
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	requestHeaders := make(http.Header)
	if headers != nil {
		requestHeaders = headers.Clone()
	}
	requestHeaders.Set("Authorization", "Bearer "+token)
	conn, response, err := websocket.Dial(requestCtx, endpoint, &websocket.DialOptions{HTTPClient: httpClient, HTTPHeader: requestHeaders})
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
	refreshSkills              bool
	instructions               *workspaceInstructions
	freshThread                bool
	connection                 *websocket.Conn
	nextID                     int
	observations               *Observations
	owner                      provider.Ownership
	execution                  telemetry.NativeExecution
	observed                   *observedTurn
	pendingObservations        []map[string]any
	pendingObservationBytes    int
	pendingObservationOverflow bool
}

func (p *protocol) configureObservations(ctx context.Context, observations *Observations, owner provider.Ownership) {
	p.observations, p.owner = observations, owner
	if run, ok := telemetry.Execution(ctx); ok && run.SessionID == owner.SessionID && run.SandboxID == owner.SandboxID {
		p.execution = run
	}
}

func (p *protocol) initialize(ctx context.Context) error {
	if _, err := p.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "dorf", "title": "Dorf", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return err
	}
	return p.send(ctx, map[string]any{"method": "initialized", "params": map[string]any{}})
}

func (p *protocol) startThread(ctx context.Context, workspace, model, capability string) (string, error) {
	result, err := p.call(ctx, "thread/start", map[string]any{"cwd": workspace, "model": model, "approvalPolicy": "never", "sandbox": capability, "config": map[string]any{"project_doc_max_bytes": maxInstructionFileBytes}})
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

func (p *protocol) readThread(ctx context.Context, sessionID string) (map[string]any, error) {
	result, err := p.call(ctx, "thread/read", map[string]any{"threadId": sessionID, "includeTurns": true})
	if err != nil {
		return nil, err
	}
	thread, _ := result["thread"].(map[string]any)
	if thread == nil || thread["id"] != sessionID {
		return nil, fmt.Errorf("thread/read did not return the bound thread")
	}
	return thread, nil
}

func (p *protocol) readTurns(ctx context.Context, sessionID string) ([]TurnOutcome, error) {
	thread, err := p.readThread(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return p.parseReadTurns(sessionID, thread)
}

func (p *protocol) parseReadTurns(sessionID string, thread map[string]any) ([]TurnOutcome, error) {
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
		parsed := parseTurn(turn)
		if parsed.ID != "" {
			turns = append(turns, parsed)
			p.seedReadTurn(sessionID, turn)
			if !parsed.Terminal() {
				if p.execution.ID == "" {
					p.execution = telemetry.NativeExecution{ID: parsed.ID, SessionID: p.owner.SessionID, SandboxID: p.owner.SandboxID, ThreadID: sessionID, TurnID: parsed.ID, Harness: Harness}
				}
				p.bindObservation(sessionID, parsed.ID, false)
			}
		}
	}
	return turns, nil
}

func parseTurn(turn map[string]any) TurnOutcome {
	id, _ := turn["id"].(string)
	status, _ := turn["status"].(string)
	outcome := TurnOutcome{ID: id, Status: status}
	var replies []string
	items, _ := turn["items"].([]any)
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if deliveryID := observationDeliveryID(item); deliveryID != "" {
			outcome.ClientIDs = append(outcome.ClientIDs, deliveryID)
			continue
		}
		if item["type"] == "userMessage" {
			if clientID := stringValue(item["clientId"]); clientID != "" {
				outcome.ClientIDs = append(outcome.ClientIDs, clientID)
			}
			continue
		}
		if item["type"] != "agentMessage" || (item["phase"] != nil && item["phase"] != "final_answer") {
			continue
		}
		if text := agentMessageText(item); text != "" {
			replies = append(replies, text)
		}
	}
	outcome.Output = strings.Join(replies, "\n\n")
	return outcome
}

func agentMessageText(item map[string]any) string {
	if text, ok := item["text"].(string); ok && text != "" {
		return text
	}
	var text strings.Builder
	contents, _ := item["content"].([]any)
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		if fragment, ok := content["text"].(string); ok {
			text.WriteString(fragment)
		}
	}
	return text.String()
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

func (p *protocol) startTurn(ctx context.Context, sessionID, workspace, inputID string, goal core.HarnessInput, model, effort, capability string) (TurnOutcome, error) {
	policyType := "dangerFullAccess"
	if capability == "read-only" {
		policyType = "readOnly"
	}
	if p.refreshSkills {
		if _, err := p.call(ctx, "skills/list", map[string]any{"cwds": []string{workspace}, "forceReload": true}); err != nil {
			return TurnOutcome{}, &skillsReloadError{err: err}
		}
	}
	bound := false
	defer func() {
		if !bound {
			p.forgetWorkspaceInstructions(sessionID)
		}
	}()
	if err := p.injectDeveloperInstructions(ctx, sessionID, goal.DeveloperInstructions); err != nil {
		return TurnOutcome{}, err
	}
	if err := p.injectWorkspaceInstructions(ctx, sessionID, inputID); err != nil {
		return TurnOutcome{}, err
	}
	params := map[string]any{"threadId": sessionID, "clientUserMessageId": inputID, "input": nativeUserInput(goal), "cwd": workspace, "model": model, "effort": effort, "approvalPolicy": "never", "sandboxPolicy": map[string]string{"type": policyType}}
	if goal.Observation {
		delete(params, "clientUserMessageId")
		params["input"] = []any{}
		params["toolOutput"] = nativeObservation(inputID, goal.Text)
	}
	result, err := p.call(ctx, "turn/start", params)
	if err != nil {
		return TurnOutcome{}, err
	}
	turn, _ := result["turn"].(map[string]any)
	id, _ := turn["id"].(string)
	if id == "" {
		return TurnOutcome{}, fmt.Errorf("turn/start response is missing result.turn.id")
	}
	outcome := TurnOutcome{ID: id, Status: "running"}
	if p.execution.ID == inputID {
		bound = p.bindObservation(sessionID, id, true)
	}
	return outcome, nil
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
	p.observeNotification(message)
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

func stringValue(value any) string { text, _ := value.(string); return text }
