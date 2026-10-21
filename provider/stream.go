package provider

import (
	"encoding/json"

	"github.com/estonshi/aperture-potatos/message"
)

// StreamEventKind says which fields of a StreamEvent are set.
type StreamEventKind uint8

const (
	SEStart          StreamEventKind = iota + 1 // carries ResponseID, RequestID and Headers
	SEBlockStart                                // opens block Index with its BlockType, and ToolCallID and ToolName for a call
	SETextDelta                                 // Text appended to block Index on Channel
	SEReasoningDelta                            // readable reasoning of ReasoningKind appended to block Index
	SEToolArgsDelta                             // a fragment of the call's JSON arguments, which parse only once the block stops
	SEBlockStop                                 // Block holds the final block including Native
	SEUsage                                     // cumulative snapshot; latest wins; kept inside the driver, never published as an event
	SENotice                                    // fallback, capability adjusted, dangling call healed, thinking dropped
	SERaw                                       // one wire event in Raw with its type in Text; only when Request.EmitRaw is set
	SEEnd                                       // Stop, Message and Usage are never nil; missing usage is filled with an Estimated value
)

var kindNames = map[StreamEventKind]string{
	SEStart: "start", SEBlockStart: "block_start", SETextDelta: "text_delta", SEReasoningDelta: "reasoning_delta",
	SEToolArgsDelta: "tool_args_delta", SEBlockStop: "block_stop", SEUsage: "usage", SENotice: "notice", SERaw: "raw", SEEnd: "end",
}

// String returns the name the kind is written with in fixtures.
func (k StreamEventKind) String() string { return kindNames[k] }

// MarshalJSON writes the kind by name.
func (k StreamEventKind) MarshalJSON() ([]byte, error) { return marshalName(kindNames, k) }

// UnmarshalJSON reads the name MarshalJSON writes.
func (k *StreamEventKind) UnmarshalJSON(b []byte) error { return unmarshalName(kindNames, b, k) }

// StreamEvent is one normalized event of a model stream; Kind says which fields it carries.
type StreamEvent struct {
	Kind          StreamEventKind       `json:"kind"`
	Index         int                   `json:"index,omitempty"` // block index inside the assistant message
	BlockType     message.BlockType     `json:"block_type,omitempty"`
	Channel       string                `json:"channel,omitempty"` // text | refusal, for SETextDelta
	Text          string                `json:"text,omitempty"`
	ReasoningKind message.ReasoningKind `json:"reasoning_kind,omitempty"`
	ToolCallID    string                `json:"tool_call_id,omitempty"`
	ToolName      string                `json:"tool_name,omitempty"`
	Block         message.Block         `json:"block,omitempty"`
	Usage         *message.Usage        `json:"usage,omitempty"`
	Stop          *Stop                 `json:"stop,omitempty"`
	Message       *message.Message      `json:"message,omitempty"`
	Notice        *Notice               `json:"notice,omitempty"`
	Raw           json.RawMessage       `json:"raw,omitempty"`
	ResponseID    string                `json:"response_id,omitempty"` // SEStart: the provider's response id, which gateways such as LiteLLM key their spend logs by
	RequestID     string                `json:"request_id,omitempty"`  // SEStart: the request-id / x-request-id response header, for provider support tickets
	Headers       map[string]string     `json:"headers,omitempty"`     // SEStart: the response headers named in Request.CaptureHeaders, keys lowercased
}

// StopReason is the neutral reason a response ended, mapped from each API's own value by spec/data/stop_reasons.json.
type StopReason string

// StopPause is Anthropic's pause_turn, which the kernel continues with Request.PauseTail; StopOther stands for any value the mapping does not list.
const (
	StopEndTurn       StopReason = "end_turn"
	StopToolUse       StopReason = "tool_use"
	StopMaxTokens     StopReason = "max_tokens"
	StopSequence      StopReason = "stop_sequence"
	StopPause         StopReason = "pause"
	StopRefusal       StopReason = "refusal"
	StopContentFilter StopReason = "content_filter"
	StopContextWindow StopReason = "context_window_exceeded"
	StopOther         StopReason = "other"
)

// Stop says why a response ended; ReasoningExhausted marks one whose output limit was spent entirely on reasoning.
type Stop struct {
	Reason             StopReason      `json:"reason"`
	Raw                string          `json:"raw,omitempty"`     // provider value, e.g. "tool_calls", "pause_turn"
	Details            json.RawMessage `json:"details,omitempty"` // stop_details / incomplete_details
	ReasoningExhausted bool            `json:"reasoning_exhausted,omitempty"`
}

// Notice reports something an adapter adjusted or repaired on its own, and the kernel publishes it as a model.notice event.
type Notice struct {
	Kind   string          `json:"kind"` // one of the model.notice kinds
	Detail json.RawMessage `json:"detail,omitempty"`
}
