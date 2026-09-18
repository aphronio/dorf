package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"path"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

// The manifest travels with the captured workspace. It contains no transcript
// and is used only to verify a backup or package transition, never to run input.
const continuityFile = ".dorf/native-continuity.json"

func (a Agent) captureContinuity(ctx context.Context, owner provider.Ownership, p *protocol, threadID string) ([]retainedTurn, error) {
	if threadID == "" {
		return nil, nil
	}
	turns, err := p.readTurns(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if len(turns) == 0 {
		return nil, fmt.Errorf("bound native Thread has no persisted work")
	}
	expected := make([]retainedTurn, 0, len(turns))
	for _, turn := range turns {
		if !turn.Terminal() {
			return nil, fmt.Errorf("native Thread is not quiescent")
		}
		expected = append(expected, retainedTurn{ThreadID: threadID, TurnID: turn.ID, TurnOutcome: turn.Status})
	}
	raw, err := json.Marshal(expected)
	if err != nil {
		return nil, err
	}
	if len(raw) > provider.MaxFileWriteBytes {
		return nil, fmt.Errorf("native continuity proof exceeds its bound")
	}
	name := path.Join(a.Sandbox.Workspace(), continuityFile)
	if err := provider.WriteFileViaExec(ctx, owner, a.Sandbox.Workspace(), name, raw, false, a.Sandbox.Exec); err != nil {
		return nil, err
	}
	return expected, nil
}

func (a Agent) readContinuity(ctx context.Context, owner provider.Ownership, threadID string) ([]retainedTurn, error) {
	raw, err := a.Sandbox.ReadFile(ctx, owner, continuityFile)
	if err != nil {
		return nil, err
	}
	var expected []retainedTurn
	if json.Unmarshal(raw, &expected) != nil || len(expected) == 0 {
		return nil, fmt.Errorf("native continuity proof is missing")
	}
	seen := map[string]bool{}
	for _, turn := range expected {
		if turn.ThreadID != threadID || turn.TurnID == "" || seen[turn.TurnID] || !terminal(turn.TurnOutcome) {
			return nil, fmt.Errorf("native continuity proof does not match the bound Thread")
		}
		seen[turn.TurnID] = true
	}
	return expected, nil
}
