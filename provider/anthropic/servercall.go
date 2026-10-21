package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/estonshi/aperture-potatos/message"
)

// dropOpenServerCalls leaves out a hosted call whose result will never come, and any result whose call is gone, because the API refuses an open server call unless only tool results follow it.
func dropOpenServerCalls(msgs []message.Message, pauseTail bool) []message.Message {
	drop := map[string]bool{}
	called := map[string]bool{}
	for i, m := range msgs {
		if m.Role != message.RoleAssistant {
			continue
		}
		for id := range openServerCalls(m.Content) {
			if !keepOpen(msgs, i, id, pauseTail) {
				drop[id] = true
			}
		}
		for _, b := range m.Content {
			if id, ok := serverCallID(b); ok && !drop[id] {
				called[id] = true
			}
		}
		for _, b := range m.Content {
			if id, ok := resultID(b); ok && !called[id] {
				drop[id] = true // a result whose call is not in the request
			}
		}
	}
	if len(drop) == 0 {
		return msgs
	}
	out := append([]message.Message(nil), msgs...)
	for i, m := range out {
		if m.Role != message.RoleAssistant {
			continue
		}
		kept := make(message.Content, 0, len(m.Content))
		for _, b := range m.Content {
			if id, ok := serverCallID(b); ok && drop[id] {
				continue
			}
			if id, ok := resultID(b); ok && drop[id] {
				continue
			}
			kept = append(kept, b)
		}
		out[i].Content = kept
	}
	return out
}

// keepOpen reports whether the open call id in msgs[i] may stay: a pause_turn resend of the last message, or a mixed call whose client results alone follow and whose result a later message carries or the server is about to produce.
func keepOpen(msgs []message.Message, i int, id string, pauseTail bool) bool {
	if i == len(msgs)-1 {
		return pauseTail
	}
	j := i + 1
	results := 0
	for ; j < len(msgs) && msgs[j].Role != message.RoleAssistant; j++ {
		for _, b := range msgs[j].Content {
			if _, ok := b.(*message.ToolResultBlock); !ok {
				return false
			}
			results++
		}
	}
	if results == 0 {
		return false
	}
	if j == len(msgs) {
		return true
	}
	for _, m := range msgs[j:] {
		for _, b := range m.Content {
			if rid, ok := resultID(b); ok && rid == id {
				return true
			}
		}
	}
	return false
}

// openServerCalls is the ids of the server tool calls in content that no result block in content answers.
func openServerCalls(content message.Content) map[string]bool {
	open := map[string]bool{}
	for _, b := range content {
		if id, ok := serverCallID(b); ok {
			open[id] = true
		}
	}
	for _, b := range content {
		if id, ok := resultID(b); ok {
			delete(open, id)
		}
	}
	return open
}

// serverCallID reads the id of a server-run call: a hosted call, or a server_tool_use, mcp_tool_use and the like kept as an opaque block.
func serverCallID(b message.Block) (string, bool) {
	switch v := b.(type) {
	case *message.HostedCallBlock:
		return v.ID, sameAPI(v.Native)
	case *message.OpaqueBlock:
		if !sameAPI(v.Native) {
			return "", false
		}
		var c struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(v.Native.Raw, &c) != nil || c.ID == "" || !strings.HasSuffix(c.Type, "_tool_use") {
			return "", false
		}
		return c.ID, true
	}
	return "", false
}

// resultID reads the call id a server-run result answers.
func resultID(b message.Block) (string, bool) {
	switch v := b.(type) {
	case *message.HostedResultBlock:
		return v.CallID, sameAPI(v.Native)
	case *message.OpaqueBlock:
		if !sameAPI(v.Native) {
			return "", false
		}
		var r struct {
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(v.Native.Raw, &r) != nil || r.ToolUseID == "" {
			return "", false
		}
		return r.ToolUseID, true
	}
	return "", false
}
