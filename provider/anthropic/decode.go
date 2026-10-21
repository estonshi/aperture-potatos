package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/estonshi/aperture-potatos/internal/sse"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/internal/httpx"
)

const api = provider.APIAnthropicMessages

// decoder turns SSE events into StreamEvents; it does no I/O, so fixtures drive it byte for byte.
type decoder struct {
	req       *provider.Request
	endpoint  string
	display   string
	requestID string
	headers   map[string]string

	started    bool
	done       bool
	responseID string
	usage      map[string]json.RawMessage
	stopRaw    string
	stopDetail json.RawMessage
	transforms json.RawMessage
	blocks     []*wireBlock
	byIndex    map[int]*wireBlock
	hosted     map[string]string // server call id → tool name, from this response and the request's history, since a result can arrive a message later
}

type wireBlock struct {
	pos       int
	typ       string
	start     map[string]json.RawMessage // content_block as it arrived
	text      strings.Builder
	signature string
	partial   strings.Builder
	citations []json.RawMessage
	kind      message.ReasoningKind
	closed    bool
	out       message.Block
}

func newDecoder(req *provider.Request, endpoint, display string, h http.Header) *decoder {
	return &decoder{
		req: req, endpoint: endpoint, display: display,
		requestID: httpx.RequestID(h), headers: httpx.Capture(h, req.CaptureHeaders),
		usage: map[string]json.RawMessage{}, byIndex: map[int]*wireBlock{}, hosted: historyCalls(req.Messages),
	}
}

// historyCalls is the server tool calls earlier assistant messages made, as hosted calls or as opaque server_tool_use blocks from older sessions.
func historyCalls(msgs []message.Message) map[string]string {
	out := map[string]string{}
	for _, m := range msgs {
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			switch v := b.(type) {
			case *message.HostedCallBlock:
				if sameAPI(v.Native) {
					out[v.ID] = v.Name
				}
			case *message.OpaqueBlock:
				if v.Kind == string(api)+":server_tool_use" && sameAPI(v.Native) {
					var c struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					}
					if json.Unmarshal(v.Native.Raw, &c) == nil && c.ID != "" {
						out[c.ID] = c.Name
					}
				}
			}
		}
	}
	return out
}

type frame struct {
	Type         string                     `json:"type"`
	Index        int                        `json:"index"`
	Message      json.RawMessage            `json:"message"`
	ContentBlock map[string]json.RawMessage `json:"content_block"`
	Delta        map[string]json.RawMessage `json:"delta"`
	Usage        map[string]json.RawMessage `json:"usage"`
	StopDetails  json.RawMessage            `json:"stop_details"`
	Transforms   json.RawMessage            `json:"input_transformations"`
}

func str(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func protocolError(format string, args ...any) *provider.Error {
	return &provider.Error{Class: provider.ClassTransient, API: api, Code: "protocol_error", Message: fmt.Sprintf(format, args...), MidStream: true}
}

func (d *decoder) handle(ev sse.Event) ([]provider.StreamEvent, error) {
	var f frame
	if err := json.Unmarshal(ev.Data, &f); err != nil {
		return nil, protocolError("event %q carries data that is not JSON: %v", ev.Name, err)
	}
	if f.Type == "" {
		f.Type = ev.Name
	}
	var out []provider.StreamEvent
	if d.req.EmitRaw {
		out = append(out, provider.StreamEvent{Kind: provider.SERaw, Text: f.Type, Raw: append(json.RawMessage(nil), ev.Data...)})
	}
	switch f.Type {
	case "message_start":
		var m struct {
			ID         string                     `json:"id"`
			Usage      map[string]json.RawMessage `json:"usage"`
			Transforms json.RawMessage            `json:"input_transformations"`
		}
		if err := json.Unmarshal(f.Message, &m); err != nil {
			return nil, protocolError("message_start: %v", err)
		}
		d.started, d.responseID = true, m.ID
		out = append(out, provider.StreamEvent{Kind: provider.SEStart, ResponseID: m.ID, RequestID: d.requestID, Headers: d.headers})
		d.mergeUsage(m.Usage)
		out = append(out, d.usageEvent())
		out = append(out, d.transformed(m.Transforms)...)
	case "content_block_start":
		ev, err := d.startBlock(f.Index, f.ContentBlock)
		if err != nil {
			return nil, err
		}
		out = append(out, ev...)
	case "content_block_delta":
		ev, err := d.delta(f.Index, f.Delta)
		if err != nil {
			return nil, err
		}
		out = append(out, ev...)
	case "content_block_stop":
		b := d.byIndex[f.Index]
		if b == nil || b.closed {
			return nil, protocolError("content_block_stop for unknown block %d", f.Index)
		}
		out = append(out, provider.StreamEvent{Kind: provider.SEBlockStop, Index: b.pos, BlockType: b.out.BlockType(), Block: d.close(b)})
	case "message_delta":
		if raw := f.Delta["stop_reason"]; len(raw) > 0 && string(raw) != "null" {
			d.stopRaw = str(raw)
		}
		for _, sd := range []json.RawMessage{f.Delta["stop_details"], f.StopDetails} {
			if len(sd) > 0 && string(sd) != "null" {
				d.stopDetail = sd
			}
		}
		if len(f.Usage) > 0 {
			d.mergeUsage(f.Usage)
			out = append(out, d.usageEvent())
		}
		out = append(out, d.transformed(f.Transforms)...)
	case "message_stop":
		if !d.started {
			return nil, protocolError("message_stop before message_start")
		}
		d.done = true
		out = append(out, d.end())
	case "error":
		e := httpx.Classify(api, 0, nil, ev.Data, true)
		if e.RequestID == "" {
			e.RequestID = d.requestID
		}
		return out, e
	}
	return out, nil
}

// transformed keeps input_transformations for the message and reports dropped thinking, which the fallback replay asks for.
func (d *decoder) transformed(raw json.RawMessage) []provider.StreamEvent {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	d.transforms = raw
	var items []struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &items)
	for _, it := range items {
		if it.Type == "thinking_dropped" {
			return []provider.StreamEvent{{Kind: provider.SENotice, Notice: &provider.Notice{Kind: "thinking_dropped", Detail: enc(map[string]json.RawMessage{"input_transformations": raw})}}}
		}
	}
	return nil
}

func (d *decoder) startBlock(index int, cb map[string]json.RawMessage) ([]provider.StreamEvent, error) {
	if _, dup := d.byIndex[index]; dup {
		return nil, protocolError("content_block_start repeats index %d", index)
	}
	b := &wireBlock{pos: len(d.blocks), typ: str(cb["type"]), start: cb}
	d.blocks = append(d.blocks, b)
	d.byIndex[index] = b
	ev := provider.StreamEvent{Kind: provider.SEBlockStart, Index: b.pos}
	var out []provider.StreamEvent
	switch b.typ {
	case "text":
		b.out = &message.TextBlock{}
		b.text.WriteString(str(cb["text"]))
	case "thinking":
		b.kind = message.ReasoningSummary
		switch d.display {
		case "omitted":
			b.kind = message.ReasoningOmitted
		case "updates":
			b.kind = message.ReasoningProgress
		}
		b.out = &message.ReasoningBlock{Kind: b.kind}
		b.text.WriteString(str(cb["thinking"]))
		b.signature = str(cb["signature"])
		ev.ReasoningKind = b.kind
	case "redacted_thinking":
		b.kind = message.ReasoningRedacted
		b.out = &message.ReasoningBlock{Kind: b.kind}
		ev.ReasoningKind = b.kind
	case "tool_use":
		c := &message.ToolCallBlock{ID: str(cb["id"]), Name: str(cb["name"])}
		b.out = c
		ev.ToolCallID, ev.ToolName = c.ID, c.Name
	case "server_tool_use":
		c := &message.HostedCallBlock{ID: str(cb["id"]), Name: str(cb["name"])}
		if c.Name == toolSearchName {
			c.Name = "tool_search" // the neutral tool search's name, as it was declared
		}
		d.hosted[c.ID] = c.Name
		b.out = c
		ev.ToolCallID, ev.ToolName = c.ID, c.Name
	default:
		if name, ok := d.hosted[str(cb["tool_use_id"])]; ok && strings.HasSuffix(b.typ, "_tool_result") {
			b.out = &message.HostedResultBlock{CallID: str(cb["tool_use_id"]), Name: name}
			ev.ToolCallID, ev.ToolName = str(cb["tool_use_id"]), name
			break
		}
		if b.typ == "fallback" {
			out = append(out, provider.StreamEvent{Kind: provider.SENotice, Notice: &provider.Notice{Kind: "fallback", Detail: enc(cb)}})
		}
		b.out = &message.OpaqueBlock{Kind: string(api) + ":" + b.typ}
	}
	ev.BlockType = b.out.BlockType()
	return append(out, ev), nil
}

func (d *decoder) delta(index int, delta map[string]json.RawMessage) ([]provider.StreamEvent, error) {
	b := d.byIndex[index]
	if b == nil || b.closed {
		return nil, protocolError("content_block_delta for unknown block %d", index)
	}
	switch str(delta["type"]) {
	case "text_delta":
		t := str(delta["text"])
		b.text.WriteString(t)
		return []provider.StreamEvent{{Kind: provider.SETextDelta, Index: b.pos, Channel: "text", Text: t}}, nil
	case "thinking_delta":
		t := str(delta["thinking"])
		b.text.WriteString(t)
		return []provider.StreamEvent{{Kind: provider.SEReasoningDelta, Index: b.pos, ReasoningKind: b.kind, Text: t}}, nil
	case "signature_delta":
		b.signature += str(delta["signature"])
	case "citations_delta":
		b.citations = append(b.citations, delta["citation"])
	case "input_json_delta":
		t := str(delta["partial_json"])
		b.partial.WriteString(t)
		if c, ok := b.out.(*message.ToolCallBlock); ok && t != "" {
			return []provider.StreamEvent{{Kind: provider.SEToolArgsDelta, Index: b.pos, ToolCallID: c.ID, Text: t}}, nil
		}
	}
	return nil, nil
}

// close builds the final block; thinking keeps the whole accumulated wire block, because the latest one must go back unmodified.
func (d *decoder) close(b *wireBlock) message.Block {
	b.closed = true
	nat := func(raw map[string]json.RawMessage) *message.Native {
		return &message.Native{API: string(api), Raw: enc(raw)}
	}
	switch v := b.out.(type) {
	case *message.TextBlock:
		v.Text = b.text.String()
		if len(b.citations) > 0 {
			raw := clone(b.start)
			raw["text"] = enc(v.Text)
			raw["citations"] = enc(b.citations)
			v.Native = nat(raw)
			v.Citations = citations(b.citations)
		}
	case *message.ReasoningBlock:
		raw := clone(b.start)
		if b.typ == "thinking" {
			v.Text = b.text.String()
			raw["thinking"] = enc(v.Text)
			raw["signature"] = enc(b.signature)
			if v.Text == "" && v.Kind == message.ReasoningSummary {
				v.Kind = message.ReasoningOmitted // the model's default display omitted the text and left only the signature
			}
		}
		v.Native = nat(raw)
	case *message.ToolCallBlock:
		in := strings.TrimSpace(b.partial.String())
		switch {
		case in == "":
			v.Input = json.RawMessage(`{}`)
		case isObject(json.RawMessage(in)):
			var buf bytes.Buffer
			_ = json.Compact(&buf, []byte(in))
			v.Input = buf.Bytes()
		default:
			v.Input, v.RawInput, v.InputError = json.RawMessage(`{}`), b.partial.String(), message.InputInvalidJSON
			if json.Valid([]byte(in)) {
				v.InputError = message.InputNotObject
			}
		}
	case *message.OpaqueBlock:
		raw := clone(b.start)
		if in := strings.TrimSpace(b.partial.String()); in != "" && json.Valid([]byte(in)) {
			raw["input"] = json.RawMessage(in)
		}
		v.Text = str(raw["text"])
		v.Native = nat(raw)
	case *message.HostedCallBlock:
		raw := clone(b.start)
		if in := strings.TrimSpace(b.partial.String()); in != "" && json.Valid([]byte(in)) {
			raw["input"] = json.RawMessage(in)
		}
		if in := raw["input"]; len(in) > 0 && string(in) != "null" {
			var buf bytes.Buffer
			if json.Compact(&buf, in) == nil {
				v.Input = buf.Bytes()
			}
		}
		v.Native = nat(raw)
	case *message.HostedResultBlock:
		v.Native = nat(b.start)
		switch b.typ {
		case "web_search_tool_result":
			v.Sources, v.Error = searchResults(b.start["content"])
		case "tool_search_tool_result":
			v.Tools, v.Error = foundTools(b.start["content"])
		}
	}
	return b.out
}

// searchResults reads a web_search_tool_result's content: a list of results, or an error object with an error_code.
func searchResults(raw json.RawMessage) ([]message.Source, string) {
	var fail struct {
		ErrorCode string `json:"error_code"`
	}
	if json.Unmarshal(raw, &fail) == nil && fail.ErrorCode != "" {
		return nil, fail.ErrorCode
	}
	var items []struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		PageAge string `json:"page_age"`
	}
	_ = json.Unmarshal(raw, &items)
	var out []message.Source
	for _, it := range items {
		if it.URL != "" {
			out = append(out, message.Source{URL: it.URL, Title: it.Title, Age: it.PageAge})
		}
	}
	return out, ""
}

// citations keeps the citations that name a URL, which is what web search results carry; document citations have none.
func citations(raw []json.RawMessage) []message.Citation {
	var out []message.Citation
	for _, r := range raw {
		var c struct {
			URL       string `json:"url"`
			Title     string `json:"title"`
			CitedText string `json:"cited_text"`
		}
		if json.Unmarshal(r, &c) == nil && c.URL != "" {
			out = append(out, message.Citation{URL: c.URL, Title: c.Title, CitedText: c.CitedText})
		}
	}
	return out
}

func clone(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mergeUsage replaces fields one by one, because message_delta usage is cumulative and may carry only some of them.
func (d *decoder) mergeUsage(u map[string]json.RawMessage) {
	for k, v := range u {
		d.usage[k] = v
	}
}

func (d *decoder) currentUsage() *message.Usage {
	n := func(k string) int64 {
		var v int64
		_ = json.Unmarshal(d.usage[k], &v)
		return v
	}
	var details struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	}
	_ = json.Unmarshal(d.usage["output_tokens_details"], &details)
	return &message.Usage{
		InputTokens:      n("input_tokens") + n("cache_read_input_tokens") + n("cache_creation_input_tokens"),
		CacheReadTokens:  n("cache_read_input_tokens"),
		CacheWriteTokens: n("cache_creation_input_tokens"),
		OutputTokens:     n("output_tokens"),
		ReasoningTokens:  details.ThinkingTokens,
		HostedCalls:      serverToolUse(d.usage["server_tool_use"]),
		Raw:              enc(d.usage),
	}
}

// serverToolUse turns server_tool_use counts such as web_search_requests into calls per tool name.
func serverToolUse(raw json.RawMessage) map[string]int64 {
	var counts map[string]json.RawMessage
	_ = json.Unmarshal(raw, &counts)
	var out map[string]int64
	for k, v := range counts {
		var n int64
		if name, ok := strings.CutSuffix(k, "_requests"); ok && json.Unmarshal(v, &n) == nil && n > 0 {
			if out == nil {
				out = map[string]int64{}
			}
			out[name] = n
		}
	}
	return out
}

func (d *decoder) usageEvent() provider.StreamEvent {
	return provider.StreamEvent{Kind: provider.SEUsage, Usage: d.currentUsage()}
}

func (d *decoder) end() provider.StreamEvent {
	stop := &provider.Stop{Reason: httpx.StopReason(api, d.stopRaw), Raw: d.stopRaw, Details: d.stopDetail}
	var blocks []message.Block
	for _, b := range d.blocks {
		if !b.closed {
			continue
		}
		if c, ok := b.out.(*message.ToolCallBlock); ok && c.InputError != "" && stop.Reason == provider.StopMaxTokens {
			cut := *c                                // a copy, because block_stop already handed the consumer this block
			cut.InputError = message.InputIncomplete // the output cap cut the arguments off, which is not the model's mistake
			blocks = append(blocks, &cut)
			continue
		}
		blocks = append(blocks, b.out)
	}
	usage := d.currentUsage()
	m := &message.Message{
		Role:       message.RoleAssistant,
		Origin:     message.Origin{Kind: message.OriginModel},
		Content:    finalize(blocks),
		Producer:   d.producer(),
		Usage:      usage,
		StopReason: string(stop.Reason),
	}
	if d.transforms != nil {
		m.Native = &message.Native{API: string(api), Raw: enc(map[string]json.RawMessage{"input_transformations": d.transforms})}
	}
	return provider.StreamEvent{Kind: provider.SEEnd, Stop: stop, Message: m, Usage: usage}
}

func (d *decoder) producer() *message.Producer {
	p := &message.Producer{API: string(api), Endpoint: d.endpoint, Model: d.req.Model, ResponseID: d.responseID, RequestID: d.requestID, Headers: d.headers}
	if f := d.req.Caps.Family; f != "" && f != d.req.Model {
		p.Family = f
	}
	return p
}

// finalize drops empty text blocks and, before the last fallback block, everything the abandoned generation produced, so its tool calls never run.
func finalize(blocks []message.Block) message.Content {
	last := -1
	for i, b := range blocks {
		if o, ok := b.(*message.OpaqueBlock); ok && o.Kind == string(api)+":fallback" {
			last = i
		}
	}
	out := make(message.Content, 0, len(blocks))
	for i, b := range blocks {
		if t, ok := b.(*message.TextBlock); ok && t.Text == "" && t.Native == nil {
			continue
		}
		if i < last {
			switch v := b.(type) {
			case *message.ReasoningBlock, *message.ToolCallBlock:
				continue
			case *message.OpaqueBlock:
				if v.Kind == string(api)+":connector_text" {
					continue
				}
			}
		}
		out = append(out, b)
	}
	return out
}
