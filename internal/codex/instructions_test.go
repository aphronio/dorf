package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestWorkspaceInstructionsFollowFileChangesInOneThread(t *testing.T) {
	cache := NewObservations(context.Background(), nil)
	defer cache.Close()
	sandbox := &instructionSandbox{files: map[string]string{"AGENTS.md": "Initial operating rules.", "SOUL.md": strings.Repeat("Complete persona instructions.\n", 512)}}
	injections := make(chan string, 1)
	turns := make(chan map[string]any, 1)
	server, requests := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			requireProtocolParams(t, method, params, map[string]any{"capabilities": map[string]any{"experimentalApi": true}})
			return map[string]any{}, false
		case "skills/list":
			return map[string]any{}, false
		case "thread/list":
			return map[string]any{"data": []any{}}, false
		case "thread/start", "thread/resume":
			return map[string]any{"thread": map[string]any{"id": "retained-thread"}}, false
		case "thread/inject_items":
			items := params["items"].([]any)
			item := items[0].(map[string]any)
			if len(items) != 1 || item["role"] != "developer" {
				t.Fatal("workspace context was not a single native developer message")
			}
			injections <- item["content"].([]any)[0].(map[string]any)["text"].(string)
			return map[string]any{}, false
		case "turn/start":
			turns <- params
			return map[string]any{"turn": map[string]any{"id": "turn-" + params["clientUserMessageId"].(string)}}, false
		default:
			return nil, true
		}
	})
	defer server.Close()
	sandbox.endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
	agent := Agent{Sandbox: sandbox, Observations: cache}
	owner := testOwner("instructions")
	const input = "  Ordinary user text stays unchanged.\n"
	replacementAgents, replacementSoul, empty := "Replacement operating rules.", "Replacement persona.", ""
	for i, test := range []struct {
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
		{name: "context rebuilt", notice: true, injectSoul: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.agents != nil {
				sandbox.files["AGENTS.md"] = *test.agents
			}
			if test.soul != nil {
				sandbox.files["SOUL.md"] = *test.soul
			}
			if test.name == "deleted agents" {
				delete(sandbox.files, "AGENTS.md")
			}
			if test.name == "context rebuilt" {
				p := &protocol{observations: cache, instructionCache: cache, observed: &observedTurn{threadID: "retained-thread", turnID: "previous-turn"}}
				p.observeNotification(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "retained-thread", "turnId": "previous-turn", "item": map[string]any{"type": "contextCompaction"}}})
			}
			runID := fmt.Sprintf("run-%d", i)
			var err error
			if i == 0 {
				_, err = agent.StartInitialTurn(context.Background(), owner, "/workspace/job", runID, input, "model", "high", false)
			} else {
				_, err = agent.StartTurn(context.Background(), owner, "/workspace/job", "retained-thread", runID, input, "model", "high", false)
			}
			if err != nil {
				t.Fatal(err)
			}
			submitted := <-turns
			requireProtocolParams(t, "turn/start", submitted, map[string]any{"threadId": "retained-thread", "model": "model", "effort": "high", "input": []any{map[string]any{"type": "text", "text": input}}})
			if test.notice || test.injectSoul {
				injected := <-injections
				if strings.Contains(injected, "was updated") != test.notice || strings.Contains(injected, "<SOUL.md>") != test.injectSoul {
					t.Fatalf("wrong changed-file delivery: %q", injected)
				}
				if test.notice && !strings.Contains(injected, "/workspace/job/AGENTS.md") {
					t.Fatal("missing exact AGENTS path")
				}
				if test.injectSoul && !strings.Contains(injected, "<SOUL.md>\n"+sandbox.files["SOUL.md"]+"\n</SOUL.md>") {
					t.Fatal("SOUL contents were truncated or changed")
				}
				if contents := sandbox.files["AGENTS.md"]; contents != "" && strings.Contains(injected, contents) {
					t.Fatal("AGENTS contents were injected instead of a change notice")
				}
			} else if len(injections) != 0 {
				t.Fatal("unchanged files were injected again")
			}
			reviewProtocolMethods(requests)
		})
	}
}

func TestUnreadableInstructionsPreventNativeSubmission(t *testing.T) {
	failure := errors.New("transport unavailable")
	sandbox := &instructionSandbox{readErr: failure}
	agent := Agent{Sandbox: sandbox}
	_, err := agent.StartTurn(context.Background(), testOwner("instructions"), "/workspace/job", "thread", "run", "hello", "model", "high", false)
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
