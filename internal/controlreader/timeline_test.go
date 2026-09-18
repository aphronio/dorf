package controlreader

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

type timelineTestStore struct {
	*readerTestStore
}

func (s *timelineTestStore) Sandboxes(context.Context, string) ([]core.Sandbox, error) {
	return []core.Sandbox{s.sandbox}, nil
}

type timelineTestRuntime struct {
	store  *timelineTestStore
	calls  int
	result core.HarnessTimeline
	err    error
}

func (r *timelineTestRuntime) ResolveSandbox(context.Context, core.SandboxProfileRef) (core.SandboxRuntime, error) {
	return core.SandboxRuntime{SandboxProfile: r.store.session.ProfileRef(), Timeline: r}, nil
}
func (r *timelineTestRuntime) ReadTimeline(_ context.Context, session core.Session, owned core.Sandbox, threadID, turnID string) (core.HarnessTimeline, error) {
	r.calls++
	if !r.store.inFence || session != r.store.session || owned != r.store.sandbox || threadID != "bound" || turnID != "selected" {
		return core.HarnessTimeline{}, errors.New("wrong read authority")
	}
	return r.result, r.err
}

func TestTimelineClientEnforcesCustodyAndPropagatesNativeFailures(t *testing.T) {
	session := core.Session{ID: "session", SandboxProfile: "profile", CleanupState: core.CleanupPending, Harness: "codex", ThreadID: "bound"}
	owned := core.Sandbox{ID: "sandbox", Name: core.DefaultSandbox, SessionID: session.ID, OwnershipNonce: strings.Repeat("a", 64)}
	store := &timelineTestStore{readerTestStore: &readerTestStore{session: session, sandbox: owned}}
	runtime := &timelineTestRuntime{store: store, result: core.HarnessTimeline{Harness: "codex", ThreadID: "bound", TurnID: "selected", Status: "inProgress", Items: []json.RawMessage{json.RawMessage(`{"id":"item","type":"agentMessage","phase":"commentary"}`)}}}
	handler, err := NewHandler(strings.Repeat("b", 64), Service{Store: store, Runtimes: runtime})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient("http://control-reader.test:8756", strings.Repeat("b", 64), &http.Client{Transport: readerHandlerTransport{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ReadTimeline(context.Background(), session.ID, "selected")
	if err != nil || result.TurnID != "selected" || runtime.calls != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, test := range []string{"missing-session", "cleanup", "no-binding", "foreign-sandbox", "foreign-thread", "unknown-turn", "unsupported", "oversized"} {
		t.Run(test, func(t *testing.T) {
			store.session = session
			store.sandbox = owned
			runtime.err = nil
			runtime.result.ThreadID = "bound"
			runtime.result.Items = []json.RawMessage{json.RawMessage(`{"id":"item","type":"agentMessage"}`)}
			sessionID := session.ID
			want := core.ErrTimelineUnavailable
			switch test {
			case "missing-session":
				sessionID = "missing"
				want = ErrSessionNotFound
			case "cleanup":
				store.session.CleanupState = core.CleanupRequested
			case "no-binding":
				store.session.Harness, store.session.ThreadID = "", ""
			case "foreign-sandbox":
				store.sandbox.SessionID = "foreign"
			case "foreign-thread":
				runtime.result.ThreadID = "other"
			case "unknown-turn":
				runtime.err = core.ErrTurnNotFound
				want = core.ErrTurnNotFound
			case "unsupported":
				runtime.err = errors.New("unsupported native version")
			case "oversized":
				runtime.result.Items = []json.RawMessage{json.RawMessage(`{"text":"` + strings.Repeat("x", MaxObservationBytes) + `"}`)}
				want = ErrResponseTooLarge
			}
			result, err := client.ReadTimeline(context.Background(), sessionID, "selected")
			if !errors.Is(err, want) || len(result.Items) != 0 {
				t.Fatalf("result=%+v err=%v want=%v", result, err, want)
			}
		})
	}
}
