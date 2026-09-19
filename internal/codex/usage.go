package codex

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

// readUsage projects native rollout records without copying history into Dorf.
// The app-server's thread/read supplies the exact file and retained Turn IDs.
func (a Agent) readUsage(ctx context.Context, owner provider.Ownership, threadID, path string, turns []TurnOutcome) error {
	if path == "" || len(turns) == 0 {
		return nil
	}
	result, err := a.Sandbox.Exec(ctx, owner, nil, "python3", "-c", readNativeUsage, path, threadID)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 || result.Truncated {
		return fmt.Errorf("read persisted Codex usage failed")
	}
	var records map[string]struct {
		Usage     *core.TokenUsage       `json:"usage"`
		Execution *core.HarnessExecution `json:"execution"`
		Requests  []core.RequestUsage    `json:"request_usage_entries"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &records); err != nil {
		return fmt.Errorf("decode persisted Codex usage: %w", err)
	}
	for i := range turns {
		record := records[turns[i].ID]
		turns[i].Usage, turns[i].Execution, turns[i].RequestUsage = record.Usage, record.Execution, record.Requests
	}
	return nil
}

// Codex 0.154 persists token_usage_record even though thread/read omits it.
// Use the native Turn aggregate, never differences of context-window counters.
// Project inside the Sandbox so conversation content never enters the response.
const readNativeUsage = `import json,sys
path,thread=sys.argv[1:]
turns={}
meta={}
def usage(value):
    if value is None: return None
    keys=('input_tokens','output_tokens','total_tokens','cached_input_tokens','cache_write_input_tokens','reasoning_output_tokens')
    for key in keys:
        count=value.get(key)
        if count is not None and (type(count) is not int or count<0): raise ValueError('invalid native counter')
    return dict(input_tokens=value.get('input_tokens'),output_tokens=value.get('output_tokens'),total_tokens=value.get('total_tokens'),
        input_tokens_details=dict(cached_tokens=value.get('cached_input_tokens'),cache_write_tokens=value.get('cache_write_input_tokens')),
        output_tokens_details=dict(reasoning_tokens=value.get('reasoning_output_tokens')))
with open(path,encoding='utf-8') as source:
    for line in source:
        # A concurrently appended final line is not yet a native record.
        if not line.endswith('\n'): break
        item=json.loads(line)
        payload=item.get('payload') or {}
        kind=item.get('type')
        if kind=='session_meta':
            if payload.get('id')!=thread: raise ValueError('wrong native thread')
            meta=dict(harness_version=payload.get('cli_version'),model_provider=payload.get('model_provider'))
        if not meta: raise ValueError('missing native identity')
        turn=payload.get('turn_id')
        if not turn or kind not in ('turn_context','token_usage_record'): continue
        record=turns.setdefault(turn,dict(execution=dict(meta),request_usage_entries=[]))
        if kind=='turn_context':
            record['execution'].update(model=payload.get('model'),reasoning=payload.get('effort'))
        else:
            if payload.get('thread_id')!=thread: continue
            record['usage']=usage(payload.get('turn_token_usage'))
            request=dict(response_id=payload['response_id'],usage=usage(payload['usage']))
            request.update({k:record['execution'].get(k) for k in ('model','reasoning')})
            entries=record['request_usage_entries']
            entries[:]=[entry for entry in entries if entry['response_id']!=request['response_id']]
            entries.append(request)
print(json.dumps(turns))
`
