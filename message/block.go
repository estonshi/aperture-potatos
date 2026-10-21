package message

import (
	"encoding/json"
	"errors"
	"fmt"
)

type BlockType string

const (
	TypeText         BlockType = "text"
	TypeRefusal      BlockType = "refusal"
	TypeImage        BlockType = "image"
	TypeDocument     BlockType = "document"
	TypeReasoning    BlockType = "reasoning"
	TypeToolCall     BlockType = "tool_call"
	TypeToolResult   BlockType = "tool_result"
	TypeReminder     BlockType = "reminder"
	TypeOpaque       BlockType = "opaque"
	TypeHostedCall   BlockType = "hosted_call"
	TypeHostedResult BlockType = "hosted_result"
)

// Block is sealed: only the pointer types in this package implement it.
type Block interface {
	BlockType() BlockType
	sealed()
}

// Content encodes as a JSON array of objects discriminated by "type"; unknown types decode to *OpaqueBlock.
type Content []Block

func (c Content) MarshalJSON() ([]byte, error) {
	out := []byte{'['}
	for i, b := range c {
		if b == nil {
			return nil, errors.New("message: nil block in content")
		}
		if i > 0 {
			out = append(out, ',')
		}
		bb, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		out = append(out, bb...)
	}
	return append(out, ']'), nil
}

func (c *Content) UnmarshalJSON(b []byte) error {
	var elems []json.RawMessage
	if err := json.Unmarshal(b, &elems); err != nil {
		return err
	}
	if elems == nil {
		*c = nil
		return nil
	}
	out := make(Content, 0, len(elems))
	for i, e := range elems {
		blk, err := decodeBlock(e)
		if err != nil {
			return fmt.Errorf("message: content[%d]: %w", i, err)
		}
		out = append(out, blk)
	}
	*c = out
	return nil
}

func decodeBlock(e json.RawMessage) (Block, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(e, &head); err != nil {
		return nil, err
	}
	var typ string
	if err := json.Unmarshal(head["type"], &typ); err != nil || typ == "" {
		return nil, errors.New("block has no string type")
	}
	var blk interface {
		Block
		json.Unmarshaler
	}
	switch BlockType(typ) {
	case TypeText:
		blk = &TextBlock{}
	case TypeRefusal:
		blk = &RefusalBlock{}
	case TypeImage:
		blk = &ImageBlock{}
	case TypeDocument:
		blk = &DocumentBlock{}
	case TypeReasoning:
		blk = &ReasoningBlock{}
	case TypeToolCall:
		blk = &ToolCallBlock{}
	case TypeToolResult:
		blk = &ToolResultBlock{}
	case TypeReminder:
		blk = &ReminderBlock{}
	case TypeOpaque:
		blk = &OpaqueBlock{}
	case TypeHostedCall:
		blk = &HostedCallBlock{}
	case TypeHostedResult:
		blk = &HostedResultBlock{}
	default:
		o := &OpaqueBlock{Kind: typ, raw: append(json.RawMessage(nil), e...)}
		_ = json.Unmarshal(head["text"], &o.Text)
		return o, nil
	}
	if err := blk.UnmarshalJSON(e); err != nil {
		return nil, err
	}
	return blk, nil
}

type TextBlock struct {
	Text      string     `json:"text"`
	Citations []Citation `json:"citations,omitempty"`
	Native    *Native    `json:"native,omitempty"` // Anthropic text block with citations; Responses message item with phase

	extra map[string]json.RawMessage
}

type RefusalBlock struct {
	Text   string  `json:"text"`
	Native *Native `json:"native,omitempty"`

	extra map[string]json.RawMessage
}

type ImageBlock struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data,omitempty"` // standard base64 on the wire
	URL       string `json:"url,omitempty"`

	extra map[string]json.RawMessage
}

type DocumentBlock struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	Filename  string `json:"filename,omitempty"`

	extra map[string]json.RawMessage
}

type ReasoningKind string

const (
	ReasoningSummary   ReasoningKind = "summary"   // Anthropic thinking (display=summarized), Responses reasoning summary
	ReasoningRaw       ReasoningKind = "raw"       // Chat reasoning_content / reasoning, gpt-oss reasoning_text
	ReasoningProgress  ReasoningKind = "progress"  // Anthropic display=updates between-tool notes
	ReasoningOmitted   ReasoningKind = "omitted"   // Anthropic display=omitted: signature only
	ReasoningRedacted  ReasoningKind = "redacted"  // Anthropic redacted_thinking
	ReasoningEncrypted ReasoningKind = "encrypted" // Responses item with encrypted_content and no summary text
)

type ReasoningBlock struct {
	Kind   ReasoningKind `json:"kind"`
	Text   string        `json:"text,omitempty"`
	Native *Native       `json:"native,omitempty"` // signature / redacted data / encrypted_content live in Raw

	extra map[string]json.RawMessage
}

type ToolCallBlock struct {
	ID         string          `json:"id"` // provider call id (toolu_… / call_…); pairs with ToolResultBlock.CallID
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input"`                 // always a JSON object; "" from the wire becomes {}
	RawInput   string          `json:"raw_input,omitempty"`   // kept only when parsing failed
	InputError string          `json:"input_error,omitempty"` // InputInvalidJSON, InputNotObject or InputIncomplete
	Native     *Native         `json:"native,omitempty"`      // Responses function_call item (fc_ id, namespace, caller)

	extra map[string]json.RawMessage
}

// InputError values are fixed words rather than parser messages, so every binding records the same line.
const (
	InputInvalidJSON = "invalid_json"
	InputNotObject   = "not_object"
	InputIncomplete  = "incomplete" // the response was cut off while the arguments streamed
)

type Outcome string

const (
	OutcomeSuccess      Outcome = "success"
	OutcomeError        Outcome = "error"
	OutcomeDenied       Outcome = "denied"        // pre_tool_use, an approval rule or a reviewer said no
	OutcomeInterrupted  Outcome = "interrupted"   // canceled while running or before starting
	OutcomeNotExecuted  Outcome = "not_executed"  // truncated response, turn-exit repair, render-time healing, review ended by deny_and_stop or interruption
	OutcomeInvalidInput Outcome = "invalid_input" // arguments were not valid JSON
	OutcomeUnknownTool  Outcome = "unknown_tool"
	OutcomeSynthetic    Outcome = "synthetic" // supplied by a pre_tool_use hook instead of running the tool
	OutcomeLost         Outcome = "lost"      // process restarted before the result was recorded; never re-run
)

type ToolResultBlock struct {
	CallID     string          `json:"call_id"`
	Name       string          `json:"name,omitempty"`
	Content    Content         `json:"content"` // text / image / document only
	IsError    bool            `json:"is_error,omitempty"`
	Outcome    Outcome         `json:"outcome"`
	Pin        *Pin            `json:"pin,omitempty"`        // stored with the result, so Resume can rebuild the pin set
	Structured json.RawMessage `json:"structured,omitempty"` // for events and hooks; never sent to the model
	Cleared    bool            `json:"cleared,omitempty"`    // content replaced by the prune placeholder; the source line keeps the original

	extra map[string]json.RawMessage
}

// Pin is content the kernel re-injects into the next epoch head after compaction; the newest pin per key wins.
type Pin struct {
	Key       string `json:"key"`
	Content   string `json:"content"`
	MaxTokens int    `json:"max_tokens,omitempty"` // 0 means ContextPolicy.PinTokens
}

// ReminderBlock is runtime-authored context; adapters render it as a <system-reminder> text block.
type ReminderBlock struct {
	Source string `json:"source"` // opaque label: time, memory, hook:<name>, ext:<name>, interrupted, registry, pin:<key>
	Text   string `json:"text"`
	Unpin  string `json:"unpin,omitempty"` // pin key this reminder retires, so Resume drops it from the pin set

	extra map[string]json.RawMessage
}

// OpaqueBlock carries provider blocks the kernel does not understand (server tools, fallback, unknown types ...).
type OpaqueBlock struct {
	Kind   string  `json:"kind"`           // "anthropic.messages:server_tool_use", "anthropic.messages:fallback"; the type name for a block type this binding does not know
	Text   string  `json:"text,omitempty"` // readable fallback text when the block has any
	Native *Native `json:"native"`

	extra map[string]json.RawMessage
	raw   json.RawMessage // a block of unknown type, written back verbatim so newer lines survive a copy
}

// HostedCallBlock is a call of a tool the provider runs itself, such as web search; no tool result ever answers it.
type HostedCallBlock struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input,omitempty"`
	Status string          `json:"status,omitempty"` // completed | failed | incomplete; absent when the API does not say
	Native *Native         `json:"native,omitempty"`

	extra map[string]json.RawMessage
}

// HostedResultBlock is what the provider's own run of a hosted call returned; it may arrive in a later assistant message than the call.
type HostedResultBlock struct {
	CallID  string   `json:"call_id"`
	Name    string   `json:"name,omitempty"`
	Sources []Source `json:"sources,omitempty"`
	Error   string   `json:"error,omitempty"`  // the provider's error code, such as max_uses_exceeded
	Tools   []string `json:"tools,omitempty"`  // the tools a tool search loaded, by name
	Native  *Native  `json:"native,omitempty"` // absent when the call's own Native already carries the result, so nothing is sent back for it

	extra map[string]json.RawMessage
}

type Source struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	Age   string `json:"age,omitempty"` // as the provider wrote it, such as "April 30, 2025"
}

type Citation struct {
	URL       string `json:"url"`
	Title     string `json:"title,omitempty"`
	CitedText string `json:"cited_text,omitempty"`
	Start     int    `json:"start,omitempty"` // code point offsets into the text block, when the API gives them
	End       int    `json:"end,omitempty"`
}

// Native keeps the provider wire object; only an adapter of the same API reads it back.
type Native struct {
	API string          `json:"api"`
	Raw json.RawMessage `json:"raw"`
}

func (*TextBlock) BlockType() BlockType         { return TypeText }
func (*RefusalBlock) BlockType() BlockType      { return TypeRefusal }
func (*ImageBlock) BlockType() BlockType        { return TypeImage }
func (*DocumentBlock) BlockType() BlockType     { return TypeDocument }
func (*ReasoningBlock) BlockType() BlockType    { return TypeReasoning }
func (*ToolCallBlock) BlockType() BlockType     { return TypeToolCall }
func (*ToolResultBlock) BlockType() BlockType   { return TypeToolResult }
func (*ReminderBlock) BlockType() BlockType     { return TypeReminder }
func (*OpaqueBlock) BlockType() BlockType       { return TypeOpaque }
func (*HostedCallBlock) BlockType() BlockType   { return TypeHostedCall }
func (*HostedResultBlock) BlockType() BlockType { return TypeHostedResult }

func (*TextBlock) sealed()         {}
func (*RefusalBlock) sealed()      {}
func (*ImageBlock) sealed()        {}
func (*DocumentBlock) sealed()     {}
func (*ReasoningBlock) sealed()    {}
func (*ToolCallBlock) sealed()     {}
func (*ToolResultBlock) sealed()   {}
func (*ReminderBlock) sealed()     {}
func (*OpaqueBlock) sealed()       {}
func (*HostedCallBlock) sealed()   {}
func (*HostedResultBlock) sealed() {}

func (b TextBlock) MarshalJSON() ([]byte, error) {
	type w TextBlock
	return marshalBlock(TypeText, w(b), b.extra)
}

func (b *TextBlock) UnmarshalJSON(data []byte) error {
	type w TextBlock
	var v w
	extra, err := decodeBlockFields(data, TypeText, &v)
	*b, b.extra = TextBlock(v), extra
	return err
}

func (b RefusalBlock) MarshalJSON() ([]byte, error) {
	type w RefusalBlock
	return marshalBlock(TypeRefusal, w(b), b.extra)
}

func (b *RefusalBlock) UnmarshalJSON(data []byte) error {
	type w RefusalBlock
	var v w
	extra, err := decodeBlockFields(data, TypeRefusal, &v)
	*b, b.extra = RefusalBlock(v), extra
	return err
}

func (b ImageBlock) MarshalJSON() ([]byte, error) {
	type w ImageBlock
	return marshalBlock(TypeImage, w(b), b.extra)
}

func (b *ImageBlock) UnmarshalJSON(data []byte) error {
	type w ImageBlock
	var v w
	extra, err := decodeBlockFields(data, TypeImage, &v)
	*b, b.extra = ImageBlock(v), extra
	return err
}

func (b DocumentBlock) MarshalJSON() ([]byte, error) {
	type w DocumentBlock
	return marshalBlock(TypeDocument, w(b), b.extra)
}

func (b *DocumentBlock) UnmarshalJSON(data []byte) error {
	type w DocumentBlock
	var v w
	extra, err := decodeBlockFields(data, TypeDocument, &v)
	*b, b.extra = DocumentBlock(v), extra
	return err
}

func (b ReasoningBlock) MarshalJSON() ([]byte, error) {
	type w ReasoningBlock
	return marshalBlock(TypeReasoning, w(b), b.extra)
}

func (b *ReasoningBlock) UnmarshalJSON(data []byte) error {
	type w ReasoningBlock
	var v w
	extra, err := decodeBlockFields(data, TypeReasoning, &v)
	*b, b.extra = ReasoningBlock(v), extra
	return err
}

func (b ToolCallBlock) MarshalJSON() ([]byte, error) {
	type w ToolCallBlock
	if len(b.Input) == 0 {
		b.Input = json.RawMessage("{}")
	}
	return marshalBlock(TypeToolCall, w(b), b.extra)
}

func (b *ToolCallBlock) UnmarshalJSON(data []byte) error {
	type w ToolCallBlock
	var v w
	extra, err := decodeBlockFields(data, TypeToolCall, &v)
	*b, b.extra = ToolCallBlock(v), extra
	return err
}

func (b ToolResultBlock) MarshalJSON() ([]byte, error) {
	type w ToolResultBlock
	return marshalBlock(TypeToolResult, w(b), b.extra)
}

func (b *ToolResultBlock) UnmarshalJSON(data []byte) error {
	type w ToolResultBlock
	var v w
	extra, err := decodeBlockFields(data, TypeToolResult, &v)
	*b, b.extra = ToolResultBlock(v), extra
	return err
}

func (b ReminderBlock) MarshalJSON() ([]byte, error) {
	type w ReminderBlock
	return marshalBlock(TypeReminder, w(b), b.extra)
}

func (b *ReminderBlock) UnmarshalJSON(data []byte) error {
	type w ReminderBlock
	var v w
	extra, err := decodeBlockFields(data, TypeReminder, &v)
	*b, b.extra = ReminderBlock(v), extra
	return err
}

func (b OpaqueBlock) MarshalJSON() ([]byte, error) {
	if b.raw != nil {
		return b.raw, nil
	}
	type w OpaqueBlock
	return marshalBlock(TypeOpaque, w(b), b.extra)
}

func (b *OpaqueBlock) UnmarshalJSON(data []byte) error {
	type w OpaqueBlock
	var v w
	extra, err := decodeBlockFields(data, TypeOpaque, &v)
	*b, b.extra = OpaqueBlock(v), extra
	return err
}

func (b HostedCallBlock) MarshalJSON() ([]byte, error) {
	type w HostedCallBlock
	return marshalBlock(TypeHostedCall, w(b), b.extra)
}

func (b *HostedCallBlock) UnmarshalJSON(data []byte) error {
	type w HostedCallBlock
	var v w
	extra, err := decodeBlockFields(data, TypeHostedCall, &v)
	*b, b.extra = HostedCallBlock(v), extra
	return err
}

func (b HostedResultBlock) MarshalJSON() ([]byte, error) {
	type w HostedResultBlock
	return marshalBlock(TypeHostedResult, w(b), b.extra)
}

func (b *HostedResultBlock) UnmarshalJSON(data []byte) error {
	type w HostedResultBlock
	var v w
	extra, err := decodeBlockFields(data, TypeHostedResult, &v)
	*b, b.extra = HostedResultBlock(v), extra
	return err
}
