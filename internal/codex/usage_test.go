package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestNativeUsageReadsAggregatesAndDeduplicatesRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	usage := func(input, output int) map[string]any {
		return map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": input + output, "cached_input_tokens": 0}
	}
	record := func(response string, input, total int) map[string]any {
		return map[string]any{"type": "token_usage_record", "payload": map[string]any{"thread_id": "thread", "turn_id": "turn", "response_id": response, "usage": usage(input, 1), "turn_token_usage": usage(total, 2), "thread_token_usage": usage(9999, 9)}}
	}
	items := []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "thread", "cli_version": "0.154.0", "model_provider": "dorf"}},
		{"type": "turn_context", "payload": map[string]any{"turn_id": "turn", "model": "model-a", "effort": "low"}},
		record("response-a", 10, 10),
		{"type": "turn_context", "payload": map[string]any{"turn_id": "turn", "model": "model-b", "effort": "high"}},
		record("response-b", 20, 30), record("response-b", 20, 30),
	}
	var contents []byte
	for _, item := range items {
		line, _ := json.Marshal(item)
		contents = append(contents, append(line, '\n')...)
	}
	contents = append(contents, []byte(`{"type":"unfinished`)...)
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	agent := Agent{Sandbox: continuitySandbox{}}
	turns := []TurnOutcome{{ID: "turn"}, {ID: "unmeasured"}}
	for i := 0; i < 2; i++ {
		if err := agent.readUsage(context.Background(), provider.Ownership{}, "thread", path, turns); err != nil {
			t.Fatal(err)
		}
		got := turns[0]
		if got.Usage == nil || *got.Usage.InputTokens != 30 || *got.Usage.TotalTokens != 32 {
			t.Fatalf("usage=%+v", got.Usage)
		}
		if got.Usage.OutputTokensDetails.ReasoningTokens != nil || *got.Usage.InputTokensDetails.CachedTokens != 0 {
			t.Fatal("unknown and zero counters were conflated")
		}
		if len(got.RequestUsage) != 2 || got.RequestUsage[0].Model != "model-a" || got.RequestUsage[1].Reasoning != "high" {
			t.Fatalf("requests=%+v", got.RequestUsage)
		}
		if !reflect.DeepEqual(turns[1], TurnOutcome{ID: "unmeasured"}) {
			t.Fatal("missing usage became zero")
		}
	}
	if err := agent.readUsage(context.Background(), provider.Ownership{}, "wrong-thread", path, turns); err == nil {
		t.Fatal("accepted another thread's records")
	}
}
