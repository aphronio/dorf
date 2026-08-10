package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const Harness = "codex"

type TurnOutcome = spine.HarnessTurn

type RejectedError struct{ Method string }

func (e *RejectedError) Error() string          { return "Codex app-server rejected " + e.Method }
func (e *RejectedError) DefiniteNoSubmit() bool { return true }

type resumeBindingError struct{}

func (e *resumeBindingError) Error() string {
	return "thread/resume did not return the exact bound thread"
}
func (e *resumeBindingError) DefiniteNoSubmit() bool { return true }

type attentionError struct{ reason string }

func (e *attentionError) Error() string         { return e.reason }
func (e *attentionError) AttentionNeeded() bool { return true }

type reviewVisibilityError struct{ reason string }

func (e *reviewVisibilityError) Error() string                   { return e.reason }
func (e *reviewVisibilityError) RetryableReviewVisibility() bool { return true }

func (a Agent) StartInitialTurn(ctx context.Context, sandboxName, workspace, agentRunID, input, model, effort string) (spine.HarnessBinding, error) {
	threadID, turn, err := a.startInitialTurn(ctx, sandboxName, workspace, agentRunID, input, model, effort, "danger-full-access")
	return spine.HarnessBinding{Harness: Harness, ThreadID: threadID, Turn: turn}, err
}

func (a Agent) StartStrictReviewTurn(ctx context.Context, sandboxName, workspace string, owner incus.ReviewMetadata, submissionNonce, input, model, effort string) (spine.HarnessBinding, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	binding := spine.HarnessBinding{Harness: Harness}
	err := a.withReviewServer(ctx, sandboxName, owner, func(protocol *protocol) error {
		threadID, turn, err := protocol.reconcileStrictReviewTurn(ctx, workspace, "", submissionNonce, input, model, effort, true)
		binding.ThreadID, binding.Turn = threadID, turn
		return err
	})
	return binding, err
}

func (a Agent) ReadStrictReviewTurns(ctx context.Context, sandboxName, workspace string, owner incus.ReviewMetadata, threadID, submissionNonce, input, model, effort string) (spine.HarnessHistory, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	history := spine.HarnessHistory{Harness: Harness}
	err := a.withReviewServer(ctx, sandboxName, owner, func(protocol *protocol) error {
		observedThread, turns, err := protocol.strictReviewHistory(ctx, workspace, threadID, submissionNonce, input, model, effort)
		history.ThreadID, history.Turns = observedThread, turns
		return err
	})
	return history, err
}

func (a Agent) RecoverStrictReviewTurn(ctx context.Context, sandboxName, workspace string, owner incus.ReviewMetadata, submissionNonce, input, model, effort string) (spine.HarnessBinding, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	binding := spine.HarnessBinding{Harness: Harness}
	err := a.withReviewServer(ctx, sandboxName, owner, func(protocol *protocol) error {
		threadID, turn, err := protocol.reconcileStrictReviewTurn(ctx, workspace, "", submissionNonce, input, model, effort, false)
		binding.ThreadID, binding.Turn = threadID, turn
		return err
	})
	return binding, err
}

func (a Agent) WaitStrictReviewTurn(ctx context.Context, sandboxName, workspace string, owner incus.ReviewMetadata, threadID, turnID, submissionNonce, input, model, effort string) (spine.HarnessBinding, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	binding := spine.HarnessBinding{Harness: Harness, ThreadID: threadID, Turn: TurnOutcome{ID: turnID, Status: "running"}}
	err := a.withReviewServer(ctx, sandboxName, owner, func(protocol *protocol) error {
		missingAttempts := 0
		for {
			observedThread, turns, err := protocol.strictReviewHistory(ctx, workspace, threadID, submissionNonce, input, model, effort)
			if err != nil {
				var missing interface{ RetryableReviewVisibility() bool }
				if !errors.As(err, &missing) || !missing.RetryableReviewVisibility() || missingAttempts >= 20 {
					return err
				}
				missingAttempts++
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			missingAttempts = 0
			binding.ThreadID = observedThread
			if len(turns) != 1 || turns[0].ID != turnID {
				return reviewAttention("strict review recovery found the wrong native turn")
			}
			binding.Turn = turns[0]
			if terminal(turns[0].Status) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	})
	return binding, err
}

func (a Agent) startInitialTurn(ctx context.Context, sandboxName, workspace, agentRunID, input, model, effort, capability string) (string, TurnOutcome, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	var sessionID string
	var outcome TurnOutcome
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		var err error
		sessionID, outcome, err = protocol.reconcileInitialTurn(ctx, workspace, agentRunID, input, model, effort, capability)
		return err
	})
	return sessionID, outcome, err
}

func (a Agent) ReadInitialTurns(ctx context.Context, sandboxName, workspace string) (spine.HarnessHistory, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	var threadID string
	var turns []TurnOutcome
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		var err error
		threadID, turns, err = protocol.inspectInitialTurns(ctx, workspace)
		return err
	})
	return spine.HarnessHistory{Harness: Harness, ThreadID: threadID, Turns: turns}, err
}

func (a Agent) ReadTurns(ctx context.Context, sandboxName, threadID string) (spine.HarnessHistory, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	var turns []TurnOutcome
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		var err error
		turns, err = protocol.readTurns(ctx, threadID)
		return err
	})
	return spine.HarnessHistory{Harness: Harness, ThreadID: threadID, Turns: turns}, err
}

func (a Agent) StartTurn(ctx context.Context, sandboxName, workspace, threadID, agentRunID, input, model, effort string) (spine.HarnessBinding, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	var outcome TurnOutcome
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		var err error
		outcome, err = protocol.resumeAndStartTurn(ctx, threadID, workspace, agentRunID, input, model, effort, "danger-full-access")
		return err
	})
	return spine.HarnessBinding{Harness: Harness, ThreadID: threadID, Turn: outcome}, err
}

func (a Agent) SteerTurn(ctx context.Context, sandboxName, sessionID, turnID, agentRunID, input string) (string, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	var acceptedTurnID string
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		var err error
		acceptedTurnID, err = protocol.steerTurn(ctx, sessionID, turnID, agentRunID, input)
		return err
	})
	return acceptedTurnID, err
}

func (a Agent) WaitTurn(ctx context.Context, sandboxName, threadID, turnID string) (spine.HarnessBinding, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	outcome := TurnOutcome{ID: turnID, Status: "running"}
	err := a.withServer(ctx, sandboxName, func(protocol *protocol) error {
		return protocol.pollTurn(ctx, threadID, turnID, &outcome)
	})
	return spine.HarnessBinding{Harness: Harness, ThreadID: threadID, Turn: outcome}, err
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

func (a Agent) withReviewServer(ctx context.Context, sandboxName string, owner incus.ReviewMetadata, fn func(*protocol) error) error {
	if err := a.Sandbox.AttestReview(ctx, sandboxName, owner); err != nil {
		return err
	}
	address, err := a.Sandbox.PrivateIPv4(ctx, sandboxName)
	if err != nil {
		return err
	}
	endpoint := "ws://" + address + ":" + strconv.Itoa(a.Port)
	return a.withReviewServerEndpoint(ctx, sandboxName, endpoint, owner, fn)
}

func (a Agent) withReviewServerEndpoint(ctx context.Context, sandboxName, endpoint string, owner incus.ReviewMetadata, fn func(*protocol) error) error {
	return a.withServerEndpointController(ctx, sandboxName, endpoint, true, func() error {
		// Re-attest after reconnect or process replacement. The authentication
		// token can rotate; only this exact host-owned Sandbox identity persists.
		if err := a.Sandbox.AttestReview(ctx, sandboxName, owner); err != nil {
			return err
		}
		return nil
	}, fn)
}

func (a Agent) withServerEndpoint(ctx context.Context, sandboxName, endpoint string, fn func(*protocol) error) error {
	return a.withServerEndpointController(ctx, sandboxName, endpoint, false, nil, fn)
}

func (a Agent) withServerEndpointController(ctx context.Context, sandboxName, endpoint string, reviewReadOnly bool, authorize func() error, fn func(*protocol) error) error {
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
			if authorize != nil {
				if err := authorize(); err != nil {
					return err
				}
			}
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
	launch := appServerScript(endpoint, tokenSHA256(token), reviewReadOnly)
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
			if authorize != nil {
				if err := authorize(); err != nil {
					return err
				}
			}
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

func appServerScript(endpoint, tokenDigest string, reviewReadOnly bool) string {
	configuration := ` -c 'approval_policy="never"'`
	if reviewReadOnly {
		configuration += ` -c 'sandbox_mode="read-only"'`
	}
	return "umask 077; install -d -m 700 " + serverControlDir + "; rm -f " + serverPIDPath + "; IFS= read -r DORF_PROVIDER_ROUTE_KEY < /root/.config/dorf/provider-route.key; export DORF_PROVIDER_ROUTE_KEY; nohup codex app-server" + configuration + " --listen " + endpoint + " --ws-auth " + serverAuthMode + " --ws-token-sha256 " + tokenDigest + " </dev/null >" + serverLogPath + " 2>&1 & printf '%s\\n' \"$!\" > " + serverPIDPath
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

func (p *protocol) listStrictReviewThreads(ctx context.Context, workspace string) ([]map[string]any, error) {
	result, err := p.call(ctx, "thread/list", map[string]any{"limit": 2, "cwd": workspace})
	if err != nil {
		return nil, err
	}
	if cursor, ok := result["nextCursor"].(string); ok && cursor != "" {
		return nil, reviewAttention("strict review thread discovery exceeded its bound")
	}
	data, ok := result["data"].([]any)
	if !ok {
		return nil, reviewAttention("strict review thread discovery omitted result.data")
	}
	if len(data) > 1 {
		return nil, reviewAttention(fmt.Sprintf("strict review recovery found %d competing threads", len(data)))
	}
	threads := make([]map[string]any, 0, len(data))
	for _, value := range data {
		thread, ok := value.(map[string]any)
		if !ok || strings.TrimSpace(stringValue(thread["id"])) == "" || stringValue(thread["cwd"]) != workspace {
			return nil, reviewAttention("strict review thread identity or cwd is missing or mismatched")
		}
		threads = append(threads, thread)
	}
	return threads, nil
}

func (p *protocol) startThread(ctx context.Context, workspace, model, capability string) (string, error) {
	result, err := p.call(ctx, "thread/start", map[string]any{"cwd": workspace, "model": model, "approvalPolicy": "never", "sandbox": capability})
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

func (p *protocol) startStrictReviewThread(ctx context.Context, workspace, model, effort string) (string, error) {
	result, err := p.call(ctx, "thread/start", map[string]any{"cwd": workspace, "model": model, "approvalPolicy": "never", "sandbox": "read-only"})
	if err != nil {
		return "", err
	}
	// Reasoning effort is a turn/start setting in the native protocol, so an
	// empty thread cannot attest it yet. The first persisted turn and every
	// recovery read below must expose the requested value.
	if err := validateReviewSettings(result, workspace, model, ""); err != nil {
		return "", err
	}
	thread, _ := result["thread"].(map[string]any)
	id := stringValue(thread["id"])
	if id == "" || stringValue(thread["cwd"]) != workspace {
		return "", reviewAttention("strict review thread/start returned a mismatched thread")
	}
	return id, nil
}

func (p *protocol) reconcileStrictReviewTurn(ctx context.Context, workspace, expectedSession, submissionNonce, input, model, effort string, allowStart bool) (string, TurnOutcome, error) {
	if len(submissionNonce) != 64 || strings.TrimSpace(input) == "" {
		return "", TurnOutcome{}, reviewAttention("strict review submission nonce or review Message is missing")
	}
	threads, err := p.listStrictReviewThreads(ctx, workspace)
	if err != nil {
		return "", TurnOutcome{}, err
	}
	var sessionID string
	fresh := false
	if len(threads) == 0 {
		if !allowStart || expectedSession != "" {
			return "", TurnOutcome{}, reviewVisibilityMissing("strict review bound thread is not yet visible")
		}
		sessionID, err = p.startStrictReviewThread(ctx, workspace, model, effort)
		if err != nil {
			return "", TurnOutcome{}, err
		}
		fresh = true
	} else {
		sessionID = stringValue(threads[0]["id"])
		if expectedSession != "" && sessionID != expectedSession {
			return "", TurnOutcome{}, reviewAttention("strict review recovery found the wrong thread")
		}
	}
	if !fresh {
		observedSession, turns, err := p.strictReviewSnapshot(ctx, workspace, sessionID, model, effort)
		if err != nil {
			return sessionID, TurnOutcome{}, err
		}
		if len(turns) > 1 {
			return sessionID, TurnOutcome{}, reviewAttention("strict review thread contains extra turns")
		}
		if len(turns) == 1 {
			if err := attestReviewTurn(turns[0], submissionNonce, input); err != nil {
				return sessionID, TurnOutcome{}, err
			}
			return observedSession, parseTurn(turns[0]), nil
		}
		if !allowStart {
			return sessionID, TurnOutcome{}, reviewVisibilityMissing("strict review bound native turn is not yet visible")
		}
	}
	turn, err := p.startTurn(ctx, sessionID, workspace, submissionNonce, input, model, effort, "read-only")
	if err != nil {
		return sessionID, TurnOutcome{}, err
	}
	return sessionID, turn, nil
}

func (p *protocol) strictReviewHistory(ctx context.Context, workspace, expectedSession, submissionNonce, input, model, effort string) (string, []TurnOutcome, error) {
	threads, err := p.listStrictReviewThreads(ctx, workspace)
	if err != nil {
		return "", nil, err
	}
	if len(threads) == 0 {
		return "", nil, reviewVisibilityMissing("strict review bound thread is not yet visible")
	}
	if len(threads) != 1 || stringValue(threads[0]["id"]) != expectedSession {
		return "", nil, reviewAttention("strict review recovery found a missing or competing thread")
	}
	sessionID, rawTurns, err := p.strictReviewSnapshot(ctx, workspace, expectedSession, model, effort)
	if err != nil {
		return "", nil, err
	}
	if len(rawTurns) == 0 {
		return sessionID, nil, reviewVisibilityMissing("strict review bound native turn is not yet visible")
	}
	if len(rawTurns) != 1 {
		return sessionID, nil, reviewAttention(fmt.Sprintf("strict review thread contains %d turns; exactly one is required", len(rawTurns)))
	}
	if err := attestReviewTurn(rawTurns[0], submissionNonce, input); err != nil {
		return sessionID, nil, err
	}
	return sessionID, []TurnOutcome{parseTurn(rawTurns[0])}, nil
}

func (p *protocol) strictReviewSnapshot(ctx context.Context, workspace, sessionID, model, effort string) (string, []map[string]any, error) {
	result, err := p.call(ctx, "thread/resume", map[string]any{
		"threadId":       sessionID,
		"cwd":            workspace,
		"approvalPolicy": "never",
		"sandbox":        "read-only",
	})
	if err != nil {
		return "", nil, err
	}
	if err := validateReviewSettings(result, workspace, model, ""); err != nil {
		return "", nil, err
	}
	thread, _ := result["thread"].(map[string]any)
	if stringValue(thread["id"]) != sessionID || stringValue(thread["cwd"]) != workspace {
		return "", nil, reviewAttention("strict review thread/resume returned a mismatched thread")
	}
	values, ok := thread["turns"].([]any)
	if !ok {
		return "", nil, reviewAttention("strict review thread omitted persisted turns")
	}
	turns := make([]map[string]any, 0, len(values))
	for _, value := range values {
		turn, ok := value.(map[string]any)
		if !ok || stringValue(turn["id"]) == "" {
			return "", nil, reviewAttention("strict review native turn identity is missing")
		}
		turns = append(turns, turn)
	}
	if len(turns) > 0 && stringValue(result["reasoningEffort"]) != effort {
		return "", nil, reviewAttention("strict review model, effort, cwd, approval, or read-only policy is missing or mismatched")
	}
	return sessionID, turns, nil
}

func validateReviewSettings(result map[string]any, workspace, model, effort string) error {
	sandbox, _ := result["sandbox"].(map[string]any)
	if stringValue(result["cwd"]) != workspace || stringValue(result["model"]) != model || stringValue(result["approvalPolicy"]) != "never" || effort != "" && stringValue(result["reasoningEffort"]) != effort || stringValue(sandbox["type"]) != "readOnly" {
		return reviewAttention("strict review model, effort, cwd, approval, or read-only policy is missing or mismatched")
	}
	if network, exists := sandbox["networkAccess"]; exists {
		allowed, ok := network.(bool)
		if !ok || allowed {
			return reviewAttention("strict review read-only policy unexpectedly exposes network access")
		}
	}
	return nil
}

func attestReviewTurn(turn map[string]any, submissionNonce, input string) error {
	items, ok := turn["items"].([]any)
	if !ok {
		return reviewAttention("strict review native turn omitted persisted user input")
	}
	matched := 0
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "userMessage" {
			continue
		}
		matched++
		if stringValue(item["clientId"]) != submissionNonce {
			return reviewAttention("strict review native turn has a missing or wrong client message identity")
		}
		content, ok := item["content"].([]any)
		if !ok || len(content) != 1 {
			return reviewAttention("strict review native turn has ambiguous persisted input")
		}
		text, ok := content[0].(map[string]any)
		if !ok || text["type"] != "text" || stringValue(text["text"]) != input {
			return reviewAttention("strict review turn prompt differs from the exact review Message")
		}
	}
	if matched != 1 {
		return reviewAttention("strict review native turn does not contain one exact persisted user message")
	}
	return nil
}

func reviewAttention(reason string) error { return &attentionError{reason: reason} }

func reviewVisibilityMissing(reason string) error { return &reviewVisibilityError{reason: reason} }

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func (p *protocol) reconcileInitialTurn(ctx context.Context, workspace, agentRunID, goal, model, effort, capability string) (string, TurnOutcome, error) {
	sessionID, turns, err := p.inspectInitialTurns(ctx, workspace)
	if err != nil {
		return "", TurnOutcome{}, err
	}
	if sessionID == "" {
		sessionID, err = p.startThread(ctx, workspace, model, capability)
		if err != nil {
			return "", TurnOutcome{}, err
		}
		turn, err := p.startTurn(ctx, sessionID, workspace, agentRunID, goal, model, effort, capability)
		return sessionID, turn, err
	}
	if len(turns) == 1 {
		return sessionID, turns[0], nil
	}
	turn, err := p.startTurn(ctx, sessionID, workspace, agentRunID, goal, model, effort, capability)
	return sessionID, turn, err
}

func (p *protocol) inspectInitialTurns(ctx context.Context, workspace string) (string, []TurnOutcome, error) {
	threads, err := p.listThreads(ctx, workspace)
	if err != nil {
		return "", nil, err
	}
	if len(threads) > 1 {
		return "", nil, &attentionError{reason: fmt.Sprintf("Codex reconciliation is ambiguous: isolated Sandbox contains %d threads", len(threads))}
	}
	if len(threads) == 0 {
		return "", nil, nil
	}
	turns, err := p.readTurns(ctx, threads[0])
	if err != nil {
		return "", nil, fmt.Errorf("inspect isolated thread before initial submit: %v", err)
	}
	if len(turns) > 1 {
		return "", nil, &attentionError{reason: fmt.Sprintf("Codex reconciliation is ambiguous: initial thread contains %d turns", len(turns))}
	}
	return threads[0], turns, nil
}

func (p *protocol) readTurns(ctx context.Context, sessionID string) ([]TurnOutcome, error) {
	result, err := p.call(ctx, "thread/read", map[string]any{"threadId": sessionID, "includeTurns": true})
	if err != nil {
		return nil, err
	}
	thread, _ := result["thread"].(map[string]any)
	if thread == nil || thread["id"] != sessionID {
		return nil, fmt.Errorf("thread/read did not return the bound thread")
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
		parsed := parseTurn(turn)
		if parsed.ID != "" {
			turns = append(turns, parsed)
		}
	}
	return turns, nil
}

func parseTurn(turn map[string]any) TurnOutcome {
	id, _ := turn["id"].(string)
	status, _ := turn["status"].(string)
	outcome := TurnOutcome{ID: id, Status: status}
	items, _ := turn["items"].([]any)
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if item["type"] == "userMessage" {
			if clientID := stringValue(item["clientId"]); clientID != "" {
				outcome.AcceptedMessageIDs = append(outcome.AcceptedMessageIDs, clientID)
			}
			continue
		}
		if item["type"] != "agentMessage" {
			continue
		}
		if text, ok := item["text"].(string); ok && text != "" {
			outcome.Output = text
			continue
		}
		if contents, ok := item["content"].([]any); ok {
			for _, raw := range contents {
				content, _ := raw.(map[string]any)
				if text, ok := content["text"].(string); ok && text != "" {
					outcome.Output = text
				}
			}
		}
	}
	return outcome
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

func (p *protocol) resumeAndStartTurn(ctx context.Context, sessionID, workspace, agentRunID, goal, model, effort, capability string) (TurnOutcome, error) {
	if err := p.resumeThread(ctx, sessionID); err != nil {
		return TurnOutcome{}, err
	}
	return p.startTurn(ctx, sessionID, workspace, agentRunID, goal, model, effort, capability)
}

func (p *protocol) startTurn(ctx context.Context, sessionID, workspace, agentRunID, goal, model, effort, capability string) (TurnOutcome, error) {
	policyType := "dangerFullAccess"
	if capability == "read-only" {
		policyType = "readOnly"
	}
	result, err := p.call(ctx, "turn/start", map[string]any{"threadId": sessionID, "clientUserMessageId": agentRunID, "input": []map[string]string{{"type": "text", "text": goal}}, "cwd": workspace, "model": model, "effort": effort, "approvalPolicy": "never", "sandboxPolicy": map[string]string{"type": policyType}})
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

func (p *protocol) steerTurn(ctx context.Context, sessionID, turnID, agentRunID, input string) (string, error) {
	if err := p.resumeThread(ctx, sessionID); err != nil {
		return "", err
	}
	result, err := p.call(ctx, "turn/steer", map[string]any{"threadId": sessionID, "expectedTurnId": turnID, "clientUserMessageId": agentRunID, "input": []map[string]string{{"type": "text", "text": input}}})
	if err != nil {
		return "", err
	}
	acceptedTurnID := stringValue(result["turnId"])
	if acceptedTurnID == "" || acceptedTurnID != turnID {
		return "", fmt.Errorf("turn/steer response did not acknowledge the exact active turn")
	}
	return acceptedTurnID, nil
}

func (p *protocol) pollTurn(ctx context.Context, sessionID, turnID string, outcome *TurnOutcome) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
	}
	turns, err := p.readTurns(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, turn := range turns {
		if turn.ID == turnID {
			*outcome = turn
			return nil
		}
	}
	return fmt.Errorf("bound turn %s is missing from thread history", turnID)
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
