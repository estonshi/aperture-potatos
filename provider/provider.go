// Package provider is the provider SPI: a neutral request, a normalized event stream and classified errors, shared by every adapter.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"time"

	"github.com/estonshi/aperture-potatos/message"
)

// TextSource supplies a host's overrides of the fixed runtime texts an adapter writes, such as the healed-call result; a missing entry falls back to the built-in text.
type TextSource interface {
	Text(key string) (string, bool)
	File(name string) (string, bool)
}

// API names the wire protocol an adapter speaks; it keys the error rules, the mappings and ModelInfo.
type API string

// The APIs of the built-in adapters.
const (
	APIAnthropicMessages API = "anthropic.messages"
	APIOpenAIChat        API = "openai.chat"
	APIOpenAIResponses   API = "openai.responses"
)

// Provider is safe for concurrent use; one value may serve many sessions.
type Provider interface {
	API() API
	Endpoint() string
	// Stream returns a single-use iterator; breaking out of the loop closes the HTTP body before returning.
	Stream(ctx context.Context, req *Request) iter.Seq2[StreamEvent, error]
}

// TokenCounter is an optional capability, discovered with a type assertion.
type TokenCounter interface {
	CountTokens(ctx context.Context, req *Request) (int64, error)
}

// CapabilityResolver asks the endpoint what a model can do; fields it cannot learn stay zero, which means unknown.
type CapabilityResolver interface {
	// ResolveCapabilities receives the api and endpoint too, so one agent-level resolver can serve every provider.
	ResolveCapabilities(ctx context.Context, m ModelInfo) (Capabilities, error)
}

// Doer may rewrite the request URL, headers and body and replace the response body, so cloud backends plug in without adapter changes.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// DefaultClient is the client the adapters, ext/mcp, ext/peer/a2a and ext/litellm use when the host gives none; it follows redirects only within one host, since Go keeps custom credential headers such as x-api-key across hosts and resends the body on 307 and 308.
var DefaultClient = &http.Client{CheckRedirect: SameHostRedirect}

// SameHostRedirect is an http.Client CheckRedirect that refuses a redirect to another host or from https to http.
func SameHostRedirect(req *http.Request, via []*http.Request) error {
	first := via[0].URL
	switch {
	case len(via) >= 10:
		return errors.New("stopped after 10 redirects")
	case req.URL.Host != first.Host:
		return fmt.Errorf("refused a redirect from %s to another host, %s", first.Host, req.URL.Host)
	case first.Scheme == "https" && req.URL.Scheme != "https":
		return fmt.Errorf("refused a redirect from https to %s", req.URL.Scheme)
	}
	return nil
}

// ModelRef picks a model on a provider, and its Params are the defaults every request to that model starts from.
type ModelRef struct {
	Provider Provider
	Model    string
	Params   Params
}

// ModelInfo is the serializable identity of a ModelRef, used in events and snapshots.
type ModelInfo struct {
	API      API    `json:"api"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
}

// Request is the neutral request the kernel builds and each adapter renders into its own wire body.
type Request struct {
	Model          string               `json:"model"`
	System         []SystemBlock        `json:"system,omitempty"`
	Messages       []message.Message    `json:"messages"`
	Tools          []message.ToolDef    `json:"tools,omitempty"`
	HostedTools    []message.HostedTool `json:"hosted_tools,omitempty"` // sent after Tools; an API without hosted tools gets a hosted_tool_unsupported notice instead
	ToolChoice     ToolChoice           `json:"tool_choice,omitzero"`
	Params         Params               `json:"params,omitzero"`
	Caps           Capabilities         `json:"caps,omitzero"`             // resolved by the kernel; a zero field is unknown and never rewrites the request
	CacheKey       string               `json:"cache_key,omitempty"`       // prompt_cache_key / session affinity
	Replay         ReplayMode           `json:"replay,omitempty"`          // ReplayFull normally; ReplayFallback for the one-shot reasoning_replay retry
	PauseTail      bool                 `json:"pause_tail,omitempty"`      // Anthropic pause_turn continuation: the only request allowed to end with an assistant message
	CacheTail      int                  `json:"cache_tail,omitempty"`      // the rolling cache breakpoint stays within the first CacheTail messages, so a side call reads the prefix the main loop cached; 0 means all
	SharedPrefix   int                  `json:"shared_prefix,omitempty"`   // the first SharedPrefix messages are another session's, a side chat's main part, so a breakpoint also goes where that session's request wrote its cache
	EmitRaw        bool                 `json:"emit_raw,omitempty"`        // also yield SERaw for provider.raw events
	CaptureHeaders []string             `json:"capture_headers,omitempty"` // from WithCaptureHeaders, so a gateway's call id reaches Producer.Headers

	FirstEventTimeout time.Duration `json:"-"` // from RetryPolicy; 0 uses the default in spec/data/limits.json, negative has none
	IdleTimeout       time.Duration `json:"-"` // from RetryPolicy; 0 uses the default in spec/data/limits.json, negative has none
	Texts             TextSource    `json:"-"` // the host's overrides of the fixed texts the adapter writes; nil for the built-ins
}

// SharedID is the last user-side message among the first SharedPrefix messages, which is where the session that owns them put its rolling breakpoint; "" when there is none.
func (r *Request) SharedID() string {
	if r.SharedPrefix <= 0 || r.SharedPrefix > len(r.Messages) {
		return ""
	}
	for i := r.SharedPrefix - 1; i >= 0; i-- {
		if m := r.Messages[i]; m.Role != message.RoleAssistant && m.ID != "" {
			return m.ID
		}
	}
	return ""
}

// ReplayMode says how much of the history's provider-native content, such as thinking signatures, an adapter sends back.
type ReplayMode uint8

const (
	ReplayFull     ReplayMode = iota // all the native content the history holds; every request but the fallback retry
	ReplayFallback                   // degraded replay after a reasoning_replay error; each API defines it in spec/mappings
)

var replayNames = map[ReplayMode]string{ReplayFull: "full", ReplayFallback: "fallback"}

// MarshalJSON writes the mode by name, as fixtures record it.
func (m ReplayMode) MarshalJSON() ([]byte, error) { return marshalName(replayNames, m) }

// UnmarshalJSON reads the name MarshalJSON writes.
func (m *ReplayMode) UnmarshalJSON(b []byte) error { return unmarshalName(replayNames, b, m) }

// SystemBlock is one rendered section of the system prompt, kept apart so adapters can place cache breakpoints between sections.
type SystemBlock struct {
	Slot  string `json:"slot"` // core | identity | capabilities | user | catalog | environment
	Name  string `json:"name,omitempty"`
	Text  string `json:"text"`
	Cache bool   `json:"cache,omitempty"` // cache breakpoint after this block (Anthropic cache_control)
}

// ToolChoiceMode says whether the model may, must or must not call a tool; an empty mode leaves the API's default.
type ToolChoiceMode string

// ToolChoiceNamed forces the tool in ToolChoice.Name; the others match the APIs' auto, none and required.
const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceNamed    ToolChoiceMode = "named"
)

// ToolChoice falls back to auto, with a notice, when the model cannot be forced to call a tool.
type ToolChoice struct {
	Mode            ToolChoiceMode `json:"mode,omitempty"`
	Name            string         `json:"name,omitempty"`
	DisableParallel bool           `json:"disable_parallel,omitempty"`
}

// ReasoningMode turns reasoning on or off, or with auto leaves it to the model's declared default.
type ReasoningMode string

// ReasoningOn and ReasoningOff override the model's default, which ReasoningAuto leaves alone.
const (
	ReasoningAuto ReasoningMode = "auto" // "" means the same
	ReasoningOn   ReasoningMode = "on"
	ReasoningOff  ReasoningMode = "off"
)

// ReasoningParams is mapped onto each API's reasoning fields as far as the model's Reasoning capabilities allow.
type ReasoningParams struct {
	Mode   ReasoningMode `json:"mode,omitempty"`
	Effort string        `json:"effort,omitempty"` // none|minimal|low|medium|high|xhigh|max; remapped only when the model's Reasoning.Efforts is known
	Budget int           `json:"budget,omitempty"` // Anthropic manual budget_tokens; Chat has no generic mapping, so vendor budgets go through Params.Extra
	Hide   bool          `json:"hide,omitempty"`   // zero value requests readable CoT whenever the request asks for reasoning at all; true opts out
}

// Params are the per-request generation parameters, which a pre_model_request hook may patch before they reach the wire.
type Params struct {
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"` // 0 means DefaultMaxOutputTokens; the kernel has already applied the window clamp
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	StopSequences   []string        `json:"stop_sequences,omitempty"`
	Reasoning       ReasoningParams `json:"reasoning,omitzero"`
	Headers         http.Header     `json:"headers,omitempty"`
	Extra           map[string]any  `json:"extra,omitempty"` // merged into the wire body last; escape hatch for new fields and vendor switches
}

func marshalName[T comparable](names map[T]string, v T) ([]byte, error) {
	s, ok := names[v]
	if !ok {
		return nil, fmt.Errorf("provider: unknown %T %v", v, v)
	}
	return json.Marshal(s)
}

func unmarshalName[T comparable](names map[T]string, b []byte, v *T) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	for k, n := range names {
		if n == s {
			*v = k
			return nil
		}
	}
	return fmt.Errorf("provider: unknown %T %q", *v, s)
}
