package message

import "encoding/json"

// Usage is per model call; InputTokens always counts the whole prompt including cached tokens.
type Usage struct {
	InputTokens      int64            `json:"input_tokens"`
	CacheReadTokens  int64            `json:"cache_read_tokens"`
	CacheWriteTokens int64            `json:"cache_write_tokens"`
	OutputTokens     int64            `json:"output_tokens"` // includes reasoning tokens
	ReasoningTokens  int64            `json:"reasoning_tokens"`
	HostedCalls      map[string]int64 `json:"hosted_calls,omitempty"` // hosted tool name → calls the provider ran and bills separately
	Estimated        bool             `json:"estimated,omitempty"`    // stream broke before the provider reported usage
	Raw              json.RawMessage  `json:"raw,omitempty"`          // provider usage verbatim (iterations, cost, speed ...)
}
