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
	job := core.Job{ID: "job"}
	owned := core.Sandbox{ID: "sandbox", JobID: job.ID, OwnershipNonce: "owned"}
	e := Externals{Agent: &attachmentHarness{}}
	if _, err := e.ReadTimeline(context.Background(), job, owned, "thread", "turn"); !errors.Is(err, core.ErrTimelineUnavailable) {
		t.Fatalf("unsupported harness error=%v", err)
	}
	native := &timelineHarness{}
	e.Agent = native
	result, err := e.ReadTimeline(context.Background(), job, owned, "thread", "turn")
	if err != nil || result.TurnID != "turn" || native.owner != (provider.Ownership{JobID: "job", SandboxID: "sandbox", OwnershipNonce: "owned"}) || native.threadID != "thread" || native.turnID != "turn" {
		t.Fatalf("result=%+v harness=%+v error=%v", result, native, err)
	}
	owned.JobID = "foreign"
	if _, err := e.ReadTimeline(context.Background(), job, owned, "thread", "turn"); !errors.Is(err, core.ErrTimelineUnavailable) {
		t.Fatalf("foreign sandbox error=%v", err)
	}
}
