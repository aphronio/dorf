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
	active       map[string]bool
	instructions map[string]instructionHashes
	wg           sync.WaitGroup
}

func NewObservations(ctx context.Context, emit func(telemetry.Event)) *Observations {
	ctx, cancel := context.WithCancel(ctx)
	return &Observations{ctx: ctx, cancel: cancel, emit: emit, active: make(map[string]bool), instructions: make(map[string]instructionHashes)}
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
	key        string
	subscribed bool
	complete   bool
}

func (p *protocol) bindObservation(threadID, turnID string, subscribed bool) {
	if p.observations == nil || p.observed != nil || p.execution.ID == "" || turnID == "" {
		return
	}
	key := p.execution.JobID + "/" + threadID + "/" + turnID
	p.observations.mu.Lock()
	defer p.observations.mu.Unlock()
	if p.observations.ctx.Err() != nil || p.observations.active[key] {
		return
	}
	p.observations.active[key] = true
	p.observations.wg.Add(1)
	p.observed = &observedTurn{run: p.execution, threadID: threadID, turnID: turnID, key: key, subscribed: subscribed}
	for _, message := range p.pendingObservations {
		p.observeNotification(message)
	}
	p.pendingObservations = nil
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
				p.emitObservation("codex.turn.snapshot", time.Now(), map[string]any{"status": turn.Status}, false)
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
	p.forgetWorkspaceInstructions()
	if p.observations.ctx.Err() == nil {
		p.emitObservation("codex.observation.disconnected", time.Now(), nil, true)
	}
}

func (p *protocol) observeNotification(message map[string]any) {
	if p.observations == nil {
		return
	}
	method := stringValue(message["method"])
	switch method {
	case "turn/started", "turn/completed", "item/started", "item/completed", "thread/tokenUsage/updated":
	default:
		return
	}
	if p.observed == nil {
		p.pendingObservations = append(p.pendingObservations, message)
		return
	}
	params, _ := message["params"].(map[string]any)
	threadID, turnID := stringValue(params["threadId"]), stringValue(params["turnId"])
	turn, _ := params["turn"].(map[string]any)
	if turn != nil {
		turnID = stringValue(turn["id"])
	}
	if threadID != p.observed.threadID || turnID != p.observed.turnID {
		return
	}
	at := time.Now()
	if emitted, ok := message["emittedAtMs"].(float64); ok {
		at = time.UnixMilli(int64(emitted))
	}
	fields := make(map[string]any)
	failed := false
	switch method {
	case "turn/started", "turn/completed":
		for _, key := range []string{"status", "error", "startedAt", "completedAt", "durationMs"} {
			if value := turn[key]; value != nil {
				fields[key] = value
			}
		}
		failed = stringValue(turn["status"]) == "failed"
		p.observed.complete = method == "turn/completed"
	case "item/started", "item/completed":
		item, _ := params["item"].(map[string]any)
		if stringValue(item["type"]) == "contextCompaction" {
			p.forgetWorkspaceInstructions()
		}
		fields, failed = itemObservation(item, method)
		if fields == nil {
			return
		}
	case "thread/tokenUsage/updated":
		fields["token_usage"] = params["tokenUsage"]
	}
	p.emitObservation("codex."+method, at, fields, failed)
}

func (p *protocol) forgetWorkspaceInstructions() {
	if p.instructionCache == nil || p.observed == nil {
		return
	}
	p.instructionCache.mu.Lock()
	defer p.instructionCache.mu.Unlock()
	delete(p.instructionCache.instructions, p.observed.threadID)
}

func (p *protocol) emitObservation(name string, at time.Time, fields map[string]any, failed bool) {
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
