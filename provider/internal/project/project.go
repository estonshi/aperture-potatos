// Package project reshapes canonical history for one request without touching the history itself; every adapter runs the same steps.
package project

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// Target is what the projected history will be sent to.
type Target = provider.ReplayTarget

type Stats struct {
	DroppedReasoning int
	DroppedOpaque    int // hosted blocks count here too, since they were opaque before they had types of their own
}

// family is the producer's family, which defaults to its model name.
func family(p *message.Producer) string {
	if p == nil {
		return ""
	}
	if p.Family != "" {
		return p.Family
	}
	return p.Model
}

// ForTarget drops or strips what the target may not receive by the replay matrix in spec/mappings/projection.md.
func ForTarget(msgs []message.Message, t Target) ([]message.Message, Stats) {
	var st Stats
	out := make([]message.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != message.RoleAssistant {
			out = append(out, m)
			continue
		}
		sameAPI := m.Producer != nil && provider.API(m.Producer.API) == t.API
		replayable := sameAPI && (t.API != provider.APIOpenAIChat || m.Producer.Endpoint == t.Endpoint && family(m.Producer) == t.Family)
		content := make(message.Content, 0, len(m.Content))
		for _, b := range m.Content {
			switch v := b.(type) {
			case *message.ReasoningBlock:
				if !provider.Replays(v, m.Producer, t) {
					st.DroppedReasoning++
					continue
				}
			case *message.OpaqueBlock, *message.HostedCallBlock, *message.HostedResultBlock:
				if !provider.Replays(v, m.Producer, t) {
					st.DroppedOpaque++
					continue
				}
			case *message.TextBlock:
				if v.Native != nil && provider.API(v.Native.API) != t.API {
					c := *v
					c.Native = nil
					b = &c
				}
			case *message.RefusalBlock:
				if v.Native != nil && provider.API(v.Native.API) != t.API {
					c := *v
					c.Native = nil
					b = &c
				}
			case *message.ToolCallBlock:
				if v.Native != nil && provider.API(v.Native.API) != t.API {
					c := *v
					c.Native = nil
					b = &c
				}
			}
			content = append(content, b)
		}
		m.Content = content
		if m.Native != nil && !(replayable && provider.API(m.Native.API) == t.API) {
			m.Native = nil
		}
		out = append(out, m)
	}
	return out, st
}

// HealDangling gives every tool call without a result a not_executed result, and drops results no call asked for, so the request is valid for every API (I1).
func HealDangling(msgs []message.Message, text string) ([]message.Message, []string) {
	var healed []string
	out := make([]message.Message, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role == message.RoleTool {
			continue // results are rebuilt below from the assistant message they answer
		}
		out = append(out, m)
		calls := m.ToolCalls()
		if m.Role != message.RoleAssistant || len(calls) == 0 {
			continue
		}
		var have map[string]message.Block
		var tool message.Message
		if i+1 < len(msgs) && msgs[i+1].Role == message.RoleTool {
			tool = msgs[i+1]
			have = map[string]message.Block{}
			for _, b := range tool.Content {
				if r, ok := b.(*message.ToolResultBlock); ok {
					have[r.CallID] = b
				}
			}
			i++
		} else {
			tool = message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, TurnID: m.TurnID, SessionID: m.SessionID, Epoch: m.Epoch}
		}
		content := make(message.Content, 0, len(calls))
		changed := len(tool.Content) != len(calls)
		for _, c := range calls {
			if b, ok := have[c.ID]; ok {
				content = append(content, b)
				continue
			}
			changed = true
			healed = append(healed, c.ID)
			content = append(content, &message.ToolResultBlock{
				CallID: c.ID, Name: c.Name, IsError: true, Outcome: message.OutcomeNotExecuted,
				Content: message.Content{&message.TextBlock{Text: text}},
			})
		}
		if changed {
			tool.Content = content
		}
		out = append(out, tool)
	}
	return out, healed
}

// ErrEndsWithAssistant is the local error for a request that would end with an assistant message outside a pause_turn continuation (I9).
var ErrEndsWithAssistant = errors.New("the request would end with an assistant message")

func CheckTail(msgs []message.Message, pauseTail bool) error {
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == message.RoleAssistant && !pauseTail {
		return ErrEndsWithAssistant
	}
	return nil
}

// RemapIDs rewrites call ids the target cannot accept to h_ plus 24 base32 characters of their sha256, identically in calls and results.
func RemapIDs(msgs []message.Message, valid func(string) bool) []message.Message {
	remap := func(id string) (string, bool) {
		if valid(id) {
			return id, false
		}
		sum := sha256.Sum256([]byte(id))
		return "h_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])[:24], true
	}
	out := make([]message.Message, len(msgs))
	for i, m := range msgs {
		var content message.Content
		for j, b := range m.Content {
			var nb message.Block
			switch v := b.(type) {
			case *message.ToolCallBlock:
				if id, ok := remap(v.ID); ok {
					c := *v
					c.ID = id
					nb = &c
				}
			case *message.ToolResultBlock:
				if id, ok := remap(v.CallID); ok {
					c := *v
					c.CallID = id
					nb = &c
				}
			}
			if nb != nil {
				if content == nil {
					content = append(message.Content(nil), m.Content...)
				}
				content[j] = nb
			}
		}
		if content != nil {
			m.Content = content
		}
		out[i] = m
	}
	return out
}

// StripReasoning removes reasoning, opaque and hosted blocks from assistant messages, keeping the latest assistant message intact when keepLatest is set.
func StripReasoning(msgs []message.Message, keepLatest bool) []message.Message {
	latest := -1
	if keepLatest {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == message.RoleAssistant {
				latest = i
				break
			}
		}
	}
	out := make([]message.Message, len(msgs))
	for i, m := range msgs {
		if m.Role == message.RoleAssistant && i != latest {
			content := make(message.Content, 0, len(m.Content))
			for _, b := range m.Content {
				switch b.(type) {
				case *message.ReasoningBlock, *message.OpaqueBlock, *message.HostedCallBlock, *message.HostedResultBlock:
					continue
				}
				content = append(content, b)
			}
			m.Content = content
		}
		out[i] = m
	}
	return out
}
