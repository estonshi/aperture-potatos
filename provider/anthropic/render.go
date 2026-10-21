package anthropic

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"

	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/internal/caps"
	"github.com/estonshi/aperture-potatos/provider/internal/project"
)

const (
	maxBreakpoints   = 4 // the API's own limit for every model
	betaInterleaved  = "interleaved-thinking-2025-05-14"
	betaBlockBinding = "thinking-binding-controls-2026-08-01"
)

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString

type rendered struct {
	body    []byte
	header  http.Header
	notices []provider.Notice
	display string // the thinking display the request asked for, so the decoder can label reasoning blocks
}

// wblock is one wire content block; cache marks a cache_control breakpoint after it.
type wblock struct {
	raw   json.RawMessage
	cache bool
	src   string // id of the message it was rendered from, which CacheTail places the rolling breakpoint by
}

func (b *wblock) MarshalJSON() ([]byte, error) {
	if !b.cache {
		return b.raw, nil
	}
	out := append([]byte(nil), b.raw[:len(b.raw)-1]...)
	return append(out, `,"cache_control":{"type":"ephemeral"}}`...), nil
}

type wmessage struct {
	Role    string    `json:"role"`
	Content []*wblock `json:"content"`

	results int // leading tool_result blocks, which must stay first when user-side messages merge
}

// enc encodes without HTML escaping, so tags such as <system-reminder> reach the wire as written.
func enc(v any) json.RawMessage {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		panic(err) // every value encoded here is built from JSON-safe types
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

func localError(class provider.ErrorClass, msg string) *provider.Error {
	return &provider.Error{Class: class, API: provider.APIAnthropicMessages, Message: msg}
}

func (p *Provider) render(req *provider.Request) (*rendered, error) {
	r := &rendered{}
	family := req.Caps.Family
	if family == "" {
		family = req.Model
	}
	msgs, _ := project.ForTarget(req.Messages, project.Target{API: provider.APIAnthropicMessages, Endpoint: p.endpoint, Family: family})
	if req.Replay == provider.ReplayFallback && !req.Caps.ThinkingPrefixBound {
		msgs = project.StripReasoning(msgs, true)
	}
	msgs, healed := project.HealDangling(msgs, prompt.Text("dangling_tool_call"))
	if len(healed) > 0 {
		r.notices = append(r.notices, provider.Notice{Kind: "dangling_tool_call_healed", Detail: enc(map[string]any{"call_ids": healed})})
	}
	msgs = dropOpenServerCalls(msgs, req.PauseTail)
	msgs = project.RemapIDs(msgs, validID)
	if err := project.CheckTail(msgs, req.PauseTail); err != nil {
		return nil, localError(provider.ClassInvalidRequest, err.Error())
	}

	wire, epochHead := renderMessages(msgs)
	system := renderSystem(req.System)
	placeBreakpoints(system, wire, epochHead, tailID(req), req.SharedID(), req.Caps.PromptCache.MaxBreakpoints)

	body := map[string]any{
		"model":    req.Model,
		"stream":   true,
		"messages": wire,
	}
	if len(system) > 0 {
		body["system"] = system
	}
	maxTokens, adjusted := caps.MaxOutput(req.Params, req.Caps)
	if adjusted {
		r.notices = append(r.notices, caps.Adjusted("max_output_tokens", req.Params.MaxOutputTokens, maxTokens))
	}
	body["max_tokens"] = maxTokens
	deferred := caps.ToolSearchSent(req)
	tools := append(renderTools(req.Tools, deferred), removedTools(msgs, req.Tools, deferred)...)
	if tools = append(tools, renderHosted(req, &r.notices)...); len(tools) > 0 {
		body["tools"] = tools
		r.notices = append(r.notices, caps.StrictDropped(req.Tools)...)
	}
	var betas []string
	th, err := thinking(req, maxTokens, &r.notices)
	if err != nil {
		return nil, err
	}
	if th.body != nil {
		body["thinking"] = th.body
		r.display, _ = th.body["display"].(string)
	}
	if th.manual && len(req.Tools) > 0 && req.Caps.Reasoning.InterleavedBeta {
		betas = append(betas, betaInterleaved)
	}
	if req.Replay == provider.ReplayFallback && req.Caps.ThinkingPrefixBound {
		tb := th.body
		if tb == nil {
			tb = map[string]any{"type": "adaptive"}
			body["thinking"] = tb
		}
		tb["block_binding"] = map[string]any{"prefix_mismatch_behavior": "drop_block"}
		betas = append(betas, betaBlockBinding)
	}
	if e := req.Params.Reasoning.Effort; e != "" {
		mapped, changed := caps.MapEffort(e, req.Caps.Reasoning.Efforts)
		if changed {
			r.notices = append(r.notices, caps.Adjusted("effort", e, mapped))
		}
		body["output_config"] = map[string]any{"effort": mapped}
	}
	sampling(req, th.on, body, &r.notices)
	if len(req.Params.StopSequences) > 0 {
		body["stop_sequences"] = req.Params.StopSequences
	}
	if tc := toolChoice(req, th.manual, &r.notices); tc != nil {
		body["tool_choice"] = tc
	}
	for k, v := range req.Params.Extra {
		body[k] = v
	}
	if tb, ok := body["thinking"].(map[string]any); ok {
		r.display, _ = tb["display"].(string)
	}
	r.body = enc(body)
	r.header = p.headers(req.Params.Headers, betas)
	return r, nil
}

type thinkingPlan struct {
	body   map[string]any
	on     bool // the request turns thinking on, which is when sampling limits apply
	manual bool // budget_tokens form, which forbids forced tool_choice and may need the interleaved beta
}

// thinking picks the thinking form from the declared modes, or, when they are unknown, from whether a budget was given.
func thinking(req *provider.Request, maxTokens int, notices *[]provider.Notice) (thinkingPlan, error) {
	rp, rc := req.Params.Reasoning, req.Caps.Reasoning
	known := len(rc.Modes) > 0
	switch rp.Mode {
	case provider.ReasoningOff:
		if known && rc.NoDisable {
			*notices = append(*notices, caps.Adjusted("reasoning.mode", "off", "model default"))
			return thinkingPlan{}, nil
		}
		return thinkingPlan{body: map[string]any{"type": "disabled"}}, nil
	case provider.ReasoningOn:
	default:
		if !rc.DefaultOn {
			return thinkingPlan{}, nil // leave the model's default behavior alone
		}
	}
	manual := rp.Budget > 0
	if known {
		adaptive, enabled := slices.Contains(rc.Modes, "adaptive"), slices.Contains(rc.Modes, "enabled")
		switch {
		case manual && !enabled && adaptive:
			*notices = append(*notices, caps.Adjusted("reasoning.form", "enabled", "adaptive"))
			manual = false
		case !manual && !adaptive && enabled:
			manual = true
		}
	}
	if !manual {
		display := "summarized"
		if rp.Hide {
			display = "omitted"
		}
		return thinkingPlan{body: map[string]any{"type": "adaptive", "display": display}, on: true}, nil
	}
	budget := rp.Budget
	if budget <= 0 {
		budget = maxTokens / 2
	}
	lo := max(rc.BudgetMin, 1)
	hi := maxTokens - 1 // budget_tokens must stay below max_tokens on every model
	if lo > hi {
		return thinkingPlan{}, localError(provider.ClassCapability, "no thinking budget fits below max_tokens; raise Params.MaxOutputTokens")
	}
	if clamped := min(max(budget, lo), hi); clamped != budget {
		*notices = append(*notices, caps.Adjusted("reasoning.budget", budget, clamped))
		budget = clamped
	}
	return thinkingPlan{body: map[string]any{"type": "enabled", "budget_tokens": budget}, on: true, manual: true}, nil
}

func sampling(req *provider.Request, thinkingOn bool, body map[string]any, notices *[]provider.Notice) {
	t, p := req.Params.Temperature, req.Params.TopP
	if t == nil && p == nil {
		return
	}
	c := req.Caps
	switch {
	case c.NoSampling:
		*notices = append(*notices, caps.Adjusted("sampling", "set", "removed"))
		return
	case thinkingOn && c.SamplingWithThinking == "none":
		*notices = append(*notices, caps.Adjusted("sampling", "set", "removed while thinking"))
		return
	case thinkingOn && c.SamplingWithThinking == "top_p_only":
		if t != nil {
			*notices = append(*notices, caps.Adjusted("temperature", *t, nil))
			t = nil
		}
		if p != nil {
			if v := min(max(*p, 0.95), 1); v != *p {
				*notices = append(*notices, caps.Adjusted("top_p", *p, v))
				p = &v
			}
		}
	}
	if t != nil {
		body["temperature"] = *t
	}
	if p != nil {
		body["top_p"] = *p
	}
}

func toolChoice(req *provider.Request, manualThinking bool, notices *[]provider.Notice) map[string]any {
	tc := req.ToolChoice
	if len(req.Tools) == 0 || tc.Mode == "" && !tc.DisableParallel {
		return nil
	}
	var out map[string]any
	switch tc.Mode {
	case provider.ToolChoiceNone:
		return map[string]any{"type": "none"}
	case provider.ToolChoiceRequired:
		out = map[string]any{"type": "any"}
	case provider.ToolChoiceNamed:
		out = map[string]any{"type": "tool", "name": tc.Name}
	default:
		out = map[string]any{"type": "auto"}
	}
	if out["type"] != "auto" && (req.Caps.NoForcedToolChoice || manualThinking) {
		*notices = append(*notices, caps.Adjusted("tool_choice", out["type"], "auto"))
		out = map[string]any{"type": "auto"}
	}
	if tc.DisableParallel {
		out["disable_parallel_tool_use"] = true
	}
	return out
}

// renderTools marks deferred definitions only when the request sends a tool search; without one they load like any tool.
func renderTools(tools []message.ToolDef, deferred bool) []json.RawMessage {
	out := make([]json.RawMessage, len(tools))
	for i, t := range tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		w := map[string]any{"name": t.Name, "description": t.Description, "input_schema": schema}
		if t.Strict && caps.StrictOK(schema) {
			w["strict"] = true
		}
		if deferred && t.Deferred {
			w["defer_loading"] = true
		}
		out[i] = enc(w)
	}
	return out
}

// renderHosted maps the neutral web search to the basic web_search tool every search-capable model knows, the neutral tool search to BM25, and appends raw definitions for this API.
func renderHosted(req *provider.Request, notices *[]provider.Notice) []json.RawMessage {
	var out []json.RawMessage
	for _, h := range req.HostedTools {
		switch ws := h.WebSearch; {
		case ws != nil:
			if req.Caps.NoWebSearch {
				*notices = append(*notices, caps.HostedUnsupported(h.Name, "no_web_search"))
				continue
			}
			w := map[string]any{"type": "web_search_20250305", "name": h.Name}
			if ws.MaxUses > 0 {
				w["max_uses"] = ws.MaxUses
			}
			if len(ws.AllowedDomains) > 0 {
				w["allowed_domains"] = ws.AllowedDomains
			}
			if len(ws.BlockedDomains) > 0 {
				w["blocked_domains"] = ws.BlockedDomains
			}
			if ws.UserLocation != nil {
				w["user_location"] = caps.ApproxLocation(ws.UserLocation)
			}
			if ws.ContextSize != "" {
				*notices = append(*notices, caps.HostedAdjusted(h.Name, "context_size", ws.ContextSize))
			}
			out = append(out, enc(w))
		case h.ToolSearch != nil:
			if req.Caps.NoToolSearch {
				*notices = append(*notices, caps.HostedUnsupported(h.Name, "no_tool_search"))
				continue
			}
			out = append(out, toolSearchDef)
		case len(h.Raw[string(provider.APIAnthropicMessages)]) > 0:
			out = append(out, h.Raw[string(provider.APIAnthropicMessages)])
		default:
			*notices = append(*notices, caps.HostedUnsupported(h.Name, "no_definition"))
		}
	}
	return out
}

func renderSystem(sys []provider.SystemBlock) []*sysBlock {
	var out []*sysBlock
	for _, s := range sys {
		if s.Text == "" {
			continue
		}
		out = append(out, &sysBlock{wblock: wblock{raw: enc(map[string]any{"type": "text", "text": s.Text})}, marked: s.Cache})
	}
	return out
}

type sysBlock struct {
	wblock
	marked bool // the kernel's cut between agent-level and session-level sections (B4)
}

func (b *sysBlock) MarshalJSON() ([]byte, error) { return b.wblock.MarshalJSON() }

// placeBreakpoints follows spec/mappings/anthropic_messages.md: system end, rolling user tail, epoch head, then the agent/session cut.
func placeBreakpoints(system []*sysBlock, wire []*wmessage, epochHead *wblock, tail, shared string, declared int) {
	limit := maxBreakpoints
	if declared > 0 && declared < limit {
		limit = declared
	}
	var order []*wblock
	if n := len(system); n > 0 {
		order = append(order, &system[n-1].wblock)
	}
	rolling := lastUserBlock(wire, tail)
	if rolling != nil {
		order = append(order, rolling)
	}
	var sharedEnd *wblock
	if shared != "" {
		if sharedEnd = lastUserBlock(wire, shared); sharedEnd != nil && sharedEnd != rolling {
			order = append(order, sharedEnd)
		}
	}
	if epochHead != nil && epochHead != rolling && epochHead != sharedEnd {
		order = append(order, epochHead)
	}
	for i := len(system) - 2; i >= 0; i-- {
		if system[i].marked {
			order = append(order, &system[i].wblock)
			break
		}
	}
	for i, b := range order {
		b.cache = i < limit
	}
}

// lastUserBlock is the last block of the last user-side wire message, or with src set the last block rendered from that source message, even when it merged with a later one.
func lastUserBlock(wire []*wmessage, src string) *wblock {
	for i := len(wire) - 1; i >= 0; i-- {
		if wire[i].Role != "user" || len(wire[i].Content) == 0 {
			continue
		}
		if src == "" {
			return wire[i].Content[len(wire[i].Content)-1]
		}
		for k := len(wire[i].Content) - 1; k >= 0; k-- {
			if wire[i].Content[k].src == src {
				return wire[i].Content[k]
			}
		}
	}
	return nil
}

// tailID names the message whose last block carries the rolling breakpoint when CacheTail bounds it: the last user-side message of the bounded prefix.
func tailID(req *provider.Request) string {
	if req.CacheTail <= 0 || req.CacheTail > len(req.Messages) {
		return ""
	}
	for i := req.CacheTail - 1; i >= 0; i-- {
		if m := req.Messages[i]; m.Role != message.RoleAssistant && m.ID != "" {
			return m.ID
		}
	}
	return ""
}

// renderMessages renders each message, skips empty ones and merges adjacent user-side messages with tool results first.
func renderMessages(msgs []message.Message) ([]*wmessage, *wblock) {
	var out []*wmessage
	var epochHead *wblock
	for _, m := range msgs {
		role := "user"
		if m.Role == message.RoleAssistant {
			role = "assistant"
		}
		var results, rest []*wblock
		for _, b := range m.Content {
			raw := renderBlock(b, role == "assistant")
			if raw == nil {
				continue
			}
			wb := &wblock{raw: raw, src: m.ID}
			if _, ok := b.(*message.ToolResultBlock); ok {
				results = append(results, wb)
			} else {
				rest = append(rest, wb)
			}
		}
		if len(results)+len(rest) == 0 {
			continue
		}
		if m.Origin.Kind == message.OriginEpochHead && epochHead == nil {
			all := append(append([]*wblock(nil), results...), rest...)
			epochHead = all[len(all)-1]
		}
		if n := len(out); n > 0 && role == "user" && out[n-1].Role == "user" {
			prev := out[n-1]
			merged := append(append([]*wblock(nil), prev.Content[:prev.results]...), results...)
			merged = append(merged, prev.Content[prev.results:]...)
			prev.Content = append(merged, rest...)
			prev.results += len(results)
			continue
		}
		out = append(out, &wmessage{Role: role, Content: append(results, rest...), results: len(results)})
	}
	return out, epochHead
}

func sameAPI(n *message.Native) bool {
	return n != nil && provider.API(n.API) == provider.APIAnthropicMessages && len(n.Raw) > 0
}

func renderBlock(b message.Block, assistant bool) json.RawMessage {
	switch v := b.(type) {
	case *message.TextBlock:
		if sameAPI(v.Native) {
			return v.Native.Raw
		}
		if v.Text == "" {
			return nil
		}
		return enc(map[string]any{"type": "text", "text": v.Text})
	case *message.RefusalBlock:
		if v.Text == "" {
			return nil
		}
		return enc(map[string]any{"type": "text", "text": v.Text})
	case *message.ReminderBlock:
		return enc(map[string]any{"type": "text", "text": prompt.EncodeReminder(*v)})
	case *message.ReasoningBlock:
		if assistant && sameAPI(v.Native) {
			return v.Native.Raw
		}
	case *message.OpaqueBlock:
		if sameAPI(v.Native) {
			return v.Native.Raw
		}
	case *message.HostedCallBlock:
		if assistant && sameAPI(v.Native) {
			return v.Native.Raw
		}
	case *message.HostedResultBlock:
		if assistant && sameAPI(v.Native) {
			return v.Native.Raw
		}
	case *message.ToolCallBlock:
		if !assistant {
			return nil
		}
		input := v.Input
		if !isObject(input) {
			input = json.RawMessage(`{}`)
		}
		return enc(map[string]any{"type": "tool_use", "id": v.ID, "name": v.Name, "input": input})
	case *message.ToolResultBlock:
		if assistant {
			return nil
		}
		w := map[string]any{"type": "tool_result", "tool_use_id": v.CallID}
		var content []json.RawMessage
		for _, c := range v.Content {
			if raw := renderBlock(c, false); raw != nil {
				content = append(content, raw)
			}
		}
		if len(content) > 0 {
			w["content"] = content
		}
		if v.IsError {
			w["is_error"] = true
		}
		return enc(w)
	case *message.ImageBlock:
		if assistant {
			return nil
		}
		return enc(map[string]any{"type": "image", "source": source(v.MediaType, v.Data, v.URL)})
	case *message.DocumentBlock:
		if assistant {
			return nil
		}
		w := map[string]any{"type": "document", "source": source(v.MediaType, v.Data, v.URL)}
		if v.Filename != "" {
			w["title"] = v.Filename
		}
		return enc(w)
	}
	return nil
}

func source(mediaType string, data []byte, url string) map[string]any {
	if len(data) > 0 {
		return map[string]any{"type": "base64", "media_type": mediaType, "data": base64.StdEncoding.EncodeToString(data)}
	}
	return map[string]any{"type": "url", "url": url}
}

func isObject(b json.RawMessage) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 1 && b[0] == '{' && json.Valid(b)
}
