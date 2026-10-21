package openairesponses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/internal/sse"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/internal/httpx"
)

// decoder turns Responses stream events into StreamEvents; it does no I/O, so fixtures drive it byte for byte.
type decoder struct {
	req       *provider.Request
	endpoint  string
	requestID string
	headers   map[string]string

	started    bool
	done       bool
	responseID string
	lastSeq    int64
	haveSeq    bool
	usage      json.RawMessage
	blocks     []*slot
	byID       map[string]*slot
	byOutput   map[int]*slot
	searches   int64  // web_search_call items whose action is a search, which is what the API bills
	lastSearch string // id of the latest tool_search_call, which the tool_search_output after it answers
}

type slot struct {
	pos     int
	out     message.Block
	text    strings.Builder
	args    strings.Builder
	raw     bool // reasoning_text arrived, as gpt-oss and vLLM send, so the text is raw chain of thought
	parts   int  // summary parts seen, which are joined by a blank line
	refusal *slot
	opened  bool
	closed  bool
}

func newDecoder(req *provider.Request, endpoint string, h http.Header) *decoder {
	return &decoder{req: req, endpoint: endpoint, requestID: httpx.RequestID(h), headers: httpx.Capture(h, req.CaptureHeaders), byID: map[string]*slot{}, byOutput: map[int]*slot{}}
}

type frame struct {
	Type        string          `json:"type"`
	Seq         *int64          `json:"sequence_number"`
	Response    json.RawMessage `json:"response"`
	Item        json.RawMessage `json:"item"`
	ItemID      string          `json:"item_id"`
	OutputIndex *int            `json:"output_index"`
	Delta       string          `json:"delta"`
	Code        json.RawMessage `json:"code"`
	Message     string          `json:"message"`
	Error       json.RawMessage `json:"error"`
}

type itemHead struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Status    string          `json:"status"`
	Arguments *string         `json:"arguments"`
	Content   []itemPart      `json:"content"`
	Summary   []itemPart      `json:"summary"`
	Encrypted string          `json:"encrypted_content"`
	Text      json.RawMessage `json:"text"`
	Action    json.RawMessage `json:"action"`
}

type itemPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []struct {
		Type  string `json:"type"`
		URL   string `json:"url"`
		Title string `json:"title"`
		Start int    `json:"start_index"`
		End   int    `json:"end_index"`
	} `json:"annotations"`
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
	if f.Seq != nil {
		if d.haveSeq && *f.Seq <= d.lastSeq {
			out = append(out, provider.StreamEvent{Kind: provider.SENotice, Notice: &provider.Notice{Kind: "sequence_regression", Detail: enc(map[string]int64{"from": d.lastSeq, "to": *f.Seq})}})
		}
		d.haveSeq, d.lastSeq = true, max(d.lastSeq, *f.Seq)
	}
	switch f.Type {
	case "response.created", "response.in_progress", "response.queued":
		if !d.started {
			var r struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(f.Response, &r)
			d.started, d.responseID = true, r.ID
			out = append(out, provider.StreamEvent{Kind: provider.SEStart, ResponseID: r.ID, RequestID: d.requestID, Headers: d.headers})
		}
	case "response.output_item.added":
		var h itemHead
		_ = json.Unmarshal(f.Item, &h)
		if s := d.slot(h, f.OutputIndex); !isOpaque(s) {
			out = append(out, d.open(s)...) // an item type this binding does not know surfaces only once it is done
		}
	case "response.output_text.delta":
		if s := d.live(f.ItemID); s != nil && f.Delta != "" {
			s.text.WriteString(f.Delta)
			out = append(out, provider.StreamEvent{Kind: provider.SETextDelta, Index: s.pos, Channel: "text", Text: f.Delta})
		}
	case "response.refusal.delta":
		if s := d.live(f.ItemID); s != nil && f.Delta != "" {
			if s.refusal == nil {
				s.refusal = &slot{out: &message.RefusalBlock{}}
				out = append(out, d.open(s.refusal)...)
			}
			s.refusal.text.WriteString(f.Delta)
			out = append(out, provider.StreamEvent{Kind: provider.SETextDelta, Index: s.refusal.pos, Channel: "refusal", Text: f.Delta})
		}
	case "response.reasoning_summary_part.added":
		if s := d.live(f.ItemID); s != nil {
			s.parts++
			if s.parts > 1 && s.text.Len() > 0 {
				s.text.WriteString("\n\n")
				out = append(out, provider.StreamEvent{Kind: provider.SEReasoningDelta, Index: s.pos, ReasoningKind: message.ReasoningSummary, Text: "\n\n"})
			}
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if s := d.live(f.ItemID); s != nil && f.Delta != "" {
			kind := message.ReasoningSummary
			if f.Type == "response.reasoning_text.delta" {
				kind, s.raw = message.ReasoningRaw, true
			}
			s.text.WriteString(f.Delta)
			out = append(out, provider.StreamEvent{Kind: provider.SEReasoningDelta, Index: s.pos, ReasoningKind: kind, Text: f.Delta})
		}
	case "response.function_call_arguments.delta":
		if s := d.live(f.ItemID); s != nil && f.Delta != "" {
			if c, ok := s.out.(*message.ToolCallBlock); ok {
				s.args.WriteString(f.Delta)
				out = append(out, provider.StreamEvent{Kind: provider.SEToolArgsDelta, Index: s.pos, ToolCallID: c.ID, Text: f.Delta})
			}
		}
	case "response.output_item.done":
		var h itemHead
		_ = json.Unmarshal(f.Item, &h)
		s := d.slot(h, f.OutputIndex)
		out = append(out, d.open(s)...)
		if !s.closed {
			out = append(out, d.close(s, f.Item, h)...)
		}
	case "response.completed", "response.incomplete":
		if !d.started {
			return out, protocolError("%s before response.created", f.Type)
		}
		d.done = true
		var r struct {
			Usage      json.RawMessage `json:"usage"`
			Incomplete json.RawMessage `json:"incomplete_details"`
		}
		_ = json.Unmarshal(f.Response, &r)
		if len(r.Usage) > 0 && string(r.Usage) != "null" {
			d.usage = r.Usage
			out = append(out, provider.StreamEvent{Kind: provider.SEUsage, Usage: d.currentUsage()})
		}
		out = append(out, d.end(f.Type == "response.incomplete", r.Incomplete))
	case "response.failed":
		var r struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(f.Response, &r)
		return out, d.midStream(r.Error)
	case "error":
		if len(f.Error) > 0 && string(f.Error) != "null" {
			return out, d.midStream(f.Error)
		}
		return out, d.midStream(enc(map[string]any{"code": f.Code, "message": f.Message}))
	}
	return out, nil
}

// midStream classifies an error object, flat or nested, by the shared rules, which read it under an error key.
func (d *decoder) midStream(obj json.RawMessage) *provider.Error {
	if len(obj) == 0 || string(obj) == "null" {
		obj = json.RawMessage(`{}`)
	}
	e := httpx.Classify(api, 0, nil, enc(map[string]json.RawMessage{"error": obj}), true)
	if e.RequestID == "" {
		e.RequestID = d.requestID
	}
	return e
}

func isOpaque(s *slot) bool {
	_, ok := s.out.(*message.OpaqueBlock)
	return ok
}

// live is the open slot an item's deltas belong to.
func (d *decoder) live(itemID string) *slot {
	if s := d.byID[itemID]; s != nil && s.opened && !s.closed {
		return s
	}
	return nil
}

// slot finds an item by id, then by output index, and creates it on first sight.
func (d *decoder) slot(h itemHead, outputIndex *int) *slot {
	if s := d.byID[h.ID]; s != nil && h.ID != "" {
		return s
	}
	if outputIndex != nil {
		if s := d.byOutput[*outputIndex]; s != nil {
			return s
		}
	}
	s := &slot{}
	switch h.Type {
	case "message":
		s.out = &message.TextBlock{}
	case "reasoning":
		s.out = &message.ReasoningBlock{Kind: message.ReasoningSummary}
	case "function_call":
		s.out = &message.ToolCallBlock{ID: h.CallID, Name: h.Name} // tools run by call_id, never by the fc_ item id
	case "web_search_call":
		s.out = &message.HostedCallBlock{ID: h.ID, Name: "web_search"}
	case "tool_search_call":
		s.out = &message.HostedCallBlock{ID: h.ID, Name: "tool_search"}
		d.lastSearch = h.ID
	case "tool_search_output":
		s.out = &message.HostedResultBlock{CallID: d.lastSearch, Name: "tool_search"}
	default:
		s.out = &message.OpaqueBlock{Kind: string(api) + ":" + h.Type}
	}
	if h.ID != "" {
		d.byID[h.ID] = s
	}
	if outputIndex != nil {
		d.byOutput[*outputIndex] = s
	}
	return s
}

func (d *decoder) open(s *slot) []provider.StreamEvent {
	if s.opened {
		return nil
	}
	s.opened = true
	s.pos = len(d.blocks)
	d.blocks = append(d.blocks, s)
	ev := provider.StreamEvent{Kind: provider.SEBlockStart, Index: s.pos, BlockType: s.out.BlockType()}
	switch v := s.out.(type) {
	case *message.ReasoningBlock:
		ev.ReasoningKind = v.Kind
	case *message.ToolCallBlock:
		ev.ToolCallID, ev.ToolName = v.ID, v.Name
	case *message.HostedCallBlock:
		ev.ToolCallID, ev.ToolName = v.ID, v.Name
	case *message.HostedResultBlock:
		ev.ToolCallID, ev.ToolName = v.CallID, v.Name
	}
	return []provider.StreamEvent{ev}
}

// close builds the final block from the done item, which is authoritative: encrypted reasoning and final arguments arrive only there.
func (d *decoder) close(s *slot, raw json.RawMessage, h itemHead) []provider.StreamEvent {
	var out []provider.StreamEvent
	s.closed = true
	nat := &message.Native{API: string(api), Raw: compact(raw)}
	switch v := s.out.(type) {
	case *message.TextBlock:
		v.Text = s.text.String()
		if parts := partsText(h.Content, "output_text", ""); parts != "" {
			v.Text = parts
		}
		v.Citations = urlCitations(h.Content)
		v.Native = nat
		if r := s.refusal; r != nil && !r.closed {
			r.closed = true
			r.out.(*message.RefusalBlock).Text = r.text.String()
			out = append(out, provider.StreamEvent{Kind: provider.SEBlockStop, Index: r.pos, BlockType: message.TypeRefusal, Block: r.out})
		}
	case *message.ReasoningBlock:
		v.Text = s.text.String()
		if v.Text == "" {
			v.Text = partsText(h.Summary, "summary_text", "\n\n")
		}
		if v.Text == "" {
			if t := partsText(h.Content, "reasoning_text", ""); t != "" {
				v.Text, s.raw = t, true
			}
		}
		switch {
		case s.raw:
			v.Kind = message.ReasoningRaw
		case v.Text == "" && h.Encrypted != "":
			v.Kind = message.ReasoningEncrypted
		}
		v.Native = nat
	case *message.ToolCallBlock:
		if v.ID == "" {
			v.ID = h.CallID
		}
		if v.Name == "" {
			v.Name = h.Name
		}
		args := s.args.String()
		if h.Arguments != nil {
			args = *h.Arguments // the done item overrides the streamed fragments
		}
		in := strings.TrimSpace(args)
		switch {
		case in == "":
			v.Input = json.RawMessage(`{}`)
		case isObject(json.RawMessage(in)):
			v.Input = compact(json.RawMessage(in))
		default:
			v.Input, v.RawInput, v.InputError = json.RawMessage(`{}`), args, message.InputInvalidJSON
			if json.Valid([]byte(in)) {
				v.InputError = message.InputNotObject
			}
		}
		if h.Status != "" && h.Status != "completed" {
			v.InputError = message.InputIncomplete // an unfinished call is truncated output and is never run
			if v.RawInput == "" && in != "" {
				v.RawInput = args
			}
		}
		v.Native = nat
	case *message.OpaqueBlock:
		v.Text = jsonString(h.Text)
		v.Native = nat
	case *message.HostedCallBlock:
		v.Status, v.Native = h.Status, nat
		if v.ID == "" {
			v.ID = h.ID
		}
		if v.Name == "tool_search" {
			v.Input = searchArguments(raw)
			break
		}
		var action struct {
			Type    string `json:"type"`
			Sources []struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"sources"`
		}
		var input map[string]json.RawMessage
		if json.Unmarshal(h.Action, &input) == nil && input != nil {
			_ = json.Unmarshal(h.Action, &action)
			delete(input, "sources") // they go to the hosted result, so the call's input stays what the model asked for
			v.Input = enc(input)
		}
		if action.Type == "search" {
			d.searches++
		}
		out = append(out, provider.StreamEvent{Kind: provider.SEBlockStop, Index: s.pos, BlockType: s.out.BlockType(), Block: s.out})
		res := &message.HostedResultBlock{CallID: v.ID, Name: v.Name}
		for _, src := range action.Sources {
			if src.URL != "" {
				res.Sources = append(res.Sources, message.Source{URL: src.URL, Title: src.Title})
			}
		}
		rs := &slot{out: res, closed: true}
		out = append(out, d.open(rs)...)
		return append(out, provider.StreamEvent{Kind: provider.SEBlockStop, Index: rs.pos, BlockType: res.BlockType(), Block: res})
	case *message.HostedResultBlock:
		v.Tools, v.Native = loadedTools(raw), nat
	}
	return append(out, provider.StreamEvent{Kind: provider.SEBlockStop, Index: s.pos, BlockType: s.out.BlockType(), Block: s.out})
}

// urlCitations reads url_citation annotations, shifting each part's offsets past the parts before it, since the block joins them into one text.
func urlCitations(parts []itemPart) []message.Citation {
	var out []message.Citation
	offset := 0
	for _, p := range parts {
		if p.Type != "output_text" {
			continue
		}
		for _, a := range p.Annotations {
			if a.Type == "url_citation" && a.URL != "" {
				out = append(out, message.Citation{URL: a.URL, Title: a.Title, Start: offset + a.Start, End: offset + a.End})
			}
		}
		offset += utf8.RuneCountInString(p.Text)
	}
	return out
}

func partsText(parts []itemPart, typ, sep string) string {
	var texts []string
	for _, p := range parts {
		if p.Type == typ && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, sep)
}

func jsonString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func (d *decoder) end(incomplete bool, details json.RawMessage) provider.StreamEvent {
	if len(details) == 0 || string(details) == "null" {
		details = nil
	}
	content := finalize(d.blocks)
	calls := 0
	for _, b := range content {
		if _, ok := b.(*message.ToolCallBlock); ok {
			calls++
		}
	}
	stop := &provider.Stop{Reason: provider.StopEndTurn, Raw: "completed", Details: details}
	if calls > 0 {
		stop.Reason = provider.StopToolUse
	}
	if incomplete {
		var reason struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(details, &reason)
		stop.Raw = reason.Reason
		stop.Reason = httpx.StopReason(api, reason.Reason)
		stop.ReasoningExhausted = d.onlyReasoning()
	}
	usage := d.currentUsage()
	m := &message.Message{
		Role:       message.RoleAssistant,
		Origin:     message.Origin{Kind: message.OriginModel},
		Content:    content,
		Producer:   d.producer(),
		Usage:      usage,
		StopReason: string(stop.Reason),
	}
	return provider.StreamEvent{Kind: provider.SEEnd, Stop: stop, Message: m, Usage: usage}
}

// onlyReasoning reports an output that is reasoning and nothing else, which is how an exhausted output budget looks.
func (d *decoder) onlyReasoning() bool {
	seen := false
	for _, s := range d.blocks {
		switch v := s.out.(type) {
		case *message.ReasoningBlock:
			seen = true
		case *message.TextBlock:
			if v.Text != "" || s.text.Len() > 0 {
				return false
			}
		default:
			return false
		}
	}
	return seen
}

// finalize keeps closed blocks, drops empty text, and removes every run of reasoning not followed right away by another item, which the API would reject on replay.
func finalize(slots []*slot) message.Content {
	var kept []message.Block
	for _, s := range slots {
		if !s.closed {
			continue
		}
		if t, ok := s.out.(*message.TextBlock); ok && t.Text == "" {
			continue
		}
		kept = append(kept, s.out)
	}
	out := make(message.Content, 0, len(kept))
	for i, b := range kept {
		if _, ok := b.(*message.ReasoningBlock); ok {
			j := i
			for j < len(kept) {
				if _, r := kept[j].(*message.ReasoningBlock); !r {
					break
				}
				j++
			}
			if j == len(kept) {
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

func (d *decoder) currentUsage() *message.Usage {
	if d.usage == nil {
		return &message.Usage{Estimated: true}
	}
	var u struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		InputDetails struct {
			CachedTokens     int64 `json:"cached_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	_ = json.Unmarshal(d.usage, &u)
	return &message.Usage{
		InputTokens:      u.InputTokens,
		CacheReadTokens:  u.InputDetails.CachedTokens,
		CacheWriteTokens: u.InputDetails.CacheWriteTokens,
		OutputTokens:     u.OutputTokens,
		ReasoningTokens:  u.OutputDetails.ReasoningTokens,
		HostedCalls:      d.hostedCalls(),
		Raw:              compact(d.usage),
	}
}

func (d *decoder) hostedCalls() map[string]int64 {
	if d.searches == 0 {
		return nil
	}
	return map[string]int64{"web_search": d.searches}
}

func (d *decoder) producer() *message.Producer {
	p := &message.Producer{API: string(api), Endpoint: d.endpoint, Model: d.req.Model, ResponseID: d.responseID, RequestID: d.requestID, Headers: d.headers}
	if f := d.req.Caps.Family; f != "" && f != d.req.Model {
		p.Family = f
	}
	return p
}

// searchArguments is a tool_search_call's arguments as compact JSON, whether the item carries them as an object or as a JSON string.
func searchArguments(item json.RawMessage) json.RawMessage {
	var it struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(item, &it) != nil || len(it.Arguments) == 0 || string(it.Arguments) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(it.Arguments, &s) == nil {
		it.Arguments = json.RawMessage(s)
	}
	if !json.Valid(it.Arguments) {
		return nil
	}
	return compact(it.Arguments)
}

// loadedTools names the definitions a tool_search_output loaded, reading into namespaces, whose functions are the tools.
func loadedTools(item json.RawMessage) []string {
	type def struct {
		Name  string `json:"name"`
		Tools []def  `json:"tools"`
	}
	var it struct {
		Tools []def `json:"tools"`
	}
	if json.Unmarshal(item, &it) != nil {
		return nil
	}
	var names []string
	var walk func([]def)
	walk = func(ds []def) {
		for _, d := range ds {
			if len(d.Tools) > 0 {
				walk(d.Tools)
			} else if d.Name != "" {
				names = append(names, d.Name)
			}
		}
	}
	walk(it.Tools)
	return names
}
