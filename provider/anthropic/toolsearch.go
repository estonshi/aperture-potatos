package anthropic

import (
	"encoding/json"
	"slices"

	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// toolSearchName is what the BM25 tool search, the neutral one's definition, is called on the wire.
const toolSearchName = "tool_search_tool_bm25"

var toolSearchDef = json.RawMessage(`{"type":"tool_search_tool_bm25_20251119","name":"` + toolSearchName + `"}`)

// removedTools stands in for each tool a replayed search loaded that the request no longer has, since the API refuses a reference to a missing definition.
func removedTools(msgs []message.Message, tools []message.ToolDef, deferred bool, texts provider.TextSource) []json.RawMessage {
	have := map[string]bool{}
	for _, t := range tools {
		have[t.Name] = true
	}
	var missing []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if r, ok := b.(*message.HostedResultBlock); ok && r.Native != nil {
				for _, name := range r.Tools {
					if !have[name] {
						have[name] = true
						missing = append(missing, name)
					}
				}
			}
		}
	}
	slices.Sort(missing)
	out := make([]json.RawMessage, len(missing))
	for i, name := range missing {
		w := map[string]any{"name": name, "description": prompt.TextFrom(texts, "tool_removed"), "input_schema": json.RawMessage(`{"type":"object"}`)}
		if deferred {
			w["defer_loading"] = true
		}
		out[i] = enc(w)
	}
	return out
}

// foundTools reads a tool_search_tool_result's content: the tools it references, or an error object with an error_code.
func foundTools(raw json.RawMessage) ([]string, string) {
	var c struct {
		ErrorCode  string `json:"error_code"`
		References []struct {
			ToolName string `json:"tool_name"`
		} `json:"tool_references"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return nil, ""
	}
	var names []string
	for _, r := range c.References {
		if r.ToolName != "" {
			names = append(names, r.ToolName)
		}
	}
	return names, c.ErrorCode
}
