[← Previous: Core objects](03-core-objects.md) · [Contents](README.md) · [Next: Sending input and waiting for results →](05-input.md)

# 4. Connecting models

This chapter explains how to connect an agent to a model: choosing an adapter, setting request parameters, declaring model capabilities and switching models at runtime. Refer to it when you need to connect a new model or gateway, or when a model does not behave the way you expect.

In this chapter: [4.1 The three adapters](#41-the-three-adapters) · [4.2 Choosing an adapter](#42-choosing-an-adapter) · [4.3 Model references and parameters](#43-model-references-and-parameters) · [4.4 Model capabilities](#44-model-capabilities) · [4.5 Reasoning](#45-reasoning) · [4.6 Switching models at runtime](#46-switching-models-at-runtime) · [4.7 Server-side tools](#47-server-side-tools) · [4.8 Gateways and custom HTTP](#48-gateways-and-custom-http) · [4.9 Best practices](#49-best-practices)

## 4.1 The three adapters

The SDK talks to model APIs through adapters. It currently provides three, one each for the Anthropic Messages, OpenAI Responses and OpenAI Chat Completions APIs. To create an adapter you only need to supply a key. If you do not specify an address, each adapter uses its vendor's official default address:

```go
claude := anthropic.New(anthropic.Config{APIKey: key})             // default https://api.anthropic.com
gpt    := openairesponses.New(openairesponses.Config{APIKey: key}) // default https://api.openai.com/v1
chat   := openaichat.New(openaichat.Config{APIKey: key})           // default https://api.openai.com/v1
```

All three adapters can point at a gateway or a compatible service through `BaseURL`. For example, to connect to a service that is compatible with Chat Completions:

```go
chat := openaichat.New(openaichat.Config{APIKey: key, BaseURL: os.Getenv("CHAT_BASE_URL")})
```

`Config` has the following fields:

| Field | Description |
|---|---|
| `APIKey` | The key used to access the API. |
| `BaseURL` | The address of the gateway or compatible service. For Chat and Responses, the address must include the version path, such as `/v1`. The Anthropic address does not need one. |
| `Doer` | A custom HTTP client. Any value that implements `Do(*http.Request) (*http.Response, error)` works. When it is `nil`, `provider.DefaultClient` is used. That client follows redirects only within the same host and never carries the API key or the request body to another host. The adapter manages stream timeouts itself, so the client does not need a timeout. |
| `Betas` | Supported only by the Anthropic adapter. Every request carries these beta headers, and the adapter also adds the beta headers that certain features require automatically. |

Each adapter implements the `provider.Provider` interface, which consists of three methods: `API()`, `Endpoint()` and `Stream()`. Adapters are safe for concurrent use, so many sessions can share one instance. The Anthropic adapter also implements `CountTokens`, which is used for an exact token count when the context approaches the compaction threshold.

## 4.2 Choosing an adapter

Which adapter you use depends on the model and the service you are connecting to. The table below lists the common pairings:

| Service you connect to | Adapter to use |
|---|---|
| Anthropic's own API, or Claude on Bedrock or Vertex through a gateway | Use `anthropic`. |
| OpenAI's own newer models | Use `openairesponses`. On these models, the Chat Completions API cannot use reasoning and tools at the same time. |
| DeepSeek, Qwen, Kimi, vLLM, Ollama, OpenRouter and other OpenAI-compatible services | Use `openaichat`. |
| A LiteLLM gateway | Choose by the model behind the gateway. Use `anthropic` for Claude, and `openaichat` or `openairesponses` for other models. In every case, `BaseURL` points at the gateway. |

## 4.3 Model references and parameters

`provider.ModelRef` describes which adapter, which model name and which parameters a model call uses. You can pass it to `kernel.WithModel` to set the agent's model, or to `sess.SwitchModel` to switch models at runtime:

```go
ref := provider.ModelRef{
	Provider: claude,
	Model:    "claude-opus-5-5",
	Params: provider.Params{
		MaxOutputTokens: 32000,
		Reasoning:       provider.ReasoningParams{Mode: provider.ReasoningOn, Effort: "high"},
	},
}
```

`provider.Params` has the following fields:

| Field | Description |
|---|---|
| `MaxOutputTokens` | When it is 0, the default of 64,000 is used. If the model's output limit is known, the value is capped at that limit. If only the window size is known, the value is capped at a quarter of the window. |
| `Temperature`, `TopP` | Of type `*float64`. Not sent when nil. |
| `StopSequences` | Stop sequences. |
| `Reasoning.Mode` | One of `auto` (the default), `on` and `off`; see [4.5](#45-reasoning). |
| `Reasoning.Effort` | One of `none`, `minimal`, `low`, `medium`, `high`, `xhigh` and `max`. If you have declared the levels the model supports, the value is mapped to the closest of them. |
| `Reasoning.Budget` | Corresponds to Anthropic's manual `budget_tokens`. The Chat API has no general mapping for it, so put a vendor's own budget field in `Extra`. |
| `Reasoning.Hide` | When true, reasoning is turned on, but no readable reasoning text is retrieved. |
| `Headers` | Extra request headers, of type `http.Header`. |
| `Extra` | Fields merged into the request body last, used to pass vendor switches or new fields, such as `{"enable_thinking": true}`. |

For models declared with `NoSampling`, the SDK removes the sampling parameters. To change parameters temporarily for a single step, use `ParamsPatch` in the `pre_model_request` hook, which can also set `ToolChoice`; see [9.4](09-hooks.md#94-inputs-and-outputs-of-each-point).

## 4.4 Model capabilities

The SDK has no built-in model table. You declare information such as the window size, the output limit and whether sampling parameters are supported, or the SDK looks it up at runtime. The SDK never rewrites a request based on a capability it does not know, so without a declaration, the request is sent exactly as you configured it.

Declare capabilities with `kernel.WithCapabilities`:

```go
kernel.WithCapabilities(provider.APIAnthropicMessages, "claude-opus-5-5*", provider.Capabilities{
	ContextWindow:       1000000,
	ThinkingPrefixBound: true, // declare this if changing earlier context (including tool definitions) invalidates thinking signatures
	Reasoning:           provider.ReasoningCaps{DefaultOn: true, Modes: []string{"adaptive"}},
})
```

The second argument is a glob pattern for the model name, in which `*` matches any characters. You can declare the same model more than once; later declarations override earlier ones field by field.

Capabilities come from three sources. In order of precedence, from highest to lowest, they are declarations made with `WithCapabilities`, runtime lookups and defaults. The defaults are a 200k window and 64k of output. All three adapters look up capabilities from their endpoint: Anthropic queries the Models API, and the other two query `GET {base}/models`, including the `model_info` that LiteLLM returns. By default, the kernel uses each Provider's own lookup, so no extra configuration is needed.

If you want to replace the lookup source across the board, for example with a query against your own model table, use `kernel.WithCapabilityResolver(r)`. It replaces the lookup for every Provider under this agent. Passing `nil` turns runtime lookups off.

`provider.Capabilities` has the following fields:

| Field | Meaning |
|---|---|
| `Family` | The model family name. When you switch models, it is used to decide whether the history can be sent back unchanged. If it is not set, the decision is based on the model name. |
| `ContextWindow`, `MaxOutputTokens` | The context window size and the output limit. |
| `NoTools` | The model does not support tools. |
| `NoParallelTools` | The model issues only one tool call at a time. |
| `NoWebSearch`, `NoToolSearch` | Do not send the neutral web search or tool search. |
| `NoToolsWithReasoning` | Reasoning must be turned off when tools are present. Some GPT models on OpenAI Chat behave this way. |
| `NoSampling` | Removes temperature and top_p. |
| `SamplingWithThinking` | Which sampling parameters can still be sent while reasoning is on: `all`, `top_p_only` or `none`. |
| `NoForcedToolChoice`, `NoToolChoiceNone` | Forcing a tool call is not supported, and `tool_choice: none` is not supported, respectively. |
| `NoImagesInToolResult` | Tool results cannot contain images. |
| `NoMidConversationSystem` | System messages cannot be inserted in the middle of a conversation. |
| `ThinkingPrefixBound` | Changing earlier context invalidates thinking signatures. When this is declared, tool definitions are fixed for each epoch, and switching to such a model starts a new epoch. |
| `Reasoning` | Of type `ReasoningCaps`, which contains `Visible` (one of `none`, `summary` and `raw`), `Modes`, `DefaultOn`, `NoDisable`, `Efforts`, `BudgetMin` and `InterleavedBeta`. |
| `PromptCache` | Of type `CacheCaps`, which contains `Explicit`, `MaxBreakpoints` and `MinPrefixTokens`. `Explicit` means that cache breakpoints must be set explicitly. |
| `SystemRole`, `MaxTokensField` | Used only by the Chat API. They are the role used for system messages and the name of the output limit field, respectively. |

If the API returns an error because the output limit is too large or because it does not recognize a field name, the SDK adjusts the request based on the error and retries once. It also emits a `model.notice` event that suggests you add a declaration.

The `Kind` of `model.notice` takes the following values. `capability_adjusted` means the SDK changed the request based on capabilities, `capability_lookup_failed` means a runtime lookup failed, and `capability_hint` suggests that you declare a capability. The other values are `fallback`, `reasoning_summary_disabled`, `reasoning_replay_fallback`, `thinking_dropped`, `reasoning_exhausted`, `sequence_regression`, `dangling_tool_call_healed`, `empty_reply_continue`, `hosted_tool_unsupported` and `hosted_tool_adjusted`. We recommend logging these events during development.

## 4.5 Reasoning

Reasoning content is the readable text a model produces while it reasons. Whether reasoning is on is controlled by `Reasoning.Mode`. With `auto`, the SDK retrieves readable reasoning only from models known to have reasoning on by default, as indicated by `ReasoningCaps.DefaultOn`. `on` explicitly requests reasoning, and `off` explicitly turns it off.

When reasoning content is available, the SDK emits it as `reasoning.started`, `reasoning.delta` and `reasoning.completed` events. The `Kind` in these events describes the nature of the content and takes the following values:

- `summary` is a summary provided by the vendor.
- `raw` is the original reasoning text.
- `progress` is a progress note between tool calls.
- `omitted`, `redacted` and `encrypted` mean that there is only a signature or ciphertext, with no text.

The Chat API automatically reads the `reasoning_content`, `reasoning`, `thinking_blocks` and `reasoning_details` fields, so you do not need to configure anything per vendor. Put vendor parameters that turn thinking on or off in `Params.Extra`.

Private fields such as thinking signatures and `encrypted_content` are stored unchanged, but they are sent back only to the same kind of API and the same model family. The Chat API also requires the same endpoint. If you switch to a different API or model family, these fields are no longer sent back. The model still works normally; it just cannot see its earlier reasoning.

Changes to reasoning settings take effect from the next turn. A single turn never mixes two settings.

## 4.6 Switching models at runtime

While a session is running, you can switch it to another model with `sess.SwitchModel`. The new model can use a different adapter:

```go
err := sess.SwitchModel(ctx, provider.ModelRef{Provider: gpt, Model: "gpt-5.6-sol"})
```

The switch takes effect between turns, and the history is reformatted for the new API before it is sent. `ctx` only bounds a capability lookup that is not cached yet. If the capabilities have already been looked up, the call returns immediately.

The switch proceeds as follows:

1. At the start of the next turn, before the switch actually takes effect, the SDK fires the `provider_switch` hook, which can block the switch. The hook's input includes `DroppedReasoning` and `DroppedOpaque`, which tell you how many reasoning blocks and vendor-private blocks cannot be sent to the new model.
2. If the current context does not fit in the new model's window, the SDK first compacts once with `switch` as the trigger, provided that a compaction strategy is installed. If the context still does not fit after compaction, the SDK rejects the switch and emits `provider.switch_rejected{reason:"capacity"}`, and that turn continues on the old model.
3. When the switch succeeds, the SDK emits a `provider.switched` event, whose `CacheReset` tells you whether a new epoch was started. If a hook blocks the switch, the SDK emits `provider.switch_rejected{reason:"hook_blocked"}`.

Switching to a model declared with `ThinkingPrefixBound` starts a new epoch. The system prompt is kept as it is, and the tool definitions are fixed again for the new model.

> Note: Some APIs are stricter about the history format. For example, DeepSeek with tools requires every assistant message to include a reasoning field. If requests are rejected after you switch, run `/compact` or `/clear` first and then try again.

## 4.7 Server-side tools

Server-side tools are tools that the model vendor runs on its own servers. Currently they are web search and tool search. You register them with `WithHostedTool`, and they are supported on the Anthropic Messages and OpenAI Responses APIs:

```go
kernel.WithHostedTool(message.HostedTool{
	Name:      "web_search",
	WebSearch: &message.WebSearch{MaxUses: 5, AllowedDomains: []string{"docs.python.org"}},
})
```

You can set only one of the following three fields of `message.HostedTool`:

| Field | Description |
|---|---|
| `WebSearch` | A neutral form of web search, which the adapter maps to each vendor's native tool. `Name` must be `web_search`. The available options are `MaxUses`, `AllowedDomains` or `BlockedDomains` (only one of the two), `UserLocation` and `ContextSize`. `UserLocation` includes the city, region, country code and time zone, and `ContextSize` is one of `low`, `medium` and `high`. |
| `ToolSearch` | A neutral form of tool search. `Name` must be `tool_search`. Once it is registered, tools whose `ToolSpec.Deferred` is true do not enter the context and are loaded only when the model finds them through search; see [7.8](07-tools.md#78-deferred-loading-and-tool-search). |
| `Raw` | A raw tool definition keyed by API name, which is either `anthropic.messages` or `openai.responses`. The definition is appended to the request as is. |

Keep the following in mind when you use server-side tools:

- Search calls and search sources appear in the reply as `hosted_call` and `hosted_result` blocks. Citations are in `TextBlock.Citations`, and the number of calls is recorded in `Usage.HostedCalls`.
- Server-side calls do **not** go through approval or the `pre_tool_use` hook, because they never come back to your process at all. If you need to control them, you can choose not to register these tools, or use domain filtering.
- The Chat Completions API does not support server-side tools. Requests sent to the Chat API do not carry these tools, and the SDK emits `model.notice{hosted_tool_unsupported}`.

## 4.8 Gateways and custom HTTP

When you access a model through a gateway, you usually need to set the address or request headers, or to process requests in some extra way. The SDK provides a way to configure each of these:

- Set the gateway address with the adapter's `BaseURL`.
- Set fixed request headers, such as authentication or routing tags, with `Params.Headers`.
- Set request headers that must be computed for each request, such as a session id that lets the gateway group requests, with `kernel.WithRequestHeaders(func(kernel.RequestInfo) map[string]string)`. The callback can read `RootSessionID`, `SessionID`, `TurnID`, `Bucket`, `API` and `Model`. `Bucket` takes values such as main, compaction, side and subagent. When a header has the same name as one in `Params.Headers`, the value returned by the callback wins.
- To record response headers, such as a call id returned by the gateway, use `kernel.WithCaptureHeaders("x-litellm-call-id")`. The recorded values are written into each call record and into the reply message's `Producer.Headers`.
- To rewrite the request itself, for example to sign it, change its URL or send it through a proxy, supply your own implementation of `Config.Doer`, and modify the request in `Do` before you forward it.
- Adjust the first-event timeout and the stream idle timeout in `provider.RetryPolicy`; see [18](18-limits.md).

> Note: Do not set an overall timeout on the HTTP client. It would cut off long replies that are otherwise normal.

## 4.9 Best practices

- Write the capabilities of the models you use often into configuration, and declare them all at startup with `WithCapabilities`. Runtime lookups can give you the window and the output limit, but behavioral traits such as `ThinkingPrefixBound` and `NoToolsWithReasoning` can come only from declarations.
- During development, subscribe to `model.notice` and `model.retry` and log them. The former tells you which capabilities need to be declared, and the latter shows how stable the endpoint is.
- After switching models, consider telling the user that the context may have been reorganized. When `DroppedReasoning` in the `provider.switched` event is greater than 0, the history the model sees is missing its earlier reasoning.
- One Provider instance per gateway is enough. Providers are safe for concurrent use, and creating several instances only gives you extra connection pools.
- Pass vendor-specific fields through `Params.Extra`. Do not modify the adapter for them.

---

[← Previous: Core objects](03-core-objects.md) · [Contents](README.md) · [Next: Sending input and waiting for results →](05-input.md)
