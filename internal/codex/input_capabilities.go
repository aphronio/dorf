package codex

import (
	"context"
	"fmt"
	"slices"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

// InputCapabilities reuses the native catalog and its cache. Unknown models never
// gain audio support from a model name, default model, or protocol support alone.
func (a Agent) InputCapabilities(ctx context.Context, owner provider.Ownership, model string) (core.InputCapabilities, error) {
	ctx, cancel := a.timeoutContext(ctx)
	defer cancel()
	result := core.InputCapabilities{Model: model, AudioMediaTypes: []string{}}
	err := a.withServer(ctx, owner, func(p *protocol) error {
		var err error
		result, err = p.inputCapabilities(ctx, model)
		return err
	})
	return result, err
}

func (p *protocol) inputCapabilities(ctx context.Context, model string) (core.InputCapabilities, error) {
	result := core.InputCapabilities{Model: model, AudioMediaTypes: []string{}}
	cursor := ""
	seen := map[string]bool{}
	for {
		params := map[string]any{"includeHidden": true, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := p.call(ctx, "model/list", params)
		if err != nil {
			return result, err
		}
		models, ok := raw["data"].([]any)
		if !ok {
			return result, fmt.Errorf("model/list returned invalid data")
		}
		for _, value := range models {
			entry, ok := value.(map[string]any)
			if !ok || stringValue(entry["model"]) != model {
				continue
			}
			modalities, _ := entry["inputModalities"].([]any)
			if slices.Contains(modalities, any("audio")) {
				result.AudioMediaTypes = []string{"audio/wav", "audio/mpeg", "audio/mp4", "audio/webm", "audio/ogg"}
				result.MaxAudioBytes = provider.MaxFileWriteBytes
			}
			return result, nil
		}
		cursor = stringValue(raw["nextCursor"])
		if cursor == "" {
			return result, nil
		}
		if seen[cursor] {
			return result, fmt.Errorf("model/list repeated its cursor")
		}
		seen[cursor] = true
	}
}
