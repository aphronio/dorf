package controlreader

import (
	"context"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"testing"
)

func TestNativeObservationRetainsCursorAndTerminalPrefix(t *testing.T) {
	session := core.Session{ID: "session", AdmissionOpen: true, CleanupState: core.CleanupPending, Harness: "codex", ThreadID: "thread"}
	owned := core.Sandbox{ID: core.MainSandboxName(session.ID), SessionID: session.ID, OwnershipNonce: "nonce"}
	feed := codex.NewReplyFeed()
	service := Service{Store: &readerTestStore{session: session, sandbox: owned}, Replies: feed}
	binding := codex.ReplyBinding{SessionID: session.ID, SandboxID: owned.ID, OwnershipNonce: "nonce", Harness: "codex", ThreadID: "thread", TurnID: "turn"}
	request := observationRequest{SessionID: session.ID, TurnID: "turn"}
	missing, _, err := service.turnObservation(context.Background(), request, false)
	if err != nil || missing.State != "resync_deferred" {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
	items := []core.HarnessConversationItem{{Index: 0, NativeItemID: "input", Kind: "input", ClientID: "client/dispatch"}}
	feed.Seed(binding, items, false)
	feed.Status(binding, "inProgress")
	first, _, err := service.turnObservation(context.Background(), request, false)
	if err != nil || len(first.Items) != 1 || first.Items[0].ClientID != "client" || first.Cursor == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	request.Cursor = *first.Cursor
	feed.Status(binding, "completed")
	early, _, err := service.turnObservation(context.Background(), request, false)
	if err != nil || early.State == "complete" {
		t.Fatalf("native completion without settled prefix=%+v err=%v", early, err)
	}
	items = append(items, core.HarnessConversationItem{Index: 1, NativeItemID: "reply", Kind: "reply", Text: "done"})
	feed.Seed(binding, items, true)
	feed.Status(binding, "completed")
	done, _, err := service.turnObservation(context.Background(), request, false)
	if err != nil || done.State != "complete" || done.CompletionWatermark == nil || *done.CompletionWatermark != 2 || len(done.Items) != 1 {
		t.Fatalf("done=%+v err=%v", done, err)
	}
	// Cache loss is explicit, and rewritten history cannot silently advance a cursor.
	replacement := codex.NewReplyFeed()
	service.Replies = replacement
	items[0].ClientID = "different/dispatch"
	replacement.Seed(binding, items, true)
	rewritten, _, err := service.turnObservation(context.Background(), request, false)
	if err != nil || rewritten.State != "gap" {
		t.Fatalf("rewritten=%+v err=%v", rewritten, err)
	}
	request.TurnID = "successor"
	if _, _, err := service.turnObservation(context.Background(), request, false); err == nil {
		t.Fatal("cursor crossed native Turns")
	}
}
