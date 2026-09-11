package codex

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestRequestedNewTurnsReloadSkillsInRetainedThread(t *testing.T) {
	const workspace = "/workspace/job"
	const threadID = "retained-thread"
	var catalog atomic.Value
	catalog.Store("initial skill")
	var cached atomic.Value
	cached.Store("stale skill")
	server, requests := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/list":
			return map[string]any{"data": []any{}}, false
		case "thread/start", "thread/resume":
			return map[string]any{"thread": map[string]any{"id": threadID}}, false
		case "skills/list":
			requireProtocolParams(t, method, params, map[string]any{"cwds": []any{workspace}, "forceReload": true})
			cached.Store(catalog.Load().(string))
			return map[string]any{"data": []any{map[string]any{"cwd": workspace, "skills": []any{}, "errors": []any{map[string]any{"path": "/unrelated/SKILL.md", "message": "invalid metadata"}}}}}, false
		case "turn/start":
			requireProtocolParams(t, method, params, map[string]any{"threadId": threadID})
			if cached.Load() != catalog.Load() {
				t.Errorf("new turn used stale skill catalog %q, want %q", cached.Load(), catalog.Load())
			}
			return map[string]any{"turn": map[string]any{"id": "turn-" + params["clientUserMessageId"].(string)}}, false
		default:
			return nil, true
		}
	})
	defer server.Close()

	first := dialTestProtocol(t, server)
	first.refreshSkills = true
	thread, turn, err := first.reconcileInitialTurn(context.Background(), workspace, "initial", "input", "model", "high", "danger-full-access")
	if err != nil || thread != threadID || turn.ID != "turn-initial" {
		t.Fatalf("initial thread=%q turn=%#v err=%v", thread, turn, err)
	}
	if methods := reviewProtocolMethods(requests); !reflect.DeepEqual(methods, []string{"thread/list", "thread/start", "skills/list", "turn/start"}) {
		t.Fatalf("initial methods=%v", methods)
	}
	_ = first.connection.CloseNow()

	for i, updated := range []string{"renamed skill with revised description", ""} {
		catalog.Store(updated)
		runID := fmt.Sprintf("follow-%d", i)
		follow := dialTestProtocol(t, server)
		follow.refreshSkills = true
		turn, err := follow.resumeAndStartTurn(context.Background(), threadID, workspace, runID, "input", "model", "high", "danger-full-access")
		if err != nil || turn.ID != "turn-"+runID {
			t.Fatalf("follow turn=%#v err=%v", turn, err)
		}
		if methods := reviewProtocolMethods(requests); !reflect.DeepEqual(methods, []string{"thread/resume", "skills/list", "turn/start"}) {
			t.Fatalf("follow methods=%v", methods)
		}
		_ = follow.connection.CloseNow()
	}
}

func TestSkillReloadFailurePreventsSubmission(t *testing.T) {
	for _, name := range []string{"rejected", "malformed response", "connection lost"} {
		t.Run(name, func(t *testing.T) {
			server, requests := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
				switch method {
				case "initialize":
					return map[string]any{}, false
				case "skills/list":
					return nil, name == "rejected"
				default:
					t.Errorf("reload failure allowed %s", method)
					return nil, true
				}
			})
			defer server.Close()
			p := dialTestProtocol(t, server)
			p.refreshSkills = true
			p.instructions = &workspaceInstructions{soul: "pending workspace instructions"}
			if name == "connection lost" {
				_ = p.connection.CloseNow()
			}
			turn, err := p.startTurn(context.Background(), "thread", "/workspace/job", "run", "input", "model", "high", "danger-full-access")
			var definite interface{ DefiniteNoSubmit() bool }
			if !errors.As(err, &definite) || !definite.DefiniteNoSubmit() || turn.ID != "" {
				t.Fatalf("turn=%#v err=%T %v", turn, err, err)
			}
			if name == "rejected" {
				var rejected *RejectedError
				if !errors.As(err, &rejected) || rejected.Method != "skills/list" {
					t.Fatalf("lost native rejection cause: %v", err)
				}
			}
			for _, method := range reviewProtocolMethods(requests) {
				if method != "skills/list" {
					t.Errorf("reload failure sent %s", method)
				}
			}
		})
	}
}

func TestOrdinaryTurnDoesNotReloadSkills(t *testing.T) {
	server, requests := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "thread/resume":
			return map[string]any{"thread": map[string]any{"id": "thread"}}, false
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn"}}, false
		default:
			t.Errorf("ordinary turn requested %s", method)
			return nil, true
		}
	})
	defer server.Close()
	turn, err := dialTestProtocol(t, server).resumeAndStartTurn(context.Background(), "thread", "/workspace/job", "run", "input", "model", "high", "danger-full-access")
	if err != nil || turn.ID != "turn" {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	if got := reviewProtocolMethods(requests); !reflect.DeepEqual(got, []string{"thread/resume", "turn/start"}) {
		t.Fatalf("methods=%v", got)
	}
}
