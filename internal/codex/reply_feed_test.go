package codex

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestReplyFeedBuffersCompletedItemsInNativeStartOrder(t *testing.T) {
	observations := NewObservations(context.Background(), nil)
	p := &protocol{observations: observations, owner: provider.Ownership{JobID: "job", SandboxID: "sandbox", OwnershipNonce: "nonce"}, observed: &observedTurn{threadID: "thread", turnID: "turn", subscribed: true}}
	binding := p.replyBinding()
	observations.Replies.Begin(binding, true)
	event := func(method, id, text string) {
		p.observeReply(method, map[string]any{"item": map[string]any{"id": id, "type": "agentMessage", "phase": "final_answer", "text": text}})
	}
	event("item/started", "first", "")
	event("item/started", "second", "")
	event("item/completed", "second", "same answer")
	snapshot, _, _ := observations.Replies.Read(binding)
	if len(snapshot.Items) != 0 {
		t.Fatal("published second reply before its native predecessor")
	}
	event("item/completed", "first", "same answer")
	snapshot, _, _ = observations.Replies.Read(binding)
	if len(snapshot.Items) != 2 || snapshot.Items[0].NativeItemID != "first" || snapshot.Items[1].Index != 1 {
		t.Fatalf("wrong prefix: %+v", snapshot)
	}
	event("item/completed", "first", "different")
	snapshot, _, _ = observations.Replies.Read(binding)
	if !snapshot.Gap {
		t.Fatal("conflicting duplicate completion was ignored")
	}
}

func TestReplyFeedRequiresRecoverySeedAndIgnoresChangedDiagnosticIDs(t *testing.T) {
	feed := NewReplyFeed()
	binding := ReplyBinding{JobID: "job", ThreadID: "thread", TurnID: "turn"}
	feed.Begin(binding, false)
	feed.Append(binding, core.HarnessConversationItem{NativeItemID: "late", Kind: "reply", Text: "late"})
	snapshot, _, _ := feed.Read(binding)
	if !snapshot.Gap || len(snapshot.Items) != 0 {
		t.Fatal("recovery appended without authoritative prefix")
	}
	items := []core.HarnessConversationItem{{Index: 0, NativeItemID: "old", Kind: "reply", Text: "answer"}}
	feed.Seed(binding, items, false)
	items[0].NativeItemID = "new"
	feed.Seed(binding, items, true)
	snapshot, _, _ = feed.Read(binding)
	if snapshot.Gap || !snapshot.Complete || len(snapshot.Items) != 1 || snapshot.Items[0].NativeItemID != "new" {
		t.Fatalf("diagnostic ID changed prefix: %+v", snapshot)
	}
	items[0].Text = "rewritten"
	feed.Seed(binding, items, true)
	snapshot, _, _ = feed.Read(binding)
	if !snapshot.Gap {
		t.Fatal("rewritten prefix was accepted")
	}
}

func TestReplyFeedGlobalBoundEvictionWakesOldReader(t *testing.T) {
	feed := NewReplyFeed()
	first := ReplyBinding{JobID: "first"}
	second := ReplyBinding{JobID: "second"}
	items := []core.HarnessConversationItem{{NativeItemID: "reply", Kind: "reply", Text: strings.Repeat("x", replyFeedMaxBytes/2+1)}}
	feed.Seed(first, items, true)
	_, changed, _ := feed.Read(first)
	feed.Seed(second, items, true)
	if _, _, found := feed.Read(first); found {
		t.Fatal("global byte cap retained old turn")
	}
	select {
	case <-changed:
	default:
		t.Fatal("evicted reader was not notified")
	}
}

func TestReplyPendingBufferOverflowDropsRawItems(t *testing.T) {
	observations := NewObservations(context.Background(), nil)
	p := &protocol{observations: observations, observed: &observedTurn{threadID: "thread", turnID: "turn", replyBytes: replyFeedMaxBytes - 1, replyItems: map[string]json.RawMessage{"item": json.RawMessage(`{}`)}, replyOrder: []string{"item"}}}
	if p.reserveReplyBytes(2) || !p.observed.replyOverflow || p.observed.replyItems != nil || p.observed.replyOrder != nil {
		t.Fatal("overflow retained unbounded native buffers")
	}
	snapshot, _, _ := observations.Replies.Read(p.replyBinding())
	if !snapshot.Gap {
		t.Fatal("overflow did not report gap")
	}
}

func TestRecoveredCompletionKeepsCachedPrefixReadableUntilReconciled(t *testing.T) {
	observations := NewObservations(context.Background(), nil)
	p := &protocol{observations: observations, observed: &observedTurn{threadID: "thread", turnID: "turn"}}
	binding := p.replyBinding()
	observations.Replies.Seed(binding, []core.HarnessConversationItem{{Index: 0, NativeItemID: "old", Kind: "reply", Text: "answer"}}, false)
	p.observeReply("item/completed", map[string]any{"item": map[string]any{"id": "new", "type": "agentMessage", "phase": "final_answer", "text": "next"}})
	snapshot, _, _ := observations.Replies.Read(binding)
	if !p.observed.replyRefresh || snapshot.Gap || len(snapshot.Items) != 1 {
		t.Fatalf("cached prefix while refresh pending: refresh=%v snapshot=%+v", p.observed.replyRefresh, snapshot)
	}
}

func TestRecoveredReplyEventsReconcileOnSameConnectionAndPreserveInflightNotifications(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		send := func(value any) {
			raw, _ := json.Marshal(value)
			_ = conn.Write(r.Context(), websocket.MessageText, raw)
		}
		reads := 0
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request map[string]any
			if json.Unmarshal(raw, &request) != nil {
				return
			}
			if request["id"] == nil {
				continue
			}
			if request["method"] == "initialize" {
				send(map[string]any{"id": request["id"], "result": map[string]any{}})
				continue
			}
			if request["method"] != "thread/turns/list" {
				t.Errorf("unexpected recovered read %v", request["method"])
				return
			}
			reads++
			items := []any{map[string]any{"id": "cold-old", "type": "agentMessage", "phase": "final_answer", "text": "old reply"}}
			status := "inProgress"
			if reads == 1 {
				// Completion arrives while the first snapshot is already being read.
				send(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": map[string]any{"id": "live-new", "type": "agentMessage", "phase": "final_answer", "text": "new reply"}}})
			} else {
				items = append(items, map[string]any{"id": "cold-new", "type": "agentMessage", "phase": "final_answer", "text": "new reply"})
			}
			if reads == 3 {
				status = "completed"
				send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": status}}})
			}
			send(map[string]any{"id": request["id"], "result": map[string]any{"data": []any{map[string]any{"id": "turn", "status": status, "itemsView": "full", "items": items}}, "nextCursor": nil}})
		}
	}))
	defer server.Close()
	p := dialTestProtocol(t, server)
	p.observations = NewObservations(context.Background(), nil)
	p.execution = core.AgentRun{ID: "run"}
	p.observed = &observedTurn{threadID: "thread", turnID: "turn"}
	p.observations.Replies.Seed(p.replyBinding(), []core.HarnessConversationItem{{Index: 0, NativeItemID: "live-old", Kind: "reply", Text: "old reply"}}, false)
	if !p.refreshReplyPrefix() || !p.observed.replyRefresh {
		t.Fatal("completion during snapshot was lost")
	}
	intermediate, _, _ := p.observations.Replies.Read(p.replyBinding())
	if intermediate.Gap {
		t.Fatal("recovered completion exposed a transient gap before reconciliation")
	}
	if !p.refreshReplyPrefix() || p.observed.replyRefresh {
		t.Fatal("event-triggered follow-up did not settle")
	}
	snapshot, _, _ := p.observations.Replies.Read(p.replyBinding())
	if snapshot.Gap || snapshot.Complete || len(snapshot.Items) != 2 || snapshot.Items[1].Index != 1 {
		t.Fatalf("recovered reply waited for terminal or duplicated prefix: %+v", snapshot)
	}
	if !p.refreshReplyPrefix() || !p.observed.complete || !p.observed.replySettled {
		t.Fatal("turn completion during hydration was lost")
	}
	snapshot, _, _ = p.observations.Replies.Read(p.replyBinding())
	if !snapshot.Complete || len(snapshot.Items) != 2 {
		t.Fatalf("terminal prefix=%+v", snapshot)
	}
}
