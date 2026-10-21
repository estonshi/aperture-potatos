// Package caps holds the capability gating every adapter applies the same way; it acts only on capabilities that are known.
package caps

import (
	"encoding/json"
	"slices"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// MaxOutput is the configured output cap, or the default, clamped to a known model limit.
func MaxOutput(p provider.Params, c provider.Capabilities) (n int, adjusted bool) {
	n = p.MaxOutputTokens
	if n <= 0 {
		n = provider.DefaultMaxOutputTokens
	}
	if c.MaxOutputTokens > 0 && n > c.MaxOutputTokens {
		return c.MaxOutputTokens, true
	}
	return n, false
}

// effortOrder ranks effort names so an unsupported one maps to its nearest supported neighbor.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// MapEffort keeps effort unless the model's supported efforts are known and exclude it; ties go toward high, the usual default.
func MapEffort(effort string, supported []string) (string, bool) {
	if effort == "" || len(supported) == 0 || slices.Contains(supported, effort) {
		return effort, false
	}
	at := slices.Index(effortOrder, effort)
	if at < 0 {
		return effort, false
	}
	high := slices.Index(effortOrder, "high")
	best, bestDist := "", 0
	for _, s := range supported {
		i := slices.Index(effortOrder, s)
		if i < 0 {
			continue
		}
		d := abs(i - at)
		if best == "" || d < bestDist || d == bestDist && abs(i-high) < abs(slices.Index(effortOrder, best)-high) {
			best, bestDist = s, d
		}
	}
	if best == "" {
		return effort, false
	}
	return best, true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// StrictOK reports whether a schema meets strict mode: every object closes additionalProperties and requires all its properties.
func StrictOK(schema json.RawMessage) bool {
	var v any
	if json.Unmarshal(schema, &v) != nil {
		return false
	}
	return strictNode(v)
}

func strictNode(v any) bool {
	switch n := v.(type) {
	case map[string]any:
		if n["type"] == "object" || n["properties"] != nil {
			if ap, ok := n["additionalProperties"].(bool); !ok || ap {
				return false
			}
			props, _ := n["properties"].(map[string]any)
			req, _ := n["required"].([]any)
			for name := range props {
				if !slices.Contains(req, any(name)) {
					return false
				}
			}
		}
		for _, child := range n {
			if !strictNode(child) {
				return false
			}
		}
	case []any:
		for _, child := range n {
			if !strictNode(child) {
				return false
			}
		}
	}
	return true
}

// Adjusted is the capability_adjusted notice for one gated field.
func Adjusted(field string, from, to any) provider.Notice {
	d, _ := json.Marshal(map[string]any{"field": field, "from": from, "to": to})
	return provider.Notice{Kind: "capability_adjusted", Detail: d}
}

// StrictDropped is one notice per tool declared strict whose schema does not meet strict mode, which the request sends without strict.
func StrictDropped(tools []message.ToolDef) []provider.Notice {
	var out []provider.Notice
	for _, t := range tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		if t.Strict && !StrictOK(schema) {
			out = append(out, Adjusted("tools."+t.Name+".strict", true, false))
		}
	}
	return out
}

// HostedUnsupported is the hosted_tool_unsupported notice for a hosted tool the request leaves out.
func HostedUnsupported(tool, reason string) provider.Notice {
	d, _ := json.Marshal(map[string]any{"tool": tool, "reason": reason})
	return provider.Notice{Kind: "hosted_tool_unsupported", Detail: d}
}

// HostedAdjusted is the hosted_tool_adjusted notice for a web search option the target API has no field for.
func HostedAdjusted(tool, field string, from any) provider.Notice {
	d, _ := json.Marshal(map[string]any{"tool": tool, "field": field, "from": from, "to": nil})
	return provider.Notice{Kind: "hosted_tool_adjusted", Detail: d}
}

// ApproxLocation is the user_location object both Anthropic and Responses accept for web search.
func ApproxLocation(l *message.UserLocation) map[string]any {
	out := map[string]any{"type": "approximate"}
	for k, v := range map[string]string{"city": l.City, "region": l.Region, "country": l.Country, "timezone": l.Timezone} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// ToolSearchSent reports that the request sends the neutral tool search, which is when its deferred tools are marked as such.
func ToolSearchSent(req *provider.Request) bool {
	return !req.Caps.NoToolSearch && slices.ContainsFunc(req.HostedTools, func(h message.HostedTool) bool { return h.ToolSearch != nil })
}
