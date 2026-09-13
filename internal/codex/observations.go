package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/coder/websocket"
)

// Observations keeps an already-authenticated subscription after a control
// operation returns. It never submits work or decides a Message's outcome.
type Observations struct {
	ctx          context.Context
	cancel       context.CancelFunc
	emit         func(telemetry.Event)
	mu           sync.Mutex
	active       map[observationKey]bool
	instructions map[instructionScope]instructionHashes
	wg           sync.WaitGroup
}

func NewObservations(ctx context.Context, emit func(telemetry.Event)) *Observations {
	ctx, cancel := context.WithCancel(ctx)
	return &Observations{ctx: ctx, cancel: cancel, emit: emit, active: make(map[observationKey]bool), instructions: make(map[instructionScope]instructionHashes)}
}

type instructionScope struct {
	jobID, sandboxID, threadID string
}

type observationKey struct {
	scope  instructionScope
	turnID string
}

func (p *protocol) instructionScope(threadID string) instructionScope {
	return instructionScope{jobID: p.owner.JobID, sandboxID: p.owner.SandboxID, threadID: threadID}
}

func (o *Observations) Close() {
	o.mu.Lock()
	o.cancel()
	o.mu.Unlock()
	o.wg.Wait()
}

type observedTurn struct {
	run        core.AgentRun
	threadID   string
	turnID     string
	key        observationKey
	subscribed bool
	complete   bool
}

func (p *protocol) bindObservation(threadID, turnID string, subscribed bool) bool {
	if p.observations == nil || p.observed != nil || p.execution.ID == "" || turnID == "" {
		return false
	}
	key := observationKey{scope: p.instructionScope(threadID), turnID: turnID}
	p.observations.mu.Lock()
	if p.observations.ctx.Err() != nil || p.observations.active[key] {
		p.observations.mu.Unlock()
		return false
	}
	p.observations.active[key] = true
	p.observations.wg.Add(1)
	p.observed = &observedTurn{run: p.execution, threadID: threadID, turnID: turnID, key: key, subscribed: subscribed}
	if subscribed && p.instructions != nil {
		p.observations.instructions[key.scope] = p.instructions.hashes
	}
	p.observations.mu.Unlock()
	for _, message := range p.pendingObservations {
		p.observeNotification(message)
	}
	p.pendingObservations = nil
	return true
}

func (p *protocol) finish() {
	if p.observed == nil {
		p.connection.Close(websocket.StatusNormalClosure, "done")
		return
	}
	o := p.observations
	go func() {
		defer o.wg.Done()
		defer p.connection.CloseNow()
		defer func() {
			o.mu.Lock()
			delete(o.active, p.observed.key)
			o.mu.Unlock()
		}()
		p.observeUntilSettled()
	}()
}

func (p *protocol) observeUntilSettled() {
	if !p.observed.subscribed && !p.observed.complete {
		if err := p.resumeThread(p.observations.ctx, p.observed.threadID); err != nil {
			p.observationGap()
			return
		}
		turns, err := p.readTurns(p.observations.ctx, p.observed.threadID)
		if err != nil {
			p.observationGap()
			return
		}
		for _, turn := range turns {
			if turn.ID == p.observed.turnID && turn.Terminal() {
				if p.observations.emit != nil {
					p.emitObservation("codex.turn.snapshot", time.Now(), map[string]any{"status": turn.Status}, false)
				}
				return
			}
		}
	}
	for !p.observed.complete {
		if _, err := p.receiveRaw(p.observations.ctx); err != nil {
			p.observationGap()
			return
		}
	}
}

func (p *protocol) observationGap() {
	p.forgetWorkspaceInstructions(p.observed.threadID)
	if p.observations.ctx.Err() == nil {
		p.emitObservation("codex.observation.disconnected", time.Now(), nil, true)
	}
}

func (p *protocol) observeNotification(message map[string]any) {
	if p.observations == nil || p.execution.ID == "" {
		return
	}
	method := stringValue(message["method"])
	params, _ := message["params"].(map[string]any)
	compaction := false
	switch method {
	case "item/started", "item/completed":
		item, _ := params["item"].(map[string]any)
		compaction = stringValue(item["type"]) == "contextCompaction"
	case "turn/started", "turn/completed", "thread/tokenUsage/updated":
	default:
		return
	}
	if p.observations.emit == nil && method != "turn/completed" && !compaction {
		return
	}
	if p.observed == nil {
		p.pendingObservations = append(p.pendingObservations, message)
		return
	}
	threadID, turnID := stringValue(params["threadId"]), stringValue(params["turnId"])
	turn, _ := params["turn"].(map[string]any)
	if turn != nil {
		turnID = stringValue(turn["id"])
	}
	if threadID != p.observed.threadID || turnID != p.observed.turnID {
		return
	}
	if method == "turn/completed" {
		p.observed.complete = true
	}
	if compaction {
		p.forgetWorkspaceInstructions(threadID)
	}
	if p.observations.emit == nil {
		return
	}
	p.emitNativeNotification(message, method, params)
}

func (p *protocol) emitNativeNotification(message map[string]any, method string, params map[string]any) {
	at := time.Now()
	if emitted, ok := message["emittedAtMs"].(float64); ok {
		at = time.UnixMilli(int64(emitted))
	}
	fields := make(map[string]any)
	failed := false
	switch method {
	case "turn/started", "turn/completed":
		turn, _ := params["turn"].(map[string]any)
		for _, key := range []string{"status", "error", "startedAt", "completedAt", "durationMs"} {
			if value := turn[key]; value != nil {
				fields[key] = value
			}
		}
		failed = stringValue(turn["status"]) == "failed"
	case "item/started", "item/completed":
		item, _ := params["item"].(map[string]any)
		fields, failed = itemObservation(item, method)
		if fields == nil {
			return
		}
	case "thread/tokenUsage/updated":
		fields["token_usage"] = params["tokenUsage"]
	}
	p.emitObservation("codex."+method, at, fields, failed)
}

func (p *protocol) forgetWorkspaceInstructions(threadID string) {
	if p.observations == nil {
		return
	}
	p.observations.mu.Lock()
	defer p.observations.mu.Unlock()
	delete(p.observations.instructions, p.instructionScope(threadID))
}

func (p *protocol) emitObservation(name string, at time.Time, fields map[string]any, failed bool) {
	if p.observations.emit == nil {
		return
	}
	attributes := map[string]any{
		"dorf.job_id": p.observed.run.JobID, "dorf.message_id": p.observed.run.MessageID,
		"dorf.agent_run_id": p.observed.run.ID, "native.thread_id": p.observed.threadID,
		"native.turn_id": p.observed.turnID,
	}
	for key, value := range fields {
		attributes[key] = value
	}
	canonical, _ := json.Marshal(attributes)
	digest := sha256.Sum256(append([]byte(name), canonical...))
	attributes["event.id"] = hex.EncodeToString(digest[:])
	p.observations.emit(telemetry.Event{Name: name, At: at, Attributes: attributes, Failed: failed})
}

func itemObservation(item map[string]any, method string) (map[string]any, bool) {
	if stringValue(item["id"]) == "" {
		return nil, false
	}
	switch stringValue(item["type"]) {
	case "userMessage", "agentMessage":
		if method != "item/completed" {
			return nil, false
		}
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "webSearch", "imageView", "imageGeneration", "collabAgentToolCall", "contextCompaction":
	default:
		return nil, false
	}
	exitCode, _ := item["exitCode"].(float64)
	return map[string]any{"item": item}, stringValue(item["status"]) == "failed" || exitCode != 0
}
