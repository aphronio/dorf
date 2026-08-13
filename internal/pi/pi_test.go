package pi

import (
	"context"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/incus"
)

type recordingRunner struct {
	input  []byte
	args   []string
	result incus.Result
	calls  int
}

func (r *recordingRunner) Run(_ context.Context, _ string, input []byte, args ...string) (incus.Result, error) {
	r.calls++
	r.input = append([]byte(nil), input...)
	r.args = append([]string(nil), args...)
	return r.result, nil
}

func TestInstallRouteUsesScopedGatewayKeyWithResponsesAPI(t *testing.T) {
	runner := &recordingRunner{}
	agent := Agent{Sandbox: incus.Sandbox{Runner: runner}}
	if err := agent.InstallRoute(context.Background(), "sandbox", "http://10.0.0.1:8317/v1", "scoped-key", "gpt-test"); err != nil {
		t.Fatal(err)
	}
	input := string(runner.input)
	for _, required := range []string{`"api":"openai-responses"`, `"apiKey":"$DORF_PROVIDER_ROUTE_KEY"`, `"id":"gpt-test"`, "scoped-key"} {
		if !strings.Contains(input, required) {
			t.Fatalf("route input is missing %q: %s", required, input)
		}
	}
	if strings.Contains(input, `"apiKey":"scoped-key"`) {
		t.Fatal("scoped key was copied into Pi model configuration")
	}
}

func TestParseSessionMapsOnePromptToOneHarnessTurn(t *testing.T) {
	raw := `{"type":"session","version":3,"id":"dorf-job","timestamp":"2026-08-13T00:00:00Z","cwd":"/workspace/job"}
{"type":"message","id":"user0001","parentId":null,"timestamp":"2026-08-13T00:00:01Z","message":{"role":"user","content":"make the change"}}
{"type":"message","id":"assist01","parentId":"user0001","timestamp":"2026-08-13T00:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call","name":"bash","arguments":{}}],"stopReason":"toolUse"}}
{"type":"message","id":"tool0001","parentId":"assist01","timestamp":"2026-08-13T00:00:03Z","message":{"role":"toolResult","toolCallId":"call","toolName":"bash","content":[{"type":"text","text":"ok"}],"isError":false}}
{"type":"message","id":"assist02","parentId":"tool0001","timestamp":"2026-08-13T00:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop"}}`

	threadID, turns, err := parseSession(raw)
	if err != nil {
		t.Fatal(err)
	}
	if threadID != "dorf-job" || len(turns) != 1 || turns[0].ID != "user0001" || turns[0].Status != "completed" || turns[0].Output != "done" {
		t.Fatalf("thread=%q turns=%#v", threadID, turns)
	}
}

func TestParseSessionRejectsASecondPromptBeforeSettlement(t *testing.T) {
	raw := `{"type":"session","version":3,"id":"dorf-job"}
{"type":"message","id":"user0001","parentId":null,"message":{"role":"user","content":"first"}}
{"type":"message","id":"user0002","parentId":"user0001","message":{"role":"user","content":"second"}}`
	if _, _, err := parseSession(raw); err == nil {
		t.Fatal("expected unsettled prior Turn to be rejected")
	}
}

func TestReadInitialTurnsAdoptsNativeTurnAfterLostBinding(t *testing.T) {
	runner := &recordingRunner{result: incus.Result{Stdout: `{"type":"session","version":3,"id":"dorf-job"}
{"type":"message","id":"user0001","parentId":null,"message":{"role":"user","content":"inspect"}}
{"type":"message","id":"assist01","parentId":"user0001","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop"}}`}}
	agent := Agent{Sandbox: incus.Sandbox{Runner: runner}}

	history, err := agent.ReadInitialTurns(context.Background(), "dorf-job", "/workspace/job")
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || history.Harness != Harness || history.ThreadID != "dorf-job" || len(history.Turns) != 1 {
		t.Fatalf("calls=%d history=%#v", runner.calls, history)
	}
	if turn := history.Turns[0]; turn.ID != "user0001" || turn.Status != "completed" || turn.Output != "done" {
		t.Fatalf("recovered Turn=%#v", turn)
	}
	if strings.Contains(strings.Join(runner.args, " "), "exec pi") {
		t.Fatalf("read-only recovery attempted a Pi submission: %q", runner.args)
	}
}
