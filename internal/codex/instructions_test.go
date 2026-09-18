package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/coder/websocket"
)

func TestWorkspaceInstructionsFollowFileChangesWithoutExport(t *testing.T) {
	f := newInstructionFixture(t)
	owner := testOwner("instructions")
	replacementAgents, replacementSoul, empty := "Replacement operating rules.", "Replacement persona.", ""
	for _, test := range []struct {
		name               string
		agents, soul       *string
		notice, injectSoul bool
	}{
		{name: "initial", injectSoul: true},
		{name: "unchanged"},
		{name: "agents changed", agents: &replacementAgents, notice: true},
		{name: "unchanged after edit"},
		{name: "soul changed", soul: &replacementSoul, injectSoul: true},
		{name: "empty soul", soul: &empty, injectSoul: true},
		{name: "deleted agents", notice: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.agents != nil {
				f.sandbox.files["AGENTS.md"] = *test.agents
			}
			if test.soul != nil {
				f.sandbox.files["SOUL.md"] = *test.soul
			}
			if test.name == "deleted agents" {
				delete(f.sandbox.files, "AGENTS.md")
			}
			s := &instructionSession{initial: test.name == "initial"}
			f.submit(t, owner, s, "exact")
			f.requireInjection(t, s, test.notice, test.injectSoul)
		})
	}
}

func TestInstructionTrackingRefreshesAfterObservationLoss(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		for _, event := range []string{"early compaction", "compaction started", "compaction completed", "disconnect", "foreign events"} {
			t.Run(fmt.Sprintf("recovery=%t/%s", recovery, event), func(t *testing.T) {
				f := newInstructionFixture(t)
				owner := testOwner("instructions")
				f.submit(t, owner, &instructionSession{initial: true}, "exact")
				f.submit(t, owner, &instructionSession{recovery: recovery, event: event}, "exact")
				next := &instructionSession{}
				f.submit(t, owner, next, "exact")
				refresh := event != "foreign events"
				f.requireInjection(t, next, refresh, refresh)
			})
		}
	}
}

func TestInstructionTrackingRequiresObservedAcceptance(t *testing.T) {
	for _, failure := range []string{"missing execution", "wrong run", "wrong session", "wrong sandbox", "rejected", "missing turn ID", "lost acknowledgement", "closed observer"} {
		t.Run(failure, func(t *testing.T) {
			f := newInstructionFixture(t)
			owner := testOwner("instructions")
			f.submit(t, owner, &instructionSession{initial: true}, "exact")
			if failure == "closed observer" {
				f.agent.Observations.Close()
			}
			unobserved := &instructionSession{failure: failure}
			f.submit(t, owner, unobserved, failure)
			switch failure {
			case "missing execution", "wrong run", "wrong session", "wrong sandbox", "closed observer":
				f.requireInjection(t, unobserved, true, true)
			}
			next := &instructionSession{}
			f.submit(t, owner, next, "exact")
			f.requireInjection(t, next, true, true)
		})
	}
}

func TestInstructionTrackingIsScopedToOwnerAndWorker(t *testing.T) {
	f := newInstructionFixture(t)
	owner := testOwner("instructions")
	f.submit(t, owner, &instructionSession{initial: true}, "exact")
	for _, other := range []provider.Ownership{
		{SessionID: "other-session", SandboxID: owner.SandboxID},
		{SessionID: owner.SessionID, SandboxID: "other-sandbox"},
	} {
		first := &instructionSession{}
		f.submit(t, other, first, "exact")
		f.requireInjection(t, first, true, true)
		f.submit(t, other, &instructionSession{event: "compaction completed"}, "exact")
	}
	unchanged := &instructionSession{}
	f.submit(t, owner, unchanged, "exact")
	f.requireInjection(t, unchanged, false, false)
	f.agent.Observations.Close()
	f.agent.Observations = NewObservations(context.Background(), nil)
	f.submit(t, owner, &instructionSession{recovery: true}, "exact")
	restarted := &instructionSession{}
	f.submit(t, owner, restarted, "exact")
	f.requireInjection(t, restarted, true, true)
}

func TestInstructionTrackingForgetsRefusedSubscription(t *testing.T) {
	f := newInstructionFixture(t)
	owner := testOwner("instructions")
	f.submit(t, owner, &instructionSession{initial: true}, "exact")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ctx := telemetry.WithExecution(context.Background(), core.AgentRun{ID: "duplicate", SessionID: owner.SessionID, SandboxID: owner.SandboxID, MessageID: "message"})
	for _, held := range []bool{true, false} {
		s := &instructionSession{runID: "duplicate", closed: make(chan struct{})}
		if held {
			s.release = release
		}
		f.sessions <- s
		turn, err := f.agent.StartTurn(ctx, owner, "/workspace/job", "retained-thread", s.runID, core.HarnessInput{Text: "retry"}, "model", "high", false)
		if err != nil || turn.Turn.ID != "native-duplicate" {
			t.Fatalf("accepted turn=%#v err=%v", turn, err)
		}
	}
	next := &instructionSession{}
	f.nextRun++
	next.runID, next.closed = fmt.Sprintf("run-%d", f.nextRun), make(chan struct{})
	f.sessions <- next
	ctx = telemetry.WithExecution(context.Background(), core.AgentRun{ID: next.runID, SessionID: owner.SessionID, SandboxID: owner.SandboxID, MessageID: "next"})
	turn, err := f.agent.StartTurn(ctx, owner, "/workspace/job", "retained-thread", next.runID, core.HarnessInput{Text: "follow"}, "model", "high", false)
	if err != nil || turn.Turn.ID != "native-"+next.runID {
		t.Fatalf("follow turn=%#v err=%v", turn, err)
	}
	select {
	case <-next.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("follow observation did not settle")
	}
	f.requireInjection(t, next, true, true)
}

type instructionSession struct {
	runID      string
	initial    bool
	recovery   bool
	event      string
	failure    string
	injections []string
	input      any
	closed     chan struct{}
	release    <-chan struct{}
}

type instructionFixture struct {
	agent    Agent
	sandbox  *instructionSandbox
	sessions chan *instructionSession
	nextRun  int
}

func newInstructionFixture(t *testing.T) *instructionFixture {
	t.Helper()
	f := &instructionFixture{
		sandbox:  &instructionSandbox{files: map[string]string{"AGENTS.md": "Initial operating rules.", "SOUL.md": strings.Repeat("Complete persona instructions.\n", 512)}},
		sessions: make(chan *instructionSession, 1),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := <-f.sessions
		defer close(s.closed)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		s.serve(t, r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	f.sandbox.endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
	f.agent = Agent{Sandbox: f.sandbox, Timeout: 3 * time.Second, Observations: NewObservations(context.Background(), nil)}
	t.Cleanup(func() { closeInstructionObservations(t, f.agent.Observations) })
	return f
}

func (f *instructionFixture) submit(t *testing.T, owner provider.Ownership, s *instructionSession, attribution string) {
	t.Helper()
	f.nextRun++
	s.runID, s.closed = fmt.Sprintf("run-%d", f.nextRun), make(chan struct{})
	f.sessions <- s
	run := core.AgentRun{ID: s.runID, SessionID: owner.SessionID, SandboxID: owner.SandboxID, MessageID: "message", TurnID: "native-" + s.runID}
	switch attribution {
	case "wrong run":
		run.ID = "other-run"
	case "wrong session":
		run.SessionID = "other-session"
	case "wrong sandbox":
		run.SandboxID = "other-sandbox"
	}
	ctx := context.Background()
	if attribution != "missing execution" {
		ctx = telemetry.WithExecution(ctx, run)
	}
	const input = "  Ordinary user text stays unchanged.\n"
	var err error
	var turn TurnOutcome
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		if s.recovery {
			var history core.HarnessHistory
			history, err = f.agent.ReadTurns(ctx, owner, "retained-thread")
			if len(history.Turns) == 1 {
				turn = history.Turns[0]
			}
		} else if s.initial {
			var binding core.HarnessBinding
			binding, err = f.agent.StartInitialTurn(ctx, owner, "/workspace/job", s.runID, core.HarnessInput{Text: input}, "model", "high", false)
			turn = binding.Turn
		} else {
			var binding core.HarnessBinding
			binding, err = f.agent.StartTurn(ctx, owner, "/workspace/job", "retained-thread", s.runID, core.HarnessInput{Text: input}, "model", "high", false)
			turn = binding.Turn
		}
		f.agent.Observations.wg.Wait()
	}()
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("instruction submission or observation did not settle")
	}
	wantError := s.failure == "rejected" || s.failure == "missing turn ID" || s.failure == "lost acknowledgement"
	if (err != nil) != wantError {
		t.Fatalf("submission err=%v, want error=%t", err, wantError)
	}
	if !wantError && turn.ID != "native-"+s.runID {
		t.Fatalf("native turn=%q, want %q", turn.ID, "native-"+s.runID)
	}
	select {
	case <-s.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("native connection was not released")
	}
	if !s.recovery && !reflect.DeepEqual(s.input, []any{map[string]any{"type": "text", "text": input}}) {
		t.Fatalf("submitted input changed: %#v", s.input)
	}
}

func closeInstructionObservations(t *testing.T, observations *Observations) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		observations.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Error("instruction observer did not stop")
	}
}

func (f *instructionFixture) requireInjection(t *testing.T, s *instructionSession, notice, soul bool) {
	t.Helper()
	if !notice && !soul {
		if len(s.injections) != 0 {
			t.Fatal("unchanged instructions were injected again")
		}
		return
	}
	if len(s.injections) != 1 {
		t.Fatalf("got %d instruction messages, want 1", len(s.injections))
	}
	injected := s.injections[0]
	if strings.Contains(injected, "was updated") != notice || strings.Contains(injected, "<SOUL.md>") != soul {
		t.Fatalf("wrong changed-file delivery: %q", injected)
	}
	if notice && !strings.Contains(injected, "/workspace/job/AGENTS.md") {
		t.Fatal("missing exact AGENTS path")
	}
	if soul && !strings.Contains(injected, "<SOUL.md>\n"+f.sandbox.files["SOUL.md"]+"\n</SOUL.md>") {
		t.Fatal("SOUL contents were truncated or changed")
	}
	if contents := f.sandbox.files["AGENTS.md"]; contents != "" && strings.Contains(injected, contents) {
		t.Fatal("AGENTS contents were injected instead of a change notice")
	}
}

func (s *instructionSession) serve(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	write := func(value any) bool {
		data, _ := json.Marshal(value)
		return conn.Write(ctx, websocket.MessageText, data) == nil
	}
	reads := 0
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var request map[string]any
		if json.Unmarshal(data, &request) != nil {
			t.Error("invalid native request")
			return
		}
		if request["id"] == nil {
			continue
		}
		method := stringValue(request["method"])
		params, _ := request["params"].(map[string]any)
		result := map[string]any{}
		switch method {
		case "initialize":
			requireProtocolParams(t, method, params, map[string]any{"capabilities": map[string]any{"experimentalApi": true}})
		case "thread/list":
			result["data"] = []any{}
		case "thread/start", "thread/resume":
			result["thread"] = map[string]any{"id": "retained-thread"}
		case "thread/inject_items":
			items := params["items"].([]any)
			if len(items) != 2 {
				t.Error("workspace refresh must revoke legacy developer authority before user context")
				return
			}
			notice := items[0].(map[string]any)
			if notice["role"] != "developer" || notice["content"].([]any)[0].(map[string]any)["text"] != workspaceAuthorityNotice {
				t.Error("workspace migration must use only the fixed developer notice")
			}
			item := items[1].(map[string]any)
			if item["role"] != "user" {
				t.Error("workspace content was not a native user message")
				return
			}
			s.injections = append(s.injections, item["content"].([]any)[0].(map[string]any)["text"].(string))
		case "turn/start":
			s.input = params["input"]
			if s.failure == "lost acknowledgement" {
				return
			}
			if s.failure != "missing turn ID" {
				result["turn"] = map[string]any{"id": "native-" + s.runID}
			}
		case "thread/turns/list":
			status := "inProgress"
			if !s.recovery || reads >= 2 {
				status = "completed"
			}
			result["data"] = []any{map[string]any{"id": "native-" + s.runID, "status": status, "itemsView": "full", "items": []any{map[string]any{"id": "input", "type": "userMessage", "clientId": s.runID, "content": []any{}}}}}
			result["nextCursor"] = nil
		case "thread/read":
			reads++
			result["thread"] = map[string]any{"id": "retained-thread", "turns": []any{map[string]any{"id": "native-" + s.runID, "status": "inProgress", "items": []any{}}}}
		default:
			t.Errorf("unexpected native request %q", method)
			return
		}
		if (method == "turn/start" || method == "thread/read" && reads == 1) && s.event == "early compaction" {
			write(s.notification("item/completed", "retained-thread", "native-"+s.runID))
		}
		response := map[string]any{"id": request["id"], "result": result}
		if method == "turn/start" && s.failure == "rejected" {
			response = map[string]any{"id": request["id"], "error": map[string]any{"code": -32000, "message": "rejected"}}
		}
		if !write(response) {
			return
		}
		if method != "turn/start" && !(method == "thread/read" && reads == 2) {
			continue
		}
		if s.release != nil {
			<-s.release
		}
		if s.event == "disconnect" {
			return
		}
		switch s.event {
		case "compaction started":
			write(s.notification("item/started", "retained-thread", "native-"+s.runID))
		case "compaction completed":
			write(s.notification("item/completed", "retained-thread", "native-"+s.runID))
		case "foreign events":
			write(s.notification("item/completed", "retained-thread", "other-turn"))
			write(s.notification("item/completed", "other-thread", "native-"+s.runID))
			write(s.notification("turn/completed", "retained-thread", "other-turn"))
		}
		write(s.notification("turn/completed", "retained-thread", "native-"+s.runID))
	}
}

func (s *instructionSession) notification(method, threadID, turnID string) map[string]any {
	params := map[string]any{"threadId": threadID, "turnId": turnID}
	if method == "turn/completed" {
		params["turn"] = map[string]any{"id": turnID, "status": "completed"}
	} else {
		params["item"] = map[string]any{"id": "compaction", "type": "contextCompaction"}
	}
	return map[string]any{"method": method, "params": params}
}

func TestUnreadableInstructionsPreventNativeSubmission(t *testing.T) {
	failure := errors.New("transport unavailable")
	sandbox := &instructionSandbox{readErr: failure}
	agent := Agent{Sandbox: sandbox}
	_, err := agent.StartTurn(context.Background(), testOwner("instructions"), "/workspace/job", "thread", "run", core.HarnessInput{Text: "hello"}, "model", "high", false)
	if !errors.Is(err, failure) || sandbox.endpoints != 0 {
		t.Fatalf("err=%v native connections=%d", err, sandbox.endpoints)
	}
}

type instructionSandbox struct {
	provider.Sandbox
	files     map[string]string
	endpoint  string
	readErr   error
	endpoints int
}

func (s *instructionSandbox) ReadFile(_ context.Context, _ provider.Ownership, path string) ([]byte, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	contents, found := s.files[path]
	if !found {
		return nil, os.ErrNotExist
	}
	return []byte(contents), nil
}

func (s *instructionSandbox) Endpoint(context.Context, provider.Ownership, int) (provider.Endpoint, error) {
	s.endpoints++
	return provider.NewEndpoint(s.endpoint, s.endpoint, nil), nil
}

func (s *instructionSandbox) Exec(_ context.Context, _ provider.Ownership, _ []byte, args ...string) (provider.Result, error) {
	if !reflect.DeepEqual(args, []string{"bash", "-lc", probeServerScript(s.endpoint)}) {
		return provider.Result{}, fmt.Errorf("unexpected process mutation")
	}
	return provider.Result{Stdout: "1\n1\nscoped-test-capability\n"}, nil
}
