package mcp

import (
	"encoding/json"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
)

// ApprovalRules returns rules as given plus, for each rule that may name a server tool, a mirror on mcp_call_tool, so discovery mode reaches the same verdicts.
func ApprovalRules(rules ...kernel.ApprovalRule) []kernel.ApprovalRule {
	out := append([]kernel.ApprovalRule(nil), rules...)
	for _, r := range rules {
		if !strings.HasPrefix(r.Tool, "mcp__") && !strings.HasPrefix(r.Tool, "*") && !strings.HasPrefix(r.Tool, "?") {
			continue
		}
		m := kernel.ApprovalRule{ID: r.ID + ".via_call_tool", Tool: callTool, Action: r.Action, Reason: r.Reason,
			When: map[string]kernel.Condition{"name": {Glob: r.Tool}}}
		for k, c := range r.When {
			m.When["arguments."+k] = c
		}
		if r.Match != nil {
			match := r.Match
			m.Match = func(call kernel.ToolCallView) bool {
				var in callInput
				if json.Unmarshal(call.Input, &in) != nil {
					return false
				}
				return match(kernel.ToolCallView{CallID: call.CallID, Name: in.Name, Input: in.Arguments})
			}
		}
		out = append(out, m)
	}
	return out
}
