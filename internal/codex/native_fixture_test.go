package codex

import (
	"context"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func fixtureMutation(inputID string, unused bool) core.NativeMutation {
	return core.NativeMutation{Unused: unused,
		Bind:  func(context.Context, string) error { return nil },
		Begin: func(context.Context, string) (string, error) { return inputID, nil },
	}
}
func (p *protocol) resumeFixture(ctx context.Context, thread, workspace, id string, input core.HarnessInput, model, effort, capability string) (TurnOutcome, error) {
	if err := p.resumeThread(ctx, thread); err != nil {
		return TurnOutcome{}, err
	}
	return p.startTurn(ctx, thread, workspace, id, input, model, effort, capability)
}
func (p *protocol) initialFixture(ctx context.Context, workspace, id string, input core.HarnessInput, model, effort, capability string) (string, TurnOutcome, error) {
	thread, err := p.startThread(ctx, workspace, model, capability)
	if err != nil {
		return "", TurnOutcome{}, err
	}
	p.freshThread = true
	turn, err := p.startTurn(ctx, thread, workspace, id, input, model, effort, capability)
	return thread, turn, err
}

func (a Agent) observeFixtureTurn(ctx context.Context, owner provider.Ownership, threadID, turnID string) (TurnOutcome, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	outcome := TurnOutcome{ID: turnID, Status: "running"}
	err := a.withServer(ctx, owner, func(protocol *protocol) error {
		return protocol.pollTurn(ctx, threadID, turnID, &outcome)
	})
	return outcome, err
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
