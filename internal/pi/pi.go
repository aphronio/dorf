package pi

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/spine"
)

const (
	Harness    = "pi"
	sessionDir = "/root/.pi/agent/sessions/dorf"
	routeKey   = "/root/.config/dorf/provider-route.key"
	modelsFile = "/root/.pi/agent/models.json"
	rpcDir     = "/run/dorf/pi-rpc"
	rpcInput   = rpcDir + "/input"
	rpcEvents  = rpcDir + "/events.jsonl"
	rpcErrors  = rpcDir + "/stderr"
	rpcConfig  = rpcDir + "/config.sha256"
	rpcUnit    = "dorf-pi-rpc.service"
)

type Agent struct {
	Sandbox incus.Sandbox
	Timeout time.Duration
}

func (Agent) Name() string { return Harness }

func (a Agent) InstallRoute(ctx context.Context, sandboxName, baseURL, key, model string) error {
	config, err := json.Marshal(map[string]any{"providers": map[string]any{"dorf": map[string]any{
		"baseUrl": baseURL,
		"api":     "openai-responses",
		"apiKey":  "$DORF_PROVIDER_ROUTE_KEY",
		"models":  []map[string]any{{"id": model, "reasoning": true}},
	}}})
	if err != nil {
		return err
	}
	input := append(append(config, '\n'), []byte(key+"\n")...)
	script := "set -eu; umask 077; install -d -m 700 /root/.pi/agent /root/.config/dorf; IFS= read -r config; printf '%s\\n' \"$config\" > " + modelsFile + "; IFS= read -r key; printf '%s\\n' \"$key\" > " + routeKey
	result, err := a.Sandbox.Exec(ctx, sandboxName, input, "bash", "-lc", script)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("install Pi scoped provider route: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (a Agent) RemoveRoute(ctx context.Context, sandboxName string) error {
	script := "set -eu; systemctl stop " + rpcUnit + " >/dev/null 2>&1 || true; rm -f " + rpcInput + " " + rpcEvents + " " + rpcErrors + " " + rpcConfig + " " + routeKey + " " + modelsFile + "; rmdir " + rpcDir + " >/dev/null 2>&1 || true"
	result, err := a.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-lc", script)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("remove Pi scoped provider route: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (a Agent) StartInitialTurn(ctx context.Context, sandboxName, workspace, agentRunID string, input, model, effort string) (spine.HarnessBinding, error) {
	threadID := sandboxName
	if err := a.runTurn(ctx, sandboxName, workspace, threadID, agentRunID, input, model, effort, false); err != nil {
		return spine.HarnessBinding{}, err
	}
	return a.latestBinding(ctx, sandboxName, threadID)
}

func (a Agent) ReadInitialTurns(ctx context.Context, sandboxName, _ string) (spine.HarnessHistory, error) {
	return a.readHistory(ctx, sandboxName, sandboxName, true)
}

func (a Agent) ReadTurns(ctx context.Context, sandboxName, threadID string) (spine.HarnessHistory, error) {
	return a.readHistory(ctx, sandboxName, threadID, false)
}

func (a Agent) StartTurn(ctx context.Context, sandboxName, workspace, threadID, agentRunID string, input, model, effort string) (spine.HarnessBinding, error) {
	before, err := a.readHistory(ctx, sandboxName, threadID, false)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	if err := a.runTurn(ctx, sandboxName, workspace, threadID, agentRunID, input, model, effort, false); err != nil {
		return spine.HarnessBinding{}, err
	}
	after, err := a.readHistory(ctx, sandboxName, threadID, false)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	if len(after.Turns) != len(before.Turns)+1 {
		return spine.HarnessBinding{}, fmt.Errorf("Pi follow-up did not create exactly one native Turn")
	}
	return spine.HarnessBinding{Harness: Harness, ThreadID: threadID, Turn: after.Turns[len(after.Turns)-1]}, nil
}

func (a Agent) SteerTurn(ctx context.Context, sandboxName, _ string, targetTurnID, agentRunID, input string) (string, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	response, err := a.rpcCommand(ctx, sandboxName, agentRunID, "steer", input)
	if err != nil {
		return "", err
	}
	if !response.Success {
		return "", fmt.Errorf("Pi RPC steer rejected: %s", response.Error)
	}
	return targetTurnID, nil
}

func (a Agent) WaitTurn(ctx context.Context, sandboxName, threadID, turnID string) (spine.HarnessBinding, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	for {
		history, err := a.readHistory(ctx, sandboxName, threadID, false)
		if err != nil {
			return spine.HarnessBinding{}, err
		}
		found := false
		for _, turn := range history.Turns {
			if turn.ID != turnID {
				continue
			}
			found = true
			if turn.Status != "running" && turn.Status != "inProgress" {
				return spine.HarnessBinding{Harness: Harness, ThreadID: threadID, Turn: turn}, nil
			}
		}
		if !found {
			return spine.HarnessBinding{}, fmt.Errorf("Pi Thread %s has no Turn %s", threadID, turnID)
		}
		select {
		case <-ctx.Done():
			return spine.HarnessBinding{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (a Agent) StartStrictReviewTurn(ctx context.Context, sandboxName, workspace string, owner incus.ReviewMetadata, submissionNonce string, input, model, effort string) (spine.HarnessBinding, error) {
	if err := a.Sandbox.AttestReview(ctx, sandboxName, owner); err != nil {
		return spine.HarnessBinding{}, err
	}
	if err := a.runTurn(ctx, sandboxName, workspace, sandboxName, submissionNonce, input, model, effort, true); err != nil {
		return spine.HarnessBinding{}, err
	}
	if err := a.Sandbox.AttestReview(ctx, sandboxName, owner); err != nil {
		return spine.HarnessBinding{}, err
	}
	return a.latestBinding(ctx, sandboxName, sandboxName)
}

func (a Agent) RecoverStrictReviewTurn(ctx context.Context, sandboxName, _ string, owner incus.ReviewMetadata, _ string, _, _, _ string) (spine.HarnessBinding, error) {
	if err := a.Sandbox.AttestReview(ctx, sandboxName, owner); err != nil {
		return spine.HarnessBinding{}, err
	}
	return a.latestBinding(ctx, sandboxName, sandboxName)
}

func (a Agent) WaitStrictReviewTurn(ctx context.Context, sandboxName, _ string, owner incus.ReviewMetadata, threadID, turnID, _ string, _, _, _ string) (spine.HarnessBinding, error) {
	if err := a.Sandbox.AttestReview(ctx, sandboxName, owner); err != nil {
		return spine.HarnessBinding{}, err
	}
	return a.WaitTurn(ctx, sandboxName, threadID, turnID)
}

func (a Agent) latestBinding(ctx context.Context, sandboxName, threadID string) (spine.HarnessBinding, error) {
	history, err := a.readHistory(ctx, sandboxName, threadID, false)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	if len(history.Turns) == 0 {
		return spine.HarnessBinding{}, fmt.Errorf("Pi Thread %s contains no Turn", threadID)
	}
	return spine.HarnessBinding{Harness: Harness, ThreadID: threadID, Turn: history.Turns[len(history.Turns)-1]}, nil
}

func (a Agent) runTurn(ctx context.Context, sandboxName, workspace, threadID, requestID, input, model, effort string, readOnly bool) error {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	tools := "read,bash,edit,write,grep,find,ls"
	if readOnly {
		tools = "read,grep,find,ls"
	}
	before, err := a.readHistory(ctx, sandboxName, threadID, true)
	if err != nil {
		return err
	}
	if err := a.ensureRPC(ctx, sandboxName, workspace, threadID, model, effort, tools); err != nil {
		return err
	}
	response, err := a.rpcCommand(ctx, sandboxName, requestID, "prompt", input)
	if err != nil {
		return err
	}
	if !response.Success {
		return &rpcRejectionError{reason: "Pi RPC prompt rejected before submission: " + response.Error}
	}
	for {
		after, err := a.readHistory(ctx, sandboxName, threadID, true)
		if err != nil {
			return err
		}
		if len(after.Turns) == len(before.Turns)+1 {
			return nil
		}
		if len(after.Turns) > len(before.Turns)+1 {
			return fmt.Errorf("Pi RPC prompt created more than one native Turn")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (a Agent) ensureRPC(ctx context.Context, sandboxName, workspace, threadID, model, effort, tools string) error {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join([]string{workspace, threadID, model, effort, tools}, "\x00"))))
	script := `set -eu
root=$1; expected=$2; workspace=$3; thread=$4; model=$5; effort=$6; tools=$7
if systemctl is-active --quiet ` + rpcUnit + `; then
  test "$(cat ` + rpcConfig + `)" = "$expected"
  exit 0
fi
systemctl reset-failed ` + rpcUnit + ` >/dev/null 2>&1 || true
install -d -m 700 "$root"
rm -f ` + rpcInput + ` ` + rpcEvents + ` ` + rpcErrors + `
mkfifo -m 600 ` + rpcInput + `
: > ` + rpcEvents + `
: > ` + rpcErrors + `
printf '%s\n' "$expected" > ` + rpcConfig + `
systemd-run --unit=` + rpcUnit + ` --collect --quiet \
  bash -lc 'set -eu; workspace=$1; thread=$2; model=$3; effort=$4; tools=$5; root=$6; IFS= read -r DORF_PROVIDER_ROUTE_KEY < ` + routeKey + `; export DORF_PROVIDER_ROUTE_KEY; cd "$workspace"; exec 3<>"$root/input"; exec pi --offline --mode rpc --session-id "$thread" --session-dir ` + sessionDir + ` --provider dorf --model "$model" --thinking "$effort" --tools "$tools" --approve <&3 >>"$root/events.jsonl" 2>>"$root/stderr"' \
  dorf-pi-rpc "$workspace" "$thread" "$model" "$effort" "$tools" "$root"
for _ in $(seq 1 50); do
  systemctl is-active --quiet ` + rpcUnit + ` && exit 0
  sleep 0.1
done
cat ` + rpcErrors + ` >&2
exit 1`
	result, err := a.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-lc", script, "dorf-pi-rpc-start", rpcDir, digest, workspace, threadID, model, effort, tools)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("start Pi RPC controller: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

type rpcResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type rpcRejectionError struct{ reason string }

func (e *rpcRejectionError) Error() string        { return e.reason }
func (*rpcRejectionError) DefiniteNoSubmit() bool { return true }

func (a Agent) rpcCommand(ctx context.Context, sandboxName, requestID, command, message string) (rpcResponse, error) {
	request, err := json.Marshal(map[string]string{"id": requestID, "type": command, "message": message})
	if err != nil {
		return rpcResponse{}, err
	}
	request = append(request, '\n')
	result, err := a.Sandbox.Exec(ctx, sandboxName, request, "bash", "-lc", "set -eu; cat > "+rpcInput)
	if err != nil {
		return rpcResponse{}, err
	}
	if result.ExitCode != 0 {
		return rpcResponse{}, fmt.Errorf("send Pi RPC %s: %s", command, strings.TrimSpace(result.Stderr))
	}
	return a.waitRPCResponse(ctx, sandboxName, requestID, command)
}

func (a Agent) waitRPCResponse(ctx context.Context, sandboxName, requestID, command string) (rpcResponse, error) {
	for {
		result, err := a.Sandbox.Exec(ctx, sandboxName, nil, "cat", rpcEvents)
		if err != nil {
			return rpcResponse{}, err
		}
		if result.ExitCode != 0 {
			return rpcResponse{}, fmt.Errorf("read Pi RPC events: %s", strings.TrimSpace(result.Stderr))
		}
		scanner := bufio.NewScanner(strings.NewReader(result.Stdout))
		for scanner.Scan() {
			var response rpcResponse
			if json.Unmarshal(scanner.Bytes(), &response) != nil || response.Type != "response" || response.ID != requestID {
				continue
			}
			if response.Command != command {
				return rpcResponse{}, fmt.Errorf("Pi RPC request %s returned command %q", requestID, response.Command)
			}
			return response, nil
		}
		if err := scanner.Err(); err != nil {
			return rpcResponse{}, err
		}
		select {
		case <-ctx.Done():
			return rpcResponse{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (a Agent) readHistory(ctx context.Context, sandboxName, threadID string, allowMissing bool) (spine.HarnessHistory, error) {
	script := "set -eu; shopt -s nullglob; files=(" + sessionDir + "/*_\"$1\".jsonl); if (( ${#files[@]} == 0 )); then exit 0; fi; if (( ${#files[@]} != 1 )); then echo 'ambiguous Pi session identity' >&2; exit 2; fi; cat -- \"${files[0]}\""
	result, err := a.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-lc", script, "dorf-pi-history", threadID)
	if err != nil {
		return spine.HarnessHistory{}, err
	}
	if result.ExitCode != 0 {
		return spine.HarnessHistory{}, fmt.Errorf("inspect Pi Thread: %s", strings.TrimSpace(result.Stderr))
	}
	if strings.TrimSpace(result.Stdout) == "" {
		if allowMissing {
			return spine.HarnessHistory{Harness: Harness}, nil
		}
		return spine.HarnessHistory{}, fmt.Errorf("Pi Thread %s is missing", threadID)
	}
	observedThread, turns, err := parseSession(result.Stdout)
	if err != nil {
		return spine.HarnessHistory{}, err
	}
	if observedThread != threadID {
		return spine.HarnessHistory{}, fmt.Errorf("Pi returned Thread %s for %s", observedThread, threadID)
	}
	return spine.HarnessHistory{Harness: Harness, ThreadID: threadID, Turns: turns}, nil
}

func (a Agent) timeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.Timeout > 0 {
		return context.WithTimeout(ctx, a.Timeout)
	}
	return context.WithCancel(ctx)
}

type sessionEntry struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	ParentID *string         `json:"parentId"`
	Message  json.RawMessage `json:"message"`
}

type sessionMessage struct {
	Role         string `json:"role"`
	Content      any    `json:"content"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
}

func parseSession(raw string) (string, []spine.HarnessTurn, error) {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	threadID, previousID := "", ""
	turns := make([]spine.HarnessTurn, 0)
	for scanner.Scan() {
		var entry sessionEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return "", nil, fmt.Errorf("decode Pi session entry: %w", err)
		}
		if entry.Type == "session" {
			if threadID != "" || entry.ID == "" {
				return "", nil, fmt.Errorf("Pi session has an invalid header")
			}
			threadID = entry.ID
			continue
		}
		if threadID == "" || entry.ID == "" || (previousID == "" && entry.ParentID != nil) || (previousID != "" && (entry.ParentID == nil || *entry.ParentID != previousID)) {
			return "", nil, fmt.Errorf("Pi session is not one linear native Thread")
		}
		previousID = entry.ID
		if entry.Type != "message" {
			continue
		}
		var message sessionMessage
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			return "", nil, fmt.Errorf("decode Pi session message: %w", err)
		}
		switch message.Role {
		case "user":
			if len(turns) > 0 && turns[len(turns)-1].Status == "running" {
				return "", nil, fmt.Errorf("Pi session starts a new Turn before the prior Turn settled")
			}
			turns = append(turns, spine.HarnessTurn{ID: entry.ID, Status: "running"})
		case "assistant":
			if len(turns) == 0 {
				return "", nil, fmt.Errorf("Pi session contains an assistant response without a Turn")
			}
			turn := &turns[len(turns)-1]
			switch message.StopReason {
			case "stop":
				turn.Status, turn.Output = "completed", messageText(message.Content)
			case "length", "error":
				turn.Status = "failed"
			case "aborted":
				turn.Status = "interrupted"
			case "toolUse", "":
			default:
				return "", nil, fmt.Errorf("Pi Turn has unknown stop reason %q", message.StopReason)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, err
	}
	if threadID == "" {
		return "", nil, fmt.Errorf("Pi session has no header")
	}
	return threadID, turns, nil
}

func messageText(content any) string {
	blocks, ok := content.([]any)
	if !ok {
		text, _ := content.(string)
		return text
	}
	var parts []string
	for _, block := range blocks {
		value, ok := block.(map[string]any)
		if ok && value["type"] == "text" {
			if text, ok := value["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "")
}
