package pi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/incus"
)

type recordingRunner struct {
	input  []byte
	args   []string
	result incus.Result
	calls  int
}

type rejectedRPCPromptRunner struct{}

func (*rejectedRPCPromptRunner) Run(_ context.Context, _ string, _ []byte, args ...string) (incus.Result, error) {
	command := strings.Join(args, " ")
	if strings.Contains(command, "events.jsonl") {
		return incus.Result{Stdout: `{"id":"run-1","type":"response","command":"prompt","success":false,"error":"prompt rejected"}`}, nil
	}
	if strings.Contains(command, sessionDir) {
		return incus.Result{Stdout: `{"type":"session","version":3,"id":"sandbox"}`}, nil
	}
	return incus.Result{}, nil
}

type acceptedRPCPromptRunner struct {
	before    string
	after     string
	requestID string
	submitted bool
}

type progressingHistoryRunner struct {
	reads int
}

type acceptedRPCSteerRunner struct {
	request []byte
}

type emptyStrictReviewRunner struct {
	owner incus.ReviewMetadata
}

func (r *emptyStrictReviewRunner) Run(_ context.Context, _ string, _ []byte, args ...string) (incus.Result, error) {
	command := strings.Join(args, " ")
	if command == "list --format=json" {
		return incus.Result{Stdout: `[{"name":"dorf-review-owned","config":{"user.dorf.owner":"review","user.dorf.job":"` + r.owner.JobID + `","user.dorf.agent_run":"` + r.owner.AgentRunID + `","user.dorf.revision":"` + r.owner.Revision + `","user.dorf.ownership_nonce":"` + r.owner.OwnershipNonce + `"}}]`}, nil
	}
	if strings.Contains(command, "ambiguous Pi session identity") {
		return incus.Result{}, nil
	}
	return incus.Result{}, nil
}

func (r *acceptedRPCSteerRunner) Run(_ context.Context, _ string, input []byte, args ...string) (incus.Result, error) {
	if len(input) > 0 {
		r.request = append([]byte(nil), input...)
		return incus.Result{}, nil
	}
	if strings.Join(args, " ") == "exec sandbox -- cat "+rpcEvents {
		return incus.Result{Stdout: `{"id":"run-steer","type":"response","command":"steer","success":true}`}, nil
	}
	return incus.Result{}, nil
}

func (r *progressingHistoryRunner) Run(_ context.Context, _ string, _ []byte, _ ...string) (incus.Result, error) {
	r.reads++
	stopReason := "toolUse"
	content := `[{"type":"toolCall","id":"call-1","name":"read","arguments":{}}]`
	if r.reads > 1 {
		stopReason = "stop"
		content = `[{"type":"text","text":"done"}]`
	}
	return incus.Result{Stdout: `{"type":"session","version":3,"id":"dorf-job"}
{"type":"message","id":"user0001","parentId":null,"message":{"role":"user","content":"inspect"}}
{"type":"message","id":"assist01","parentId":"user0001","message":{"role":"assistant","content":` + content + `,"stopReason":"` + stopReason + `"}}`}, nil
}

func (r *acceptedRPCPromptRunner) Run(_ context.Context, _ string, input []byte, args ...string) (incus.Result, error) {
	command := strings.Join(args, " ")
	if strings.Contains(string(input), `"type":"prompt"`) {
		r.submitted = true
		return incus.Result{}, nil
	}
	if command == "exec sandbox -- cat "+rpcEvents {
		return incus.Result{Stdout: `{"id":"` + r.requestID + `","type":"response","command":"prompt","success":true}`}, nil
	}
	if strings.Contains(command, "ambiguous Pi session identity") {
		if r.submitted {
			return incus.Result{Stdout: r.after}, nil
		}
		return incus.Result{Stdout: r.before}, nil
	}
	return incus.Result{}, nil
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

func TestParseSessionKeepsQueuedSteerInsideActiveHarnessTurn(t *testing.T) {
	raw := `{"type":"session","version":3,"id":"dorf-job"}
{"type":"message","id":"user0001","parentId":null,"message":{"role":"user","content":"first"}}
	{"type":"message","id":"assist01","parentId":"user0001","message":{"role":"assistant","content":[],"stopReason":"toolUse"}}
	{"type":"message","id":"user0002","parentId":"assist01","message":{"role":"user","content":"steer active work"}}
	{"type":"message","id":"assist02","parentId":"user0002","message":{"role":"assistant","content":[{"type":"text","text":"steered result"}],"stopReason":"stop"}}`
	_, turns, err := parseSession(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].ID != "user0001" || turns[0].Status != "completed" || turns[0].Output != "steered result" {
		t.Fatalf("steered Turns=%#v", turns)
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

func TestStartTurnAppendsExactlyOneNativeTurn(t *testing.T) {
	oneTurn := `{"type":"session","version":3,"id":"dorf-job"}
{"type":"message","id":"user0001","parentId":null,"message":{"role":"user","content":"first"}}
{"type":"message","id":"assist01","parentId":"user0001","message":{"role":"assistant","content":[{"type":"text","text":"first done"}],"stopReason":"stop"}}`
	twoTurns := oneTurn + `
{"type":"message","id":"user0002","parentId":"assist01","message":{"role":"user","content":"second"}}
{"type":"message","id":"assist02","parentId":"user0002","message":{"role":"assistant","content":[{"type":"text","text":"second done"}],"stopReason":"stop"}}`
	runner := &acceptedRPCPromptRunner{before: oneTurn, after: twoTurns, requestID: "run-2"}
	agent := Agent{Sandbox: incus.Sandbox{Runner: runner}}

	binding, err := agent.StartTurn(context.Background(), "sandbox", "/workspace/job", "dorf-job", "run-2", "second", "gpt-test", "low")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Harness != Harness || binding.ThreadID != "dorf-job" {
		t.Fatalf("binding=%#v", binding)
	}
	if binding.Turn.ID != "user0002" || binding.Turn.Status != "completed" || binding.Turn.Output != "second done" {
		t.Fatalf("follow-up Turn=%#v", binding.Turn)
	}
}

func TestActiveTurnSteerAcknowledgesExactTarget(t *testing.T) {
	runner := &acceptedRPCSteerRunner{}
	agent := Agent{Sandbox: incus.Sandbox{Runner: runner}}

	accepted, err := agent.SteerTurn(context.Background(), "sandbox", "thread", "turn-active", "run-steer", "change direction")
	if err != nil {
		t.Fatal(err)
	}
	if accepted != "turn-active" {
		t.Fatalf("accepted Turn=%q", accepted)
	}
	if !strings.Contains(string(runner.request), `"id":"run-steer"`) || !strings.Contains(string(runner.request), `"type":"steer"`) || !strings.Contains(string(runner.request), `"message":"change direction"`) {
		t.Fatalf("RPC request=%s", runner.request)
	}
}

func TestStrictReviewRecoveryWithoutNativeTurnAllowsOriginalSubmission(t *testing.T) {
	owner := incus.ReviewMetadata{JobID: "job-review", AgentRunID: "run-review", Revision: strings.Repeat("b", 40), OwnershipNonce: strings.Repeat("c", 64)}
	agent := Agent{Sandbox: incus.Sandbox{Runner: &emptyStrictReviewRunner{owner: owner}}}

	binding, err := agent.RecoverStrictReviewTurn(context.Background(), "dorf-review-owned", "/workspace/job", owner, strings.Repeat("a", 64), "review", "gpt-test", "low")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Harness != "" || binding.ThreadID != "" || binding.Turn.ID != "" || binding.ControllerID != "" {
		t.Fatalf("empty strict review recovery binding=%#v", binding)
	}
}

func TestInitialTurnReportsRPCPreflightRejectionAsDefinitelyNotSubmitted(t *testing.T) {
	agent := Agent{Sandbox: incus.Sandbox{Runner: &rejectedRPCPromptRunner{}}}

	_, err := agent.StartInitialTurn(context.Background(), "sandbox", "/workspace/job", "run-1", "inspect", "gpt-test", "low")
	if err == nil || !strings.Contains(err.Error(), "prompt rejected") {
		t.Fatalf("initial Turn error=%v", err)
	}
	definite, ok := err.(interface{ DefiniteNoSubmit() bool })
	if !ok || !definite.DefiniteNoSubmit() {
		t.Fatalf("preflight rejection is not definite-no-submit: %T %v", err, err)
	}
}

func TestWaitTurnObservesExactNativeTurnUntilTerminal(t *testing.T) {
	runner := &progressingHistoryRunner{}
	agent := Agent{Sandbox: incus.Sandbox{Runner: runner}, Timeout: time.Second}

	binding, err := agent.WaitTurn(context.Background(), "sandbox", "dorf-job", "user0001")
	if err != nil {
		t.Fatal(err)
	}
	if runner.reads != 2 || binding.ThreadID != "dorf-job" || binding.Turn.ID != "user0001" || binding.Turn.Status != "completed" || binding.Turn.Output != "done" {
		t.Fatalf("reads=%d binding=%#v", runner.reads, binding)
	}
}
