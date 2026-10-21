// Package message holds the canonical, provider-neutral history types; a Message's JSON is exactly one line of a session file.
package message

import (
	"encoding/json"
	"strings"
	"time"
)

// Version is the line format version written to the v field of every session file line.
const Version = 1

type Role string

const (
	RoleSystem    Role = "system" // first line of every epoch; sent as the system prompt, never as a conversation message
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // one parallel batch of results in call order; its own role because every API renders it differently
)

type OriginKind string

const (
	OriginHuman        OriginKind = "human"
	OriginCommand      OriginKind = "command"      // expanded prompt-template command
	OriginAgent        OriginKind = "agent"        // another agent: subagent prompt, peer or A2A inbound
	OriginTask         OriginKind = "task"         // task notification; only the task manager writes it, so Send rejects it
	OriginReminder     OriginKind = "reminder"     // reminder-only user message
	OriginContinuation OriginKind = "continuation" // stop-hook, truncation and empty-reply nudges
	OriginEpochHead    OriginKind = "epoch_head"   // instructions, summary and pins at the start of an epoch
	OriginModel        OriginKind = "model"
	OriginTool         OriginKind = "tool"
	OriginSystem       OriginKind = "system"
)

// Origin tells the model and the hooks whether a human authored the message.
type Origin struct {
	Kind   OriginKind `json:"kind"`
	Source string     `json:"source,omitempty"` // opaque to the kernel: "hook:audit", "agent:Tudou", "a2a:<caller>"
}

// Producer records which API and model generated an assistant message; replay decisions key on it.
type Producer struct {
	API        string            `json:"api"`      // anthropic.messages | openai.chat | openai.responses | synthetic
	Endpoint   string            `json:"endpoint"` // normalized host, e.g. api.deepseek.com
	Model      string            `json:"model"`
	Family     string            `json:"family,omitempty"`      // omitted when it equals the model, so readers take a missing family as the model name
	ResponseID string            `json:"response_id,omitempty"` // gateways such as LiteLLM key their spend logs by it
	RequestID  string            `json:"request_id,omitempty"`  // the API's request id response header
	Headers    map[string]string `json:"headers,omitempty"`     // response headers named by WithCaptureHeaders
}

// Message is immutable once committed; its JSON is exactly one line of the session file.
type Message struct {
	V           int          `json:"v"`
	ID          string       `json:"id"`
	SessionID   string       `json:"session_id"`
	Seq         int64        `json:"seq"`
	Epoch       int          `json:"epoch"`
	Role        Role         `json:"role"`
	Origin      Origin       `json:"origin"`
	Content     Content      `json:"content"`
	Producer    *Producer    `json:"producer,omitempty"`
	Usage       *Usage       `json:"usage,omitempty"`
	StopReason  string       `json:"stop_reason,omitempty"`
	Native      *Native      `json:"native,omitempty"` // message-level raw: chat assistant message, OpenRouter reasoning_details
	TurnID      string       `json:"turn_id,omitempty"`
	Partial     bool         `json:"partial,omitempty"` // committed after an interrupt; only closed blocks are kept
	Boundary    *Boundary    `json:"boundary,omitempty"`
	Tools       []ToolDef    `json:"tools,omitempty"`
	HostedTools []HostedTool `json:"hosted_tools,omitempty"`
	Tasks       []TaskRef    `json:"tasks,omitempty"` // a list, because one batch can start several tasks and one notification can cover several
	CopyOf      string       `json:"copy_of,omitempty"`
	Batch       string       `json:"batch,omitempty"`     // storage only: shared by the lines of one multi-line Append, so Load can find a batch a crash tore
	BatchEnd    bool         `json:"batch_end,omitempty"` // storage only: a batch whose last line never landed is dropped as a whole
	CreatedAt   time.Time    `json:"ts"`

	extra map[string]json.RawMessage // fields this binding does not know, written back so a copy or rewrite of a newer line loses nothing
}

type messageFields Message

func (m Message) MarshalJSON() ([]byte, error) {
	w := messageFields(m)
	w.CreatedAt = m.CreatedAt.UTC()
	b, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	return appendExtra(b, m.extra), nil
}

func (m *Message) UnmarshalJSON(b []byte) error {
	var w messageFields
	extra, err := decodeKnown(b, &w, nil)
	if err != nil {
		return err
	}
	*m = Message(w)
	m.extra = extra
	return nil
}

// Boundary opens an epoch; the current context is the last line carrying one plus every line after it.
type Boundary struct {
	Reason         string        `json:"reason"`             // new | clear | compaction | switch | rewind | fork | dropped
	Trigger        string        `json:"trigger,omitempty"`  // auto | manual | overflow | switch
	Strategy       string        `json:"strategy,omitempty"` // "summarize" or "prune" for ext/compact; a custom compactor's own value
	PreTokens      int64         `json:"pre_tokens,omitempty"`
	Range          *CompactRange `json:"range,omitempty"`           // partial compaction only
	TurnID         string        `json:"turn_id,omitempty"`         // rewind target
	From           *ForkFrom     `json:"from,omitempty"`            // fork source
	ToolsAllowed   *[]string     `json:"tools_allowed,omitempty"`   // a fork's call-time allow-list, kept on resume; a pointer, since an empty list allows no tool
	ReplayFallback bool          `json:"replay_fallback,omitempty"` // kept lines follow changed content on a prefix-bound model, so the epoch renders with ReplayFallback
	Session        *SessionMeta  `json:"session,omitempty"`         // first line of the file only
}

// CompactRange says which side of a turn a partial compaction summarized.
type CompactRange struct {
	Kind   string `json:"kind"` // from | up_to
	TurnID string `json:"turn_id"`
}

// ForkFrom names the lines a fork copied: the source session up to seq.
type ForkFrom struct {
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
}

type SessionMeta struct {
	Agent           string            `json:"agent"`
	ParentSessionID string            `json:"parent_session_id,omitempty"`
	SideChat        string            `json:"side_chat,omitempty"` // name, on a side chat's file
	Labels          map[string]string `json:"labels,omitempty"`
	SpecVersion     string            `json:"spec_version"`
}

// TaskRef is the only persisted trace of a task, which is why it carries the spec and the last saved state.
type TaskRef struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Title   string          `json:"title,omitempty"`
	Status  string          `json:"status"`
	Attempt int             `json:"attempt"`
	Spec    json.RawMessage `json:"spec,omitempty"`
	State   json.RawMessage `json:"state,omitempty"`
}

// ToolDef lives here rather than in provider, so a system line can freeze it and store depends on message alone.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Strict      bool            `json:"strict,omitempty"`
	Deferred    bool            `json:"deferred,omitempty"` // loaded on demand when the request carries a tool search; an ordinary definition otherwise
}

// HostedTool is a tool the provider runs; exactly one of WebSearch, ToolSearch and Raw is set.
type HostedTool struct {
	Name       string                     `json:"name"`                  // what the model calls it; unique among the session's tools
	WebSearch  *WebSearch                 `json:"web_search,omitempty"`  // the neutral web search; Name must be "web_search"
	ToolSearch *ToolSearch                `json:"tool_search,omitempty"` // the neutral tool search over the deferred tools; Name must be "tool_search"
	Raw        map[string]json.RawMessage `json:"raw,omitempty"`         // API → that API's own tool definition, appended verbatim
}

// ToolSearch has no options, because every API it maps to searches by natural language.
type ToolSearch struct{}

type WebSearch struct {
	MaxUses        int           `json:"max_uses,omitempty"`
	AllowedDomains []string      `json:"allowed_domains,omitempty"` // at most one of the two lists
	BlockedDomains []string      `json:"blocked_domains,omitempty"`
	UserLocation   *UserLocation `json:"user_location,omitempty"`
	ContextSize    string        `json:"context_size,omitempty"` // low | medium | high
}

type UserLocation struct {
	City     string `json:"city,omitempty"`
	Region   string `json:"region,omitempty"`
	Country  string `json:"country,omitempty"`  // ISO 3166-1 alpha-2
	Timezone string `json:"timezone,omitempty"` // IANA id
}

// UserText is a human-authored user message with one text block.
func UserText(s string) Message {
	return Message{Role: RoleUser, Origin: Origin{Kind: OriginHuman}, Content: Content{&TextBlock{Text: s}}}
}

// AssistantText is an assistant message that no model produced, so replay never treats it as a provider's output.
func AssistantText(s string) Message {
	return Message{
		Role:     RoleAssistant,
		Origin:   Origin{Kind: OriginModel},
		Content:  Content{&TextBlock{Text: s}},
		Producer: &Producer{API: "synthetic"},
	}
}

// Text concatenates the text of the message's text blocks.
func (m Message) Text() string {
	var sb strings.Builder
	for _, b := range m.Content {
		if t, ok := b.(*TextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

// ToolCalls returns copies of the message's tool calls in content order.
func (m Message) ToolCalls() []ToolCallBlock {
	var out []ToolCallBlock
	for _, b := range m.Content {
		if c, ok := b.(*ToolCallBlock); ok {
			out = append(out, *c)
		}
	}
	return out
}

// Empty reports that no block is left after dropping empty text blocks; such a message is never committed (RULE-MSG-001).
func (m Message) Empty() bool {
	for _, b := range m.Content {
		if t, ok := b.(*TextBlock); ok && t.Text == "" {
			continue
		}
		return false
	}
	return true
}
