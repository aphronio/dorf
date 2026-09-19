package core

// TokenUsage preserves native counters. Detail counts are subsets of their
// parent counts; nil means the native source did not supply the measurement.
type TokenUsage struct {
	InputTokens         *int64              `json:"input_tokens"`
	OutputTokens        *int64              `json:"output_tokens"`
	TotalTokens         *int64              `json:"total_tokens"`
	InputTokensDetails  InputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails OutputTokensDetails `json:"output_tokens_details"`
}
type InputTokensDetails struct {
	CachedTokens     *int64 `json:"cached_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}
type OutputTokensDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens"`
}

// HarnessExecution describes the native execution rather than mutable Session defaults.
type HarnessExecution struct {
	Model          string `json:"model,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
	ModelProvider  string `json:"model_provider,omitempty"`
	HarnessVersion string `json:"harness_version,omitempty"`
}
type RequestUsage struct {
	ResponseID string     `json:"response_id"`
	Model      string     `json:"model,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"`
	Usage      TokenUsage `json:"usage"`
}
