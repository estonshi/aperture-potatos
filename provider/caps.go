package provider

const (
	DefaultContextWindow   = 200000 // used when neither the caller nor the resolver knows the window
	DefaultMaxOutputTokens = 64000  // not 65536, because some models cap output at exactly 64,000
)

// Capabilities declares what a model can do; every zero field means unknown, and unknown never rewrites the request.
type Capabilities struct {
	Family                  string        `json:"family,omitempty"`
	ContextWindow           int           `json:"context_window,omitempty"`
	MaxOutputTokens         int           `json:"max_output_tokens,omitempty"`
	NoTools                 bool          `json:"no_tools,omitempty"`
	NoParallelTools         bool          `json:"no_parallel_tools,omitempty"`
	NoWebSearch             bool          `json:"no_web_search,omitempty"`           // the neutral web search is then not sent
	NoToolSearch            bool          `json:"no_tool_search,omitempty"`          // the neutral tool search is then not sent, and deferred tools go as ordinary ones
	NoToolsWithReasoning    bool          `json:"no_tools_with_reasoning,omitempty"` // OpenAI chat GPT-5.4+: reasoning_effort must be none when tools are present
	Reasoning               ReasoningCaps `json:"reasoning,omitzero"`
	NoSampling              bool          `json:"no_sampling,omitempty"`            // temperature/top_p/top_k are stripped
	SamplingWithThinking    string        `json:"sampling_with_thinking,omitempty"` // all | top_p_only | none: what survives while thinking is on
	NoForcedToolChoice      bool          `json:"no_forced_tool_choice,omitempty"`
	NoToolChoiceNone        bool          `json:"no_tool_choice_none,omitempty"`
	NoImagesInToolResult    bool          `json:"no_images_in_tool_result,omitempty"`
	NoMidConversationSystem bool          `json:"no_mid_conversation_system,omitempty"`
	ThinkingPrefixBound     bool          `json:"thinking_prefix_bound,omitempty"` // editing the prefix, tools included, invalidates thinking signatures
	PromptCache             CacheCaps     `json:"prompt_cache,omitzero"`
	SystemRole              string        `json:"system_role,omitempty"`      // chat only; "" sends "system"
	MaxTokensField          string        `json:"max_tokens_field,omitempty"` // chat only; "" sends "max_tokens"
}

// ReasoningCaps declares the model's reasoning controls, so a request asks only for forms and efforts the model accepts.
type ReasoningCaps struct {
	Visible         string   `json:"visible,omitempty"`    // none | summary | raw
	Modes           []string `json:"modes,omitempty"`      // anthropic: adaptive | enabled; openai: effort
	DefaultOn       bool     `json:"default_on,omitempty"` // Mode=auto asks Anthropic for display:"summarized" only when this is known to be true
	NoDisable       bool     `json:"no_disable,omitempty"` // read only when Modes is known, so an undeclared model still receives Mode=off
	Efforts         []string `json:"efforts,omitempty"`
	BudgetMin       int      `json:"budget_min,omitempty"`       // manual budget_tokens floor (1024 on Anthropic)
	InterleavedBeta bool     `json:"interleaved_beta,omitempty"` // manual thinking with tools needs interleaved-thinking-2025-05-14
}

// CacheCaps declares the model's prompt caching; Explicit makes the OpenAI adapters mark cache breakpoints, which Anthropic requests always carry.
type CacheCaps struct {
	Explicit        bool `json:"explicit,omitempty"`
	MaxBreakpoints  int  `json:"max_breakpoints,omitempty"` // only a value below the API's own limit changes anything
	MinPrefixTokens int  `json:"min_prefix_tokens,omitempty"`
}
