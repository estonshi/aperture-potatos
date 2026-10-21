package openairesponses

import (
	"encoding/json"
	"slices"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// maxBreakpoints is the most cache writes one request may create.
const maxBreakpoints = 4

// cacheMarks names the source messages whose last rendered item carries an explicit breakpoint, by the order of mappings/anthropic_messages.md without its system marks, since instructions take none.
func cacheMarks(req *provider.Request, msgs []message.Message) map[string]bool {
	if !req.Caps.PromptCache.Explicit {
		return nil
	}
	limit := maxBreakpoints
	if n := req.Caps.PromptCache.MaxBreakpoints; n > 0 && n < limit {
		limit = n
	}
	var order []string
	if id := rollingID(req, msgs); id != "" {
		order = append(order, id)
	}
	if id := req.SharedID(); id != "" && !slices.Contains(order, id) {
		order = append(order, id)
	}
	for _, m := range msgs {
		if m.Origin.Kind == message.OriginEpochHead && m.ID != "" {
			if !slices.Contains(order, m.ID) {
				order = append(order, m.ID)
			}
			break
		}
	}
	marks := map[string]bool{}
	for i, id := range order {
		if i < limit {
			marks[id] = true
		}
	}
	return marks
}

// rollingID is the last user-side message, of the prefix CacheTail bounds when it is set, so a side call marks where the main loop's request wrote its cache.
func rollingID(req *provider.Request, msgs []message.Message) string {
	src := msgs
	if req.CacheTail > 0 && req.CacheTail <= len(req.Messages) {
		src = req.Messages[:req.CacheTail]
	}
	for i := len(src) - 1; i >= 0; i-- {
		if m := src[i]; m.Role != message.RoleAssistant && m.ID != "" {
			return m.ID
		}
	}
	return ""
}

var breakpoint = map[string]any{"mode": "explicit"}

// marked puts the breakpoint on an item's last content part; a function_call_output's string output becomes one input_text part first.
func marked(raw json.RawMessage) json.RawMessage {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return raw
	}
	key := "content"
	if _, ok := item["output"]; ok {
		key = "output"
	}
	var s string
	if json.Unmarshal(item[key], &s) == nil {
		if s == "" {
			return raw
		}
		item[key] = enc([]any{map[string]any{"type": "input_text", "text": s, "prompt_cache_breakpoint": breakpoint}})
		return enc(item)
	}
	var parts []map[string]any
	if json.Unmarshal(item[key], &parts) != nil || len(parts) == 0 {
		return raw
	}
	parts[len(parts)-1]["prompt_cache_breakpoint"] = breakpoint
	item[key] = enc(parts)
	return enc(item)
}
