package openaichat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/estonshi/aperture-potatos/internal/sse"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/internal/httpx"
)

// decoder synthesizes block boundaries the Chat protocol lacks; it does no I/O, so fixtures drive it byte for byte.
type decoder struct {
	req           *provider.Request
	endpoint      string
	requestID     string
	headers       map[string]string
	inputEstimate int64

	started    bool
	done       bool
	responseID string
	finish     string
	usage      json.RawMessage
	blocks     []*slot
	reasoning  *slot
	text       *slot
	refusal    *slot
	tools      []*slot         // tool calls in the order they opened
	byIndex    map[int]*slot   // the call each wire index currently feeds; -1 stands for a missing index
	usedIDs    map[string]bool // call ids in the request and this message, so a reused id gets a fresh one

	reasoningDone  bool   // one reasoning block per message: text arriving after it closed is not shown
	reasoningField string // the first string field reasoning came from, which replay writes back
	sawString      bool   // once a string field carried reasoning, array fields no longer supply text
	thinking       []map[string]any
	details        []json.RawMessage
	emitted        int64 // estimate of the output, for a stream that never reports usage
}

type slot struct {
	pos    int
	out    message.Block
	text   strings.Builder
	args   strings.Builder
	wireID string // the call id the server sent, which tells a new call on a reused index apart
	closed bool
}

func newDecoder(req *provider.Request, endpoint string, h http.Header, inputEstimate int64) *decoder {
	return &decoder{req: req, endpoint: endpoint, requestID: httpx.RequestID(h), headers: httpx.Capture(h, req.CaptureHeaders), inputEstimate: inputEstimate, byIndex: map[int]*slot{}}
}

type chunk struct {
	ID      string          `json:"id"`
	Choices []choice        `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

type choice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Content          json.RawMessage   `json:"content"`
	Refusal          json.RawMessage   `json:"refusal"`
	ReasoningContent json.RawMessage   `json:"reasoning_content"`
	Reasoning        json.RawMessage   `json:"reasoning"`
	ThinkingBlocks   []map[string]any  `json:"thinking_blocks"`
	ReasoningDetails []json.RawMessage `json:"reasoning_details"`
	ToolCalls        []struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// jsonString reads a string field and ignores every other JSON type, since vLLM and OpenRouter disagree on what reasoning holds.
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func protocolError(format string, args ...any) *provider.Error {
	return &provider.Error{Class: provider.ClassTransient, API: api, Code: "protocol_error", Message: fmt.Sprintf(format, args...), MidStream: true}
}

// transientFinishes are finish reasons that report a server failure rather than an end of output.
var transientFinishes = []string{"insufficient_system_resource", "aborted", "error"}

func (d *decoder) handle(ev sse.Event) ([]provider.StreamEvent, error) {
	if ev.Done() {
		if !d.started {
			return nil, protocolError("the stream ended before any chunk")
		}
		d.done = true
		return d.end(), nil
	}
	var c chunk
	if err := json.Unmarshal(ev.Data, &c); err != nil {
		return nil, protocolError("a chunk is not JSON: %v", err)
	}
	var out []provider.StreamEvent
	if d.req.EmitRaw {
		out = append(out, provider.StreamEvent{Kind: provider.SERaw, Raw: append(json.RawMessage(nil), ev.Data...)})
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		e := httpx.Classify(api, 0, nil, ev.Data, true)
		if e.RequestID == "" {
			e.RequestID = d.requestID
		}
		return out, e
	}
	if !d.started {
		d.started, d.responseID = true, c.ID
		out = append(out, provider.StreamEvent{Kind: provider.SEStart, ResponseID: c.ID, RequestID: d.requestID, Headers: d.headers})
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue // one completion is requested; other choices are not part of it
		}
		out = append(out, d.delta(ch.Delta)...)
		if f := ch.FinishReason; f != nil && *f != "" && d.finish == "" {
			d.finish = *f // OpenRouter repeats it in the usage chunk; the first one counts
			if slices.Contains(transientFinishes, *f) {
				return out, &provider.Error{Class: provider.ClassTransient, API: api, Code: *f, Message: "the server stopped the response: " + *f, MidStream: true, RequestID: d.requestID}
			}
		}
	}
	if len(c.Usage) > 0 && string(c.Usage) != "null" {
		d.usage = c.Usage
		out = append(out, provider.StreamEvent{Kind: provider.SEUsage, Usage: d.currentUsage()})
	}
	return out, nil
}

func (d *decoder) delta(dl delta) []provider.StreamEvent {
	var out []provider.StreamEvent
	if t := d.reasoningText(dl); t != "" {
		out = append(out, d.reasoningDelta(t)...)
	}
	if t := jsonString(dl.Content); t != "" {
		out = append(out, d.closeReasoning()...)
		if d.text == nil || d.text.closed {
			d.text = &slot{out: &message.TextBlock{}}
			out = append(out, d.open(d.text))
		}
		d.text.text.WriteString(t)
		d.emitted += estimate(t)
		out = append(out, provider.StreamEvent{Kind: provider.SETextDelta, Index: d.text.pos, Channel: "text", Text: t})
	}
	if t := jsonString(dl.Refusal); t != "" {
		out = append(out, d.closeReasoning()...)
		if d.refusal == nil {
			d.refusal = &slot{out: &message.RefusalBlock{}}
			out = append(out, d.open(d.refusal))
		}
		d.refusal.text.WriteString(t)
		d.emitted += estimate(t)
		out = append(out, provider.StreamEvent{Kind: provider.SETextDelta, Index: d.refusal.pos, Channel: "refusal", Text: t})
	}
	for _, tc := range dl.ToolCalls {
		out = append(out, d.closeReasoning()...)
		if d.text != nil && !d.text.closed {
			out = append(out, d.close(d.text))
		}
		key := -1
		if tc.Index != nil {
			key = *tc.Index
		}
		s := d.byIndex[key]
		if s != nil && tc.ID != "" && s.wireID != "" && tc.ID != s.wireID {
			s = nil // a different id on the same or a missing index is another call, as some servers send every call at index 0
		}
		if s == nil {
			id := d.callID(tc.ID)
			s = &slot{out: &message.ToolCallBlock{ID: id, Name: tc.Function.Name}, wireID: tc.ID}
			d.byIndex[key] = s
			d.tools = append(d.tools, s)
			ev := d.open(s)
			ev.ToolCallID, ev.ToolName = id, tc.Function.Name
			out = append(out, ev)
		} else {
			c := s.out.(*message.ToolCallBlock)
			if s.wireID == "" {
				s.wireID = tc.ID
			}
			if n := tc.Function.Name; n != "" && n != c.Name {
				c.Name += n // split names are appended; a repeated whole name is ignored
			}
		}
		if a := tc.Function.Arguments; a != "" {
			s.args.WriteString(a)
			d.emitted += estimate(a)
			out = append(out, provider.StreamEvent{Kind: provider.SEToolArgsDelta, Index: s.pos, ToolCallID: s.out.(*message.ToolCallBlock).ID, Text: a})
		}
	}
	return out
}

// callID keeps the server's id unless it is empty or already taken in this context, since some compatible servers omit it or number from call_0 in every response, and the kernel reuses the result of an id that already ran.
func (d *decoder) callID(id string) string {
	if d.usedIDs == nil {
		d.usedIDs = map[string]bool{}
		for _, m := range d.req.Messages {
			for _, b := range m.Content {
				if c, ok := b.(*message.ToolCallBlock); ok {
					d.usedIDs[c.ID] = true
				}
			}
		}
	}
	for n := len(d.usedIDs); id == "" || d.usedIDs[id]; n++ {
		id = fmt.Sprintf("call_%d", n)
	}
	d.usedIDs[id] = true
	return id
}

// reasoningText picks this chunk's reasoning text from its highest-priority source and keeps the array fields for replay.
func (d *decoder) reasoningText(dl delta) string {
	d.thinking = mergeThinking(d.thinking, dl.ThinkingBlocks)
	d.details = append(d.details, dl.ReasoningDetails...)
	for _, f := range []struct {
		name string
		raw  json.RawMessage
	}{{"reasoning_content", dl.ReasoningContent}, {"reasoning", dl.Reasoning}} {
		if t := jsonString(f.raw); t != "" {
			d.sawString = true
			if d.reasoningField == "" && !d.reasoningDone {
				d.reasoningField = f.name
			}
			return t
		}
	}
	if d.sawString {
		return ""
	}
	var b strings.Builder
	for _, tb := range dl.ThinkingBlocks {
		if tb["type"] == "thinking" {
			b.WriteString(str(tb["thinking"]))
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	for _, raw := range dl.ReasoningDetails {
		var item map[string]any
		_ = json.Unmarshal(raw, &item)
		if t := str(item["text"]); t != "" {
			b.WriteString(t)
		} else {
			b.WriteString(str(item["summary"]))
		}
	}
	return b.String()
}

func (d *decoder) reasoningDelta(t string) []provider.StreamEvent {
	if d.reasoningDone {
		return nil
	}
	var out []provider.StreamEvent
	if d.reasoning == nil {
		d.reasoning = &slot{out: &message.ReasoningBlock{Kind: message.ReasoningRaw}}
		ev := d.open(d.reasoning)
		ev.ReasoningKind = message.ReasoningRaw
		out = append(out, ev)
	}
	d.reasoning.text.WriteString(t)
	d.emitted += estimate(t)
	return append(out, provider.StreamEvent{Kind: provider.SEReasoningDelta, Index: d.reasoning.pos, ReasoningKind: message.ReasoningRaw, Text: t})
}

func (d *decoder) closeReasoning() []provider.StreamEvent {
	if d.reasoning == nil || d.reasoningDone {
		return nil
	}
	d.reasoningDone = true
	return []provider.StreamEvent{d.close(d.reasoning)}
}

func (d *decoder) open(s *slot) provider.StreamEvent {
	s.pos = len(d.blocks)
	d.blocks = append(d.blocks, s)
	return provider.StreamEvent{Kind: provider.SEBlockStart, Index: s.pos, BlockType: s.out.BlockType()}
}

func (d *decoder) close(s *slot) provider.StreamEvent {
	s.closed = true
	switch v := s.out.(type) {
	case *message.TextBlock:
		v.Text = s.text.String()
	case *message.RefusalBlock:
		v.Text = s.text.String()
	case *message.ReasoningBlock:
		v.Text = s.text.String()
		if d.reasoningField != "" {
			v.Native = &message.Native{API: string(api), Raw: enc(map[string]string{"field": d.reasoningField})}
		}
	case *message.ToolCallBlock:
		in := strings.TrimSpace(s.args.String())
		switch {
		case in == "":
			v.Input = json.RawMessage(`{}`)
		case isObject(json.RawMessage(in)):
			v.Input = compact(json.RawMessage(in))
		default:
			v.Input, v.RawInput, v.InputError = json.RawMessage(`{}`), s.args.String(), message.InputInvalidJSON
			if json.Valid([]byte(in)) {
				v.InputError = message.InputNotObject
			}
		}
	}
	return provider.StreamEvent{Kind: provider.SEBlockStop, Index: s.pos, BlockType: s.out.BlockType(), Block: s.out}
}

// end closes every open block, tool calls last in their index order, and assembles the message.
func (d *decoder) end() []provider.StreamEvent {
	var out []provider.StreamEvent
	out = append(out, d.closeReasoning()...)
	for _, s := range []*slot{d.text, d.refusal} {
		if s != nil && !s.closed {
			out = append(out, d.close(s))
		}
	}
	for _, s := range d.tools {
		out = append(out, d.close(s))
	}

	var blocks message.Content
	visible, calls := false, 0
	for _, s := range d.blocks {
		switch v := s.out.(type) {
		case *message.TextBlock:
			if v.Text == "" {
				continue
			}
			visible = true
		case *message.RefusalBlock:
			visible = visible || v.Text != ""
		case *message.ToolCallBlock:
			calls++
		}
		blocks = append(blocks, s.out)
	}
	stop := &provider.Stop{Reason: httpx.StopReason(api, d.finish), Raw: d.finish}
	switch {
	case d.finish == "length":
		stop.Reason = provider.StopMaxTokens // length wins over accumulated calls, whose arguments may be cut off
		stop.ReasoningExhausted = !visible && calls == 0
		for i, b := range blocks {
			if c, ok := b.(*message.ToolCallBlock); ok && c.InputError != "" {
				cut := *c // a copy, because block_stop already handed the consumer this block
				cut.InputError = message.InputIncomplete
				blocks[i] = &cut
			}
		}
	case calls > 0 && (d.finish == "stop" || d.finish == ""):
		stop.Reason = provider.StopToolUse // Ollama and forced tool choice report stop after a call
	}
	usage := d.currentUsage()
	m := &message.Message{
		Role:       message.RoleAssistant,
		Origin:     message.Origin{Kind: message.OriginModel},
		Content:    blocks,
		Producer:   d.producer(),
		Usage:      usage,
		StopReason: string(stop.Reason),
	}
	if len(d.thinking) > 0 || len(d.details) > 0 {
		raw := map[string]any{}
		if len(d.thinking) > 0 {
			raw["thinking_blocks"] = d.thinking
		}
		if len(d.details) > 0 {
			raw["reasoning_details"] = d.details
		}
		m.Native = &message.Native{API: string(api), Raw: enc(raw)}
	}
	return append(out, provider.StreamEvent{Kind: provider.SEEnd, Stop: stop, Message: m, Usage: usage})
}

func (d *decoder) currentUsage() *message.Usage {
	if d.usage == nil {
		return &message.Usage{InputTokens: d.inputEstimate, OutputTokens: d.emitted, Estimated: true}
	}
	var u struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens     int64 `json:"cached_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	_ = json.Unmarshal(d.usage, &u)
	var raw bytes.Buffer
	_ = json.Compact(&raw, d.usage)
	return &message.Usage{
		InputTokens:      u.PromptTokens,
		CacheReadTokens:  u.PromptDetails.CachedTokens,
		CacheWriteTokens: u.PromptDetails.CacheWriteTokens,
		OutputTokens:     u.CompletionTokens,
		ReasoningTokens:  u.CompletionDetails.ReasoningTokens,
		Raw:              raw.Bytes(),
	}
}

func (d *decoder) producer() *message.Producer {
	p := &message.Producer{API: string(api), Endpoint: d.endpoint, Model: d.req.Model, ResponseID: d.responseID, RequestID: d.requestID, Headers: d.headers}
	if f := d.req.Caps.Family; f != "" && f != d.req.Model {
		p.Family = f
	}
	return p
}

// mergeThinking folds streamed thinking_blocks fragments into acc and never extends a signed block, because fusing two signatures corrupts both.
func mergeThinking(acc, frags []map[string]any) []map[string]any {
	for _, b := range frags {
		n := len(acc)
		if n > 0 && b["type"] == "thinking" && acc[n-1]["type"] == "thinking" && str(acc[n-1]["signature"]) == "" {
			acc[n-1]["thinking"] = str(acc[n-1]["thinking"]) + str(b["thinking"])
			if sig := str(b["signature"]); sig != "" {
				acc[n-1]["signature"] = sig
			}
			continue
		}
		acc = append(acc, b) // the first fragment, a redacted_thinking item, or a fragment after a signed block
	}
	return acc
}
