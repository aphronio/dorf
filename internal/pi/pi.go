package pi

import (
	"bufio"
	"context"
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
	result, err := a.Sandbox.Exec(ctx, sandboxName, nil, "rm", "-f", routeKey, modelsFile)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("remove Pi scoped provider route: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (a Agent) StartInitialTurn(ctx context.Context, sandboxName, workspace, _ string, input, model, effort string) (spine.HarnessBinding, error) {
	threadID := sandboxName
	if err := a.runTurn(ctx, sandboxName, workspace, threadID, input, model, effort, false); err != nil {
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

func (a Agent) StartTurn(ctx context.Context, sandboxName, workspace, threadID, _ string, input, model, effort string) (spine.HarnessBinding, error) {
	before, err := a.readHistory(ctx, sandboxName, threadID, false)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	if err := a.runTurn(ctx, sandboxName, workspace, threadID, input, model, effort, false); err != nil {
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

func (Agent) SteerTurn(context.Context, string, string, string, string, string) (string, error) {
	return "", fmt.Errorf("Pi active-Turn steering is not yet supported")
}

func (a Agent) WaitTurn(ctx context.Context, sandboxName, threadID, turnID string) (spine.HarnessBinding, error) {
	history, err := a.readHistory(ctx, sandboxName, threadID, false)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	for _, turn := range history.Turns {
		if turn.ID == turnID {
			return spine.HarnessBinding{Harness: Harness, ThreadID: threadID, Turn: turn}, nil
		}
	}
	return spine.HarnessBinding{}, fmt.Errorf("Pi Thread %s has no Turn %s", threadID, turnID)
}

func (a Agent) StartStrictReviewTurn(ctx context.Context, sandboxName, workspace string, owner incus.ReviewMetadata, _ string, input, model, effort string) (spine.HarnessBinding, error) {
	if err := a.Sandbox.AttestReview(ctx, sandboxName, owner); err != nil {
		return spine.HarnessBinding{}, err
	}
	if err := a.runTurn(ctx, sandboxName, workspace, sandboxName, input, model, effort, true); err != nil {
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

func (a Agent) runTurn(ctx context.Context, sandboxName, workspace, threadID, input, model, effort string, readOnly bool) error {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	tools := "read,bash,edit,write,grep,find,ls"
	if readOnly {
		tools = "read,grep,find,ls"
	}
	script := "set -eu; IFS= read -r DORF_PROVIDER_ROUTE_KEY < " + routeKey + "; export DORF_PROVIDER_ROUTE_KEY; cd \"$1\"; exec pi --offline --mode json --session-id \"$2\" --session-dir " + sessionDir + " --provider dorf --model \"$3\" --thinking \"$4\" --tools \"$5\" --approve"
	result, err := a.Sandbox.Exec(ctx, sandboxName, []byte(input), "bash", "-lc", script, "dorf-pi", workspace, threadID, model, effort, tools)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("run Pi Turn: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
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
