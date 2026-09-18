package terminal

import (
	"context"
	"errors"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type timelineHarness struct {
	Harness
	owner            provider.Ownership
	threadID, turnID string
}

func (h *timelineHarness) ReadTimeline(_ context.Context, owner provider.Ownership, threadID, turnID string) (core.HarnessTimeline, error) {
	h.owner, h.threadID, h.turnID = owner, threadID, turnID
	return core.HarnessTimeline{Harness: "codex", ThreadID: threadID, TurnID: turnID}, nil
}

func TestTimelineUsesOptionalHarnessAndExactSandboxOwnership(t *testing.T) {
	session := core.Session{ID: "session"}
	owned := core.Sandbox{ID: "sandbox", SessionID: session.ID, OwnershipNonce: "owned"}
	e := Externals{Agent: &attachmentHarness{}}
	if _, err := e.ReadTimeline(context.Background(), session, owned, "thread", "turn"); !errors.Is(err, core.ErrTimelineUnavailable) {
		t.Fatalf("unsupported harness error=%v", err)
	}
	native := &timelineHarness{}
	e.Agent = native
	result, err := e.ReadTimeline(context.Background(), session, owned, "thread", "turn")
	if err != nil || result.TurnID != "turn" || native.owner != (provider.Ownership{SessionID: "session", SandboxID: "sandbox", OwnershipNonce: "owned"}) || native.threadID != "thread" || native.turnID != "turn" {
		t.Fatalf("result=%+v harness=%+v error=%v", result, native, err)
	}
	owned.SessionID = "foreign"
	if _, err := e.ReadTimeline(context.Background(), session, owned, "thread", "turn"); !errors.Is(err, core.ErrTimelineUnavailable) {
		t.Fatalf("foreign sandbox error=%v", err)
	}
}
