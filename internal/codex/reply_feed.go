package codex

import (
	"encoding/json"
	"reflect"
	"sync"

	"github.com/aphronio/dorf/internal/core"
)

const replyFeedMaxTurns = 128
const replyFeedMaxBytes = 16 << 20

// ReplyBinding includes custody, not just native diagnostic identities.
type ReplyBinding struct {
	JobID, SandboxID, OwnershipNonce, Harness, ThreadID, TurnID string
}

type ReplySnapshot struct {
	Items    []core.HarnessConversationItem
	Complete bool
	Gap      bool
}

type replyEntry struct {
	snapshot ReplySnapshot
	bytes    int
	changed  chan struct{}
}

// ReplyFeed is bounded ephemeral projection state. It owns no native connection,
// persistent transcript, execution outcome, or subscriber activity lease.
type ReplyFeed struct {
	mu      sync.Mutex
	entries map[ReplyBinding]*replyEntry
	order   []ReplyBinding
	changed chan struct{}
}

func NewReplyFeed() *ReplyFeed {
	return &ReplyFeed{entries: make(map[ReplyBinding]*replyEntry), changed: make(chan struct{})}
}

func (f *ReplyFeed) Read(binding ReplyBinding) (ReplySnapshot, <-chan struct{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[binding]
	if !ok {
		return ReplySnapshot{}, f.changed, false
	}
	snapshot := entry.snapshot
	snapshot.Items = append([]core.HarnessConversationItem{}, snapshot.Items...)
	return snapshot, entry.changed, true
}

func (f *ReplyFeed) entry(binding ReplyBinding) *replyEntry {
	if entry := f.entries[binding]; entry != nil {
		return entry
	}
	if len(f.order) == replyFeedMaxTurns {
		old := f.order[0]
		close(f.entries[old].changed)
		delete(f.entries, old)
		f.order = f.order[1:]
	}
	entry := &replyEntry{snapshot: ReplySnapshot{Items: []core.HarnessConversationItem{}}, changed: make(chan struct{})}
	f.entries[binding] = entry
	f.order = append(f.order, binding)
	close(f.changed)
	f.changed = make(chan struct{})
	return entry
}

func notifyReply(entry *replyEntry) { close(entry.changed); entry.changed = make(chan struct{}) }

func (f *ReplyFeed) Gap(binding ReplyBinding) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.entry(binding)
	entry.snapshot.Gap = true
	notifyReply(entry)
}

func (f *ReplyFeed) Append(binding ReplyBinding, item core.HarnessConversationItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.entry(binding)
	if entry.snapshot.Gap || entry.snapshot.Complete {
		return
	}
	for _, previous := range entry.snapshot.Items {
		if previous.NativeItemID == item.NativeItemID {
			item.Index = previous.Index
			if previous != item {
				entry.snapshot.Gap = true
				notifyReply(entry)
			}
			return
		}
	}
	item.Index = len(entry.snapshot.Items)
	raw, _ := json.Marshal(item)
	entry.bytes += len(raw)
	if entry.bytes > replyFeedMaxBytes {
		entry.snapshot.Gap = true
	} else {
		entry.snapshot.Items = append(entry.snapshot.Items, item)
	}
	f.trim(binding)
	notifyReply(entry)
}

// Seed accepts only the exact full-history projection. Native IDs may change on
// reconnect; stable completed positions, kind, text, and input origin may not.
func (f *ReplyFeed) Seed(binding ReplyBinding, items []core.HarnessConversationItem, complete bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.entry(binding)
	raw, _ := json.Marshal(items)
	valid := items != nil && len(raw) <= replyFeedMaxBytes && len(items) >= len(entry.snapshot.Items)
	for i, old := range entry.snapshot.Items {
		if i >= len(items) {
			break
		}
		next := items[i]
		old.NativeItemID, next.NativeItemID = "", ""
		if !reflect.DeepEqual(old, next) {
			valid = false
		}
	}
	if !valid {
		entry.snapshot.Gap = true
		notifyReply(entry)
		return
	}
	entry.snapshot = ReplySnapshot{Items: append([]core.HarnessConversationItem{}, items...), Complete: complete}
	entry.bytes = len(raw)
	f.trim(binding)
	notifyReply(entry)
}

func (p *protocol) replyBinding() ReplyBinding {
	return ReplyBinding{JobID: p.owner.JobID, SandboxID: p.owner.SandboxID, OwnershipNonce: p.owner.OwnershipNonce, Harness: Harness, ThreadID: p.observed.threadID, TurnID: p.observed.turnID}
}

// item/started establishes native order. Completed events may arrive out of
// order; publish only the contiguous completed prefix of that order.
func (p *protocol) observeReply(method string, params map[string]any) {
	if method != "item/started" && method != "item/completed" {
		return
	}
	item, _ := params["item"].(map[string]any)
	kind, id := stringValue(item["type"]), stringValue(item["id"])
	if kind != "userMessage" && kind != "agentMessage" && kind != "functionCallOutput" {
		return
	}
	// Replayed native IDs cannot establish a retained ordinal. Request one
	// event-triggered full-prefix read outside notification dispatch instead.
	if !p.observed.subscribed {
		if method == "item/completed" {
			p.observed.replyRefresh = true
		}
		return
	}
	if id == "" || p.observed.replyOverflow {
		p.observations.Replies.Gap(p.replyBinding())
		return
	}
	observed := p.observed
	if observed.replyItems == nil {
		observed.replyItems = make(map[string]json.RawMessage)
	}
	if method == "item/started" {
		if _, exists := observed.replyItems[id]; !exists {
			if !p.reserveReplyBytes(len(id) + 64) {
				return
			}
			observed.replyOrder = append(observed.replyOrder, id)
			observed.replyItems[id] = nil
		}
		return
	}
	p.completeReply(id, item)
}

func (p *protocol) completeReply(id string, item map[string]any) {
	observed := p.observed
	raw, err := json.Marshal(item)
	if err != nil {
		p.observations.Replies.Gap(p.replyBinding())
		return
	}
	if _, exists := observed.replyItems[id]; !exists {
		// A completion without its start cannot establish its retained position.
		p.observations.Replies.Gap(p.replyBinding())
		return
	}
	if previous := observed.replyItems[id]; previous != nil {
		if string(previous) != string(raw) {
			p.observations.Replies.Gap(p.replyBinding())
		}
		return
	}
	if !p.reserveReplyBytes(len(raw)) {
		return
	}
	observed.replyItems[id] = raw
	p.drainCompletedReplies()
}

func (p *protocol) drainCompletedReplies() {
	observed := p.observed
	for observed.replyProcessed < len(observed.replyOrder) {
		next := observed.replyItems[observed.replyOrder[observed.replyProcessed]]
		if next == nil {
			return
		}
		items, err := completedConversationItems([]json.RawMessage{next})
		if err != nil {
			p.observations.Replies.Gap(p.replyBinding())
			return
		}
		for _, completed := range items {
			p.observations.Replies.Append(p.replyBinding(), completed)
		}
		observed.replyProcessed++
	}
}

// Cap total retained reply bytes as well as entry count. Eviction wakes readers
// so an old cursor gets explicit resync_deferred rather than a silent reset.
func (f *ReplyFeed) trim(current ReplyBinding) {
	total := 0
	for _, entry := range f.entries {
		total += entry.bytes
	}
	for i := 0; total > replyFeedMaxBytes && i < len(f.order); {
		key := f.order[i]
		if key == current {
			i++
			continue
		}
		entry := f.entries[key]
		total -= entry.bytes
		close(entry.changed)
		delete(f.entries, key)
		f.order = append(f.order[:i], f.order[i+1:]...)
	}
}

func (f *ReplyFeed) Begin(binding ReplyBinding, fresh bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries[binding] != nil {
		return
	}
	entry := f.entry(binding)
	entry.snapshot.Gap = !fresh
}

func (p *protocol) reserveReplyBytes(size int) bool {
	observed := p.observed
	observed.replyBytes += size
	if observed.replyBytes <= replyFeedMaxBytes {
		return true
	}
	observed.replyOverflow = true
	observed.replyItems = nil
	observed.replyOrder = nil
	p.observations.Replies.Gap(p.replyBinding())
	return false
}

// Baseline/recovery work already reads native history. Reuse only explicitly
// full terminal projections; summary arrays never define retained positions.
func (p *protocol) seedReadTurn(threadID string, turn map[string]any) {
	if p.observations == nil || stringValue(turn["itemsView"]) != "full" || !terminal(stringValue(turn["status"])) {
		return
	}
	raw, err := json.Marshal(turn)
	if err != nil {
		return
	}
	full, err := decodeTimelineTurn(raw)
	if err != nil {
		return
	}
	items, err := completedConversationItems(full.Items)
	if err != nil {
		return
	}
	p.observations.Replies.Seed(ReplyBinding{JobID: p.owner.JobID, SandboxID: p.owner.SandboxID, OwnershipNonce: p.owner.OwnershipNonce, Harness: Harness, ThreadID: threadID, TurnID: full.ID}, items, true)
}
