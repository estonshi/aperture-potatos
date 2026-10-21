package openaichat

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

// validID is OpenAI's own limit on tool call ids, the tightest among Chat servers.
var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`).MatchString

type rendered struct {
	body          []byte
	header        http.Header
	notices       []provider.Notice
	inputEstimate int64 // input tokens reported when the stream carries no usage
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
		msgs = dropReplayedReasoning(msgs)
	}
	msgs, healed := project.HealDangling(msgs, prompt.Text("dangling_tool_call"))
	if len(healed) > 0 {
		r.notices = append(r.notices, provider.Notice{Kind: "dangling_tool_call_healed", Detail: enc(map[string]any{"call_ids": healed})})
	}
	msgs = project.RemapIDs(msgs, validID)
	if err := project.CheckTail(msgs, req.PauseTail); err != nil {
		return nil, localError(provider.ClassInvalidRequest, err.Error())
	}

	official := p.endpoint == officialHost
	streamOpts := map[string]any{"include_usage": true}
	if official {
		streamOpts["include_obfuscation"] = false
	}
	body := map[string]any{
		"model":          req.Model,
		"stream":         true,
		"stream_options": streamOpts,
		"messages":       renderMessages(req, msgs),
	}
	maxTokens, adjusted := caps.MaxOutput(req.Params, req.Caps)
	if adjusted {
		r.notices = append(r.notices, caps.Adjusted("max_output_tokens", req.Params.MaxOutputTokens, maxTokens))
	}
	field := req.Caps.MaxTokensField
	if field == "" {
		field = "max_tokens"
	}
	body[field] = maxTokens
	for _, h := range req.HostedTools {
		r.notices = append(r.notices, caps.HostedUnsupported(h.Name, "api_unsupported")) // the protocol has no tools the server runs
	}
	if len(req.Tools) > 0 {
		body["tools"] = renderTools(req.Tools)
		r.notices = append(r.notices, caps.StrictDropped(req.Tools)...)
		if tc := toolChoice(req, &r.notices); tc != nil {
			body["tool_choice"] = tc
		}
		if req.ToolChoice.DisableParallel {
			body["parallel_tool_calls"] = false
		}
	}
	if e := effort(req, &r.notices); e != "" {
		body["reasoning_effort"] = e
	}
	sampling(req, body, &r.notices)
	if len(req.Params.StopSequences) > 0 {
		body["stop"] = req.Params.StopSequences
	}
	if official && req.CacheKey != "" {
		body["prompt_cache_key"] = req.CacheKey
	}
	for k, v := range req.Params.Extra {
		body[k] = v
	}
	r.body = enc(body)
	r.header = p.headers(req.Params.Headers)
	r.inputEstimate = estimate(string(r.body))
	return r, nil
}

// dropReplayedReasoning is the degraded replay: every reasoning field goes, since most servers treat them as optional input.
func dropReplayedReasoning(msgs []message.Message) []message.Message {
	out := project.StripReasoning(msgs, false)
	for i := range out {
		if out[i].Role == message.RoleAssistant {
			out[i].Native = nil
		}
	}
	return out
}

func effort(req *provider.Request, notices *[]provider.Notice) string {
	e := req.Params.Reasoning.Effort
	if e != "" {
		mapped, changed := caps.MapEffort(e, req.Caps.Reasoning.Efforts)
		if changed {
			*notices = append(*notices, caps.Adjusted("effort", e, mapped))
		}
		e = mapped
	}
	if req.Caps.NoToolsWithReasoning && len(req.Tools) > 0 && e != "none" {
		from := e
		if from == "" {
			from = "default"
		}
		*notices = append(*notices, caps.Adjusted("effort", from, "none"))
		e = "none"
	}
	return e
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
		out = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	default:
		return "auto"
	}
	if req.Caps.NoForcedToolChoice {
		*notices = append(*notices, caps.Adjusted("tool_choice", string(tc.Mode), "auto"))
		return "auto"
	}
	return out
}

func renderTools(tools []message.ToolDef) []json.RawMessage {
	out := make([]json.RawMessage, len(tools))
	for i, t := range tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		fn := map[string]any{"name": t.Name, "description": t.Description, "parameters": schema}
		if t.Strict && caps.StrictOK(schema) {
			fn["strict"] = true
		}
		out[i] = enc(map[string]any{"type": "function", "function": fn})
	}
	return out
}

func renderMessages(req *provider.Request, msgs []message.Message) []json.RawMessage {
	var out []json.RawMessage
	var sys []string
	for _, s := range req.System {
		if s.Text != "" {
			sys = append(sys, s.Text)
		}
	}
	plan := planCache(req, msgs)
	if len(sys) > 0 {
		role := req.Caps.SystemRole
		if role == "" {
			role = "system"
		}
		var content any = strings.Join(sys, "\n\n")
		if plan != nil {
			parts := make([]map[string]any, len(sys))
			for i, t := range sys {
				parts[i] = map[string]any{"type": "text", "text": t}
				if plan.system[i] {
					parts[i]["cache_control"] = map[string]any{"type": "ephemeral"}
				}
			}
			content = parts
		}
		out = append(out, enc(map[string]any{"role": role, "content": content}))
	}
	for _, m := range msgs {
		n := len(out)
		switch m.Role {
		case message.RoleAssistant:
			if w := renderAssistant(m); w != nil {
				out = append(out, w)
			}
		case message.RoleTool:
			out = append(out, renderResults(m)...)
		default:
			if parts := userParts(m.Content); len(parts) > 0 {
				out = append(out, enc(map[string]any{"role": "user", "content": content(parts)}))
			}
		}
		if plan != nil && plan.msgs[m.ID] && len(out) > n {
			out[len(out)-1] = marked(out[len(out)-1])
		}
	}
	return out
}

// part is one user content part; text parts collapse to a plain string when nothing else is present, which every server accepts.
type part struct {
	text string
	raw  json.RawMessage
}

func content(parts []part) any {
	var texts []string
	for _, p := range parts {
		if p.raw != nil {
			out := make([]json.RawMessage, len(parts))
			for i, q := range parts {
				out[i] = q.raw
				if q.raw == nil {
					out[i] = enc(map[string]any{"type": "text", "text": q.text})
				}
			}
			return out
		}
		texts = append(texts, p.text)
	}
	return strings.Join(texts, "\n\n")
}

func userParts(blocks message.Content) []part {
	var out []part
	for _, b := range blocks {
		switch v := b.(type) {
		case *message.TextBlock:
			if v.Text != "" {
				out = append(out, part{text: v.Text})
			}
		case *message.ReminderBlock:
			out = append(out, part{text: prompt.EncodeReminder(*v)})
		case *message.ImageBlock, *message.DocumentBlock:
			out = append(out, media(b))
		}
	}
	return out
}

func media(b message.Block) part {
	switch v := b.(type) {
	case *message.ImageBlock:
		u := v.URL
		if len(v.Data) > 0 {
			u = dataURL(v.MediaType, v.Data)
		}
		return part{raw: enc(map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})}
	case *message.DocumentBlock:
		if len(v.Data) == 0 {
			return part{text: prompt.Textf("document_link", map[string]string{"url": v.URL})} // Chat has no URL form for files
		}
		f := map[string]any{"file_data": dataURL(v.MediaType, v.Data)}
		if v.Filename != "" {
			f["filename"] = v.Filename
		}
		return part{raw: enc(map[string]any{"type": "file", "file": f})}
	}
	return part{}
}

func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func sameAPI(n *message.Native) bool {
	return n != nil && provider.API(n.API) == api && len(n.Raw) > 0
}

// renderAssistant builds the content from canonical blocks and, for this endpoint's own messages, puts back the reasoning fields it received.
func renderAssistant(m message.Message) json.RawMessage {
	var texts []string
	var calls []json.RawMessage
	w := map[string]any{"role": "assistant"}
	for _, b := range m.Content {
		switch v := b.(type) {
		case *message.TextBlock:
			if v.Text != "" {
				texts = append(texts, v.Text)
			}
		case *message.RefusalBlock:
			if v.Text != "" {
				texts = append(texts, v.Text)
			}
		case *message.ToolCallBlock:
			input := v.Input
			if !isObject(input) {
				input = json.RawMessage(`{}`)
			}
			calls = append(calls, enc(map[string]any{"id": v.ID, "type": "function", "function": map[string]any{"name": v.Name, "arguments": string(compact(input))}}))
		case *message.ReasoningBlock:
			if !sameAPI(v.Native) || v.Text == "" {
				continue
			}
			var src struct {
				Field string `json:"field"`
			}
			if json.Unmarshal(v.Native.Raw, &src) == nil && (src.Field == "reasoning_content" || src.Field == "reasoning") {
				w[src.Field] = v.Text
			}
		}
	}
	if len(texts) == 0 && len(calls) == 0 {
		return nil
	}
	w["content"] = nil // null alongside tool calls, as the API asks
	if len(texts) > 0 {
		w["content"] = strings.Join(texts, "\n\n")
	}
	if len(calls) > 0 {
		w["tool_calls"] = calls
	}
	if sameAPI(m.Native) {
		var extra struct {
			ThinkingBlocks   []map[string]any  `json:"thinking_blocks"`
			ReasoningDetails []json.RawMessage `json:"reasoning_details"`
		}
		_ = json.Unmarshal(m.Native.Raw, &extra)
		if tb := replayableThinking(extra.ThinkingBlocks); len(tb) > 0 {
			w["thinking_blocks"] = tb
		}
		if len(extra.ReasoningDetails) > 0 {
			w["reasoning_details"] = extra.ReasoningDetails
		}
	}
	return enc(w)
}

// renderResults writes one tool message per result; media cannot sit in a tool message, so it moves to one user message after them.
func renderResults(m message.Message) []json.RawMessage {
	var out []json.RawMessage
	var moved []part
	for _, b := range m.Content {
		r, ok := b.(*message.ToolResultBlock)
		if !ok {
			continue
		}
		var texts []string
		var media []part
		for _, c := range r.Content {
			switch v := c.(type) {
			case *message.TextBlock:
				if v.Text != "" {
					texts = append(texts, v.Text)
				}
			case *message.ImageBlock, *message.DocumentBlock:
				media = append(media, userParts(message.Content{c})...)
			}
		}
		out = append(out, enc(map[string]any{"role": "tool", "tool_call_id": r.CallID, "content": strings.Join(texts, "\n\n")}))
		if len(media) > 0 {
			moved = append(moved, part{text: prompt.Textf("tool_result_media", map[string]string{"call_id": r.CallID})})
			moved = append(moved, media...)
		}
	}
	if len(moved) > 0 {
		out = append(out, enc(map[string]any{"role": "user", "content": content(moved)}))
	}
	return out
}

// replayableThinking keeps signed and redacted items only, because unsigned ones come from aborted streams and upstream rejects them.
func replayableThinking(blocks []map[string]any) []map[string]any {
	var out []map[string]any
	for _, b := range blocks {
		if b["type"] == "redacted_thinking" || str(b["signature"]) != "" {
			out = append(out, b)
		}
	}
	return out
}

func str(v any) string { s, _ := v.(string); return s }

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

// estimate is the token heuristic for a missing usage report: a token per four ASCII bytes, plus one per other character.
func estimate(s string) int64 {
	var ascii, other int64
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}
