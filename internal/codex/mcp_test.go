package codex

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestRequestedMCPReloadPrecedesRetainedThreadInput(t *testing.T) {
	server, requests := testProtocolServer(t, func(method string, params map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize", "config/mcpServer/reload":
			return map[string]any{}, false
		case "thread/resume":
			return map[string]any{"thread": map[string]any{"id": "thread"}}, false
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn"}}, false
		default:
			t.Errorf("unexpected method %s", method)
			return nil, true
		}
	})
	defer server.Close()
	p := dialTestProtocol(t, server)
	p.refreshMCPServers = true
	turn, err := p.resumeFixture(context.Background(), "thread", "/workspace", "run", core.HarnessInput{Text: "input"}, "model", "high", "danger-full-access")
	if err != nil || turn.ID != "turn" {
		t.Fatalf("turn=%#v err=%v", turn, err)
	}
	if got := protocolMethods(requests); !reflect.DeepEqual(got, []string{"thread/resume", "config/mcpServer/reload", "turn/start"}) {
		t.Fatalf("methods=%v", got)
	}
}

func TestMCPReloadFailureIsDefiniteNoSubmit(t *testing.T) {
	server, requests := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
		switch method {
		case "initialize":
			return map[string]any{}, false
		case "config/mcpServer/reload":
			return nil, true
		default:
			t.Errorf("failed reload allowed %s", method)
			return nil, true
		}
	})
	defer server.Close()
	p := dialTestProtocol(t, server)
	p.refreshMCPServers = true
	turn, err := p.startTurn(context.Background(), "thread", "/workspace", "run", core.HarnessInput{Text: "input"}, "model", "high", "danger-full-access")
	var definite interface{ DefiniteNoSubmit() bool }
	if !errors.As(err, &definite) || !definite.DefiniteNoSubmit() || turn.ID != "" {
		t.Fatalf("turn=%#v err=%v", turn, err)
	}
	if got := protocolMethods(requests); !reflect.DeepEqual(got, []string{"config/mcpServer/reload"}) {
		t.Fatalf("methods=%v", got)
	}
}
