package openaichat

import (
	"encoding/json"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// maxBreakpoints is the most cache_control marks a request carries, Anthropic's limit, which servers that take the field share.
const maxBreakpoints = 4

// cachePlan says where a request with explicit caching puts cache_control: on system parts and on the last part of whole messages, by the order of mappings/anthropic_messages.md.
type cachePlan struct {
	system map[int]bool    // index among the non-empty system blocks
	msgs   map[string]bool // id of a source message whose last rendered message is marked
}

func planCache(req *provider.Request, msgs []message.Message) *cachePlan {
	if !req.Caps.PromptCache.Explicit {
		return nil
	}
	limit := maxBreakpoints
	if n := req.Caps.PromptCache.MaxBreakpoints; n > 0 && n < limit {
		limit = n
	}
	var sys []provider.SystemBlock
	for _, s := range req.System {
		if s.Text != "" {
			sys = append(sys, s)
		}
	}
	type mark struct {
		sys int
		msg string
	}
	var order []mark
	if len(sys) > 0 {
		order = append(order, mark{sys: len(sys) - 1})
	}
	rolling := rollingID(req, msgs)
	if rolling != "" {
		order = append(order, mark{sys: -1, msg: rolling})
	}
	shared := req.SharedID()
	if shared != "" && shared != rolling {
		order = append(order, mark{sys: -1, msg: shared})
	}
	for _, m := range msgs {
		if m.Origin.Kind == message.OriginEpochHead && m.ID != "" {
			if m.ID != rolling && m.ID != shared {
				order = append(order, mark{sys: -1, msg: m.ID})
			}
			break
		}
	}
	for i := len(sys) - 2; i >= 0; i-- {
		if sys[i].Cache {
			order = append(order, mark{sys: i})
			break
		}
	}
	p := &cachePlan{system: map[int]bool{}, msgs: map[string]bool{}}
	for i, m := range order {
		if i >= limit {
			break
		}
		if m.msg != "" {
			p.msgs[m.msg] = true
		} else {
			p.system[m.sys] = true
		}
	}
	return p
}

// rollingID is the last user-side message, of the prefix CacheTail bounds when it is set, so a side call marks where the main loop's request wrote its cache.
func rollingID(req *provider.Request, msgs []message.Message) string {
	if req.CacheTail > 0 && req.CacheTail <= len(req.Messages) {
		for i := req.CacheTail - 1; i >= 0; i-- {
			if m := req.Messages[i]; m.Role != message.RoleAssistant && m.ID != "" {
				return m.ID
			}
		}
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role != message.RoleAssistant && m.ID != "" {
			return m.ID
		}
	}
	return ""
}

// marked puts cache_control on the last content part of a rendered message, turning string content into one text part.
func marked(raw json.RawMessage) json.RawMessage {
	var w map[string]json.RawMessage
	if json.Unmarshal(raw, &w) != nil {
		return raw
	}
	cc := map[string]any{"type": "ephemeral"}
	var s string
	if json.Unmarshal(w["content"], &s) == nil {
		w["content"] = enc([]any{map[string]any{"type": "text", "text": s, "cache_control": cc}})
		return enc(w)
	}
	var parts []map[string]any
	if json.Unmarshal(w["content"], &parts) != nil || len(parts) == 0 {
		return raw
	}
	parts[len(parts)-1]["cache_control"] = cc
	w["content"] = enc(parts)
	return enc(w)
}
