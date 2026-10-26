package openairesponses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/internal/caps"
	"github.com/estonshi/aperture-potatos/provider/internal/project"
)

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString

type rendered struct {
	body    []byte
	header  http.Header
	notices []provider.Notice
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
	return &provider.Error{Class: class, API: api, Message: msg}
}

func (p *Provider) render(req *provider.Request) (*rendered, error) {
	r := &rendered{}
	family := req.Caps.Family
	if family == "" {
		family = req.Model
	}
	msgs, _ := project.ForTarget(req.Messages, project.Target{API: api, Endpoint: p.endpoint, Family: family})
	if req.Replay == provider.ReplayFallback {
		msgs = dropReasoning(msgs)
	}
	msgs, healed := project.HealDangling(msgs, prompt.TextFrom(req.Texts, "dangling_tool_call"))
	if len(healed) > 0 {
		r.notices = append(r.notices, provider.Notice{Kind: "dangling_tool_call_healed", Detail: enc(map[string]any{"call_ids": healed})})
	}
	msgs = project.RemapIDs(msgs, validID)
	if err := project.CheckTail(msgs, req.PauseTail); err != nil {
		return nil, localError(provider.ClassInvalidRequest, err.Error())
	}

	body := map[string]any{
		"model":          req.Model,
		"stream":         true,
		"stream_options": map[string]any{"include_obfuscation": false},
		"store":          false,
		"truncation":     "disabled", // the runtime manages the context, so the server must never drop items on its own
		"input":          renderItems(msgs, cacheMarks(req, msgs)),
	}
	if req.Caps.PromptCache.Explicit {
		body["prompt_cache_options"] = map[string]any{"mode": "explicit"}
	}
	var sys []string
	for _, s := range req.System {
		if s.Text != "" {
			sys = append(sys, s.Text)
		}
	}
	if len(sys) > 0 {
		body["instructions"] = strings.Join(sys, "\n\n") // resent with every request, since nothing is stored
	}
	maxTokens, adjusted := caps.MaxOutput(req.Params, req.Caps)
	if adjusted {
		r.notices = append(r.notices, caps.Adjusted("max_output_tokens", req.Params.MaxOutputTokens, maxTokens))
	}
	body["max_output_tokens"] = maxTokens
	var include []string
	hosted, search := renderHosted(req, &r.notices)
	if search {
		include = append(include, "web_search_call.action.sources") // without it a search call names only some of the pages it read
	}
	if tools := append(renderTools(req.Tools, caps.ToolSearchSent(req)), hosted...); len(tools) > 0 {
		body["tools"] = tools
	}
	if len(req.Tools) > 0 {
		r.notices = append(r.notices, caps.StrictDropped(req.Tools)...)
		if tc := toolChoice(req, &r.notices); tc != nil {
			body["tool_choice"] = tc
		}
		if req.ToolChoice.DisableParallel {
			body["parallel_tool_calls"] = false
		}
	}
	if rs := reasoning(req, &r.notices); rs != nil {
		body["reasoning"] = rs
		include = append(include, "reasoning.encrypted_content") // without it a stateless request never gets reasoning it can replay
	}
	if len(include) > 0 {
		body["include"] = include
	}
	sampling(req, body, &r.notices)
	if len(req.Params.StopSequences) > 0 {
		r.notices = append(r.notices, caps.Adjusted("stop_sequences", req.Params.StopSequences, nil)) // the API has no stop parameter
	}
	if req.CacheKey != "" {
		body["prompt_cache_key"] = req.CacheKey
	}
	for k, v := range req.Params.Extra {
		body[k] = v
	}
	r.body = enc(body)
	r.header = p.headers(req.Params.Headers)
	return r, nil
}

// dropReasoning is the degraded replay: without its reasoning items a request only loses the earlier reasoning context.
func dropReasoning(msgs []message.Message) []message.Message {
	out := make([]message.Message, len(msgs))
	for i, m := range msgs {
		if m.Role == message.RoleAssistant {
			content := make(message.Content, 0, len(m.Content))
			for _, b := range m.Content {
				if _, ok := b.(*message.ReasoningBlock); !ok {
					content = append(content, b)
				}
			}
			m.Content = content
		}
		out[i] = m
	}
	return out
}

// reasoning is sent only when the request asks for it or the model is known to reason, so a model that may not reason never receives a summary request.
func reasoning(req *provider.Request, notices *[]provider.Notice) map[string]any {
	rp := req.Params.Reasoning
	if rp.Effort == "" && rp.Mode != provider.ReasoningOn && (len(req.Caps.Reasoning.Modes) == 0 || rp.Mode == provider.ReasoningOff) {
		return nil
	}
	out := map[string]any{}
	if e := rp.Effort; e != "" {
		mapped, changed := caps.MapEffort(e, req.Caps.Reasoning.Efforts)
		if changed {
			*notices = append(*notices, caps.Adjusted("effort", e, mapped))
		}
		out["effort"] = mapped
	}
	if !rp.Hide {
		out["summary"] = "auto"
	}
	return out
}

func sampling(req *provider.Request, body map[string]any, notices *[]provider.Notice) {
	t, p := req.Params.Temperature, req.Params.TopP
	if t == nil && p == nil {
		return
	}
	if req.Caps.NoSampling {
		*notices = append(*notices, caps.Adjusted("sampling", "set", "removed"))
		return
	}
	if t != nil {
		body["temperature"] = *t
	}
	if p != nil {
		body["top_p"] = *p
	}
}

func toolChoice(req *provider.Request, notices *[]provider.Notice) any {
	tc := req.ToolChoice
	var out any
	switch tc.Mode {
	case "":
		return nil
	case provider.ToolChoiceNone:
		return "none"
	case provider.ToolChoiceRequired:
		out = "required"
	case provider.ToolChoiceNamed:
		out = map[string]any{"type": "function", "name": tc.Name}
	default:
		return "auto"
	}
	if req.Caps.NoForcedToolChoice {
		*notices = append(*notices, caps.Adjusted("tool_choice", string(tc.Mode), "auto"))
		return "auto"
	}
	return out
}

// renderTools always states strict, because the API turns strict mode on when the field is absent; deferred definitions are marked only when a tool search goes with them.
func renderTools(tools []message.ToolDef, deferred bool) []json.RawMessage {
	out := make([]json.RawMessage, len(tools))
	for i, t := range tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		w := map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": schema, "strict": t.Strict && caps.StrictOK(schema)}
		if deferred && t.Deferred {
			w["defer_loading"] = true
		}
		out[i] = enc(w)
	}
	return out
}

// renderHosted maps the neutral web search and appends raw definitions for this API; search reports a web search in the request.
func renderHosted(req *provider.Request, notices *[]provider.Notice) (out []json.RawMessage, search bool) {
	for _, h := range req.HostedTools {
		switch ws := h.WebSearch; {
		case ws != nil:
			if req.Caps.NoWebSearch {
				*notices = append(*notices, caps.HostedUnsupported(h.Name, "no_web_search"))
				continue
			}
			w := map[string]any{"type": "web_search"}
			filters := map[string]any{}
			if len(ws.AllowedDomains) > 0 {
				filters["allowed_domains"] = ws.AllowedDomains
			}
			if len(ws.BlockedDomains) > 0 {
				filters["blocked_domains"] = ws.BlockedDomains
			}
			if len(filters) > 0 {
				w["filters"] = filters
			}
			if ws.UserLocation != nil {
				w["user_location"] = caps.ApproxLocation(ws.UserLocation)
			}
			if ws.ContextSize != "" {
				w["search_context_size"] = ws.ContextSize
			}
			if ws.MaxUses > 0 {
				*notices = append(*notices, caps.HostedAdjusted(h.Name, "max_uses", ws.MaxUses))
			}
			out, search = append(out, enc(w)), true
		case h.ToolSearch != nil:
			if req.Caps.NoToolSearch {
				*notices = append(*notices, caps.HostedUnsupported(h.Name, "no_tool_search"))
				continue
			}
			out = append(out, json.RawMessage(`{"type":"tool_search"}`))
		case len(h.Raw[string(api)]) > 0:
			out = append(out, h.Raw[string(api)])
		default:
			*notices = append(*notices, caps.HostedUnsupported(h.Name, "no_definition"))
		}
	}
	return out, search
}

func renderItems(msgs []message.Message, marks map[string]bool) []json.RawMessage {
	out := []json.RawMessage{}
	for _, m := range msgs {
		before := len(out)
		switch m.Role {
		case message.RoleAssistant:
			out = append(out, assistantItems(m)...)
		case message.RoleTool:
			for _, b := range m.Content {
				if r, ok := b.(*message.ToolResultBlock); ok {
					out = append(out, enc(map[string]any{"type": "function_call_output", "call_id": r.CallID, "output": output(r.Content)}))
				}
			}
		default:
			if parts := inputParts(m.Content); len(parts) > 0 {
				out = append(out, enc(map[string]any{"type": "message", "role": "user", "content": parts}))
			}
		}
		if marks[m.ID] && m.Role != message.RoleAssistant && len(out) > before {
			out[len(out)-1] = marked(out[len(out)-1])
		}
	}
	return out
}

func inputParts(blocks message.Content) []json.RawMessage {
	var out []json.RawMessage
	for _, b := range blocks {
		switch v := b.(type) {
		case *message.TextBlock:
			if v.Text != "" {
				out = append(out, enc(map[string]any{"type": "input_text", "text": v.Text}))
			}
		case *message.ReminderBlock:
			out = append(out, enc(map[string]any{"type": "input_text", "text": prompt.EncodeReminder(*v)}))
		case *message.ImageBlock:
			u := v.URL
			if len(v.Data) > 0 {
				u = dataURL(v.MediaType, v.Data)
			}
			out = append(out, enc(map[string]any{"type": "input_image", "image_url": u}))
		case *message.DocumentBlock:
			f := map[string]any{"type": "input_file"}
			if len(v.Data) > 0 {
				f["file_data"] = dataURL(v.MediaType, v.Data)
			} else {
				f["file_url"] = v.URL
			}
			if v.Filename != "" {
				f["filename"] = v.Filename
			}
			out = append(out, enc(f))
		}
	}
	return out
}

// output is a plain string when the result is only text, and a list of input parts when it carries media.
func output(blocks message.Content) any {
	var texts []string
	for _, b := range blocks {
		switch v := b.(type) {
		case *message.TextBlock:
			if v.Text != "" {
				texts = append(texts, v.Text)
			}
		case *message.ImageBlock, *message.DocumentBlock:
			return inputParts(blocks)
		}
	}
	return strings.Join(texts, "\n\n")
}

func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func sameAPI(n *message.Native) bool {
	return n != nil && provider.API(n.API) == api && len(n.Raw) > 0
}

// withoutID drops an item's id, because nothing is stored server-side and an id there fails the request.
func withoutID(raw json.RawMessage) map[string]json.RawMessage {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	delete(item, "id")
	return item
}

// assistantItems renders one item per block and drops a trailing run of reasoning, which the API rejects without the item it precedes.
func assistantItems(m message.Message) []json.RawMessage {
	var out []json.RawMessage
	var reasoning []bool
	add := func(item any, isReasoning bool) {
		out = append(out, enc(item))
		reasoning = append(reasoning, isReasoning)
	}
	for _, b := range m.Content {
		switch v := b.(type) {
		case *message.TextBlock:
			if v.Text == "" {
				continue
			}
			if sameAPI(v.Native) {
				if item := withoutID(v.Native.Raw); item != nil {
					add(item, false)
					continue
				}
			}
			add(outputMessage(v.Text), false)
		case *message.RefusalBlock:
			if v.Text != "" {
				add(outputMessage(v.Text), false)
			}
		case *message.ReasoningBlock:
			if sameAPI(v.Native) {
				if item := withoutID(v.Native.Raw); item != nil {
					add(item, true)
				}
			}
		case *message.ToolCallBlock:
			item := map[string]json.RawMessage{}
			if sameAPI(v.Native) {
				if raw := withoutID(v.Native.Raw); raw != nil {
					item = raw
				}
			}
			input := v.Input
			if !isObject(input) {
				input = json.RawMessage(`{}`)
			}
			item["type"], item["call_id"], item["name"] = enc("function_call"), enc(v.ID), enc(v.Name)
			item["arguments"] = enc(string(compact(input)))
			add(item, false)
		case *message.OpaqueBlock:
			if sameAPI(v.Native) {
				if item := withoutID(v.Native.Raw); item != nil {
					add(item, false)
				}
			}
		case *message.HostedCallBlock:
			if sameAPI(v.Native) {
				if item := withoutID(v.Native.Raw); item != nil {
					add(item, false)
				}
			}
		case *message.HostedResultBlock:
			if sameAPI(v.Native) { // a web_search_call's result has none, since the call's item already holds it
				if item := withoutID(v.Native.Raw); item != nil {
					add(item, false)
				}
			}
		}
	}
	for len(out) > 0 && reasoning[len(out)-1] {
		out = out[:len(out)-1]
	}
	return out
}

func outputMessage(text string) map[string]any {
	return map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func isObject(b json.RawMessage) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 1 && b[0] == '{' && json.Valid(b)
}

func compact(b json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return b
	}
	return buf.Bytes()
}
