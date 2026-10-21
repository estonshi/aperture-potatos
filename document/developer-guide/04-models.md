[← 上一章：核心对象](03-core-objects.md) · [目录](README.md) · [下一章：发送输入与等待结果 →](05-input.md)

# 4. 接入模型

本章介绍如何让 agent 连接到一个模型，内容包括选择适配器、设置请求参数、声明模型能力，以及在运行中切换模型。当你需要接入新的模型或网关，或者发现模型的行为不符合预期时，可以参考本章。

本章内容：[4.1 三种适配器](#41-三种适配器) · [4.2 选择适配器](#42-选择适配器) · [4.3 模型引用与参数](#43-模型引用与参数) · [4.4 模型能力](#44-模型能力) · [4.5 思维链](#45-思维链) · [4.6 运行中切换模型](#46-运行中切换模型) · [4.7 服务端工具](#47-服务端工具) · [4.8 网关与自定义 HTTP](#48-网关与自定义-http) · [4.9 建议做法](#49-建议做法)

## 4.1 三种适配器

SDK 通过适配器与模型接口通信。目前提供三种适配器，分别对应 Anthropic Messages、OpenAI Responses 和 OpenAI Chat Completions 三种接口。创建适配器时只需要提供密钥，未指定地址时使用各自官方的默认地址：

```go
claude := anthropic.New(anthropic.Config{APIKey: key})             // 默认 https://api.anthropic.com
gpt    := openairesponses.New(openairesponses.Config{APIKey: key}) // 默认 https://api.openai.com/v1
chat   := openaichat.New(openaichat.Config{APIKey: key})           // 默认 https://api.openai.com/v1
```

三种适配器都可以通过 `BaseURL` 指向网关或兼容服务。例如，连接一个兼容 Chat Completions 的服务：

```go
chat := openaichat.New(openaichat.Config{APIKey: key, BaseURL: os.Getenv("CHAT_BASE_URL")})
```

`Config` 包含以下字段：

| 字段 | 说明 |
|---|---|
| `APIKey` | 访问接口使用的密钥。 |
| `BaseURL` | 网关或兼容服务的地址。Chat 与 Responses 的地址需要包含版本路径，例如 `/v1`，Anthropic 的地址不需要。 |
| `Doer` | 自定义的 HTTP 客户端，任何实现了 `Do(*http.Request) (*http.Response, error)` 的值都可以使用。为 `nil` 时使用 `provider.DefaultClient`，它只跟随同一主机内的重定向，不会把 API key 和请求体带到别的主机。流的超时由适配器自行管理，因此客户端不需要设置超时。 |
| `Betas` | 仅 Anthropic 适配器支持。每个请求都会携带这些 beta 头，适配器还会为某些功能自动追加所需的 beta 头。 |

每个适配器都实现了 `provider.Provider` 接口，即 `API()`、`Endpoint()` 和 `Stream()` 三个方法。适配器是并发安全的，一个实例可以供多个 session 共用。Anthropic 适配器还实现了 `CountTokens`，用于在上下文接近压缩阈值时进行精确计数。

## 4.2 选择适配器

应该使用哪种适配器，取决于你要连接的模型和服务。下表列出了常见的对应关系：

| 要连接的服务 | 使用的适配器 |
|---|---|
| Anthropic 官方，或经网关访问 Bedrock、Vertex 上的 Claude | 使用 `anthropic`。 |
| OpenAI 自家的新模型 | 使用 `openairesponses`。Chat Completions 接口在这些模型上不能同时使用推理和工具。 |
| DeepSeek、Qwen、Kimi、vLLM、Ollama、OpenRouter 等 OpenAI 兼容服务 | 使用 `openaichat`。 |
| LiteLLM 网关 | 按网关背后的模型选择。Claude 使用 `anthropic`，其他模型使用 `openaichat` 或 `openairesponses`，`BaseURL` 都指向网关。 |

## 4.3 模型引用与参数

`provider.ModelRef` 描述一次模型调用使用哪个适配器、哪个模型名以及哪些参数。你可以把它传给 `kernel.WithModel` 来设置 agent 的模型，也可以传给 `sess.SwitchModel` 在运行中切换模型：

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

`provider.Params` 包含以下字段：

| 字段 | 说明 |
|---|---|
| `MaxOutputTokens` | 为 0 时使用默认值 64,000。如果已知模型的输出上限，值会被压到上限以内。如果只知道窗口大小，则压到窗口的四分之一。 |
| `Temperature`、`TopP` | 类型为 `*float64`，为 nil 时不发送。 |
| `StopSequences` | 停止序列。 |
| `Reasoning.Mode` | 可选 `auto`（默认）、`on` 和 `off`，详见 [4.5](#45-思维链)。 |
| `Reasoning.Effort` | 可选 `none`、`minimal`、`low`、`medium`、`high`、`xhigh` 和 `max`。如果声明了模型支持的档位，会映射到最接近的档位。 |
| `Reasoning.Budget` | 对应 Anthropic 的手动 `budget_tokens`。Chat 接口没有通用的映射方式，厂商自己的预算字段请放进 `Extra`。 |
| `Reasoning.Hide` | 为 true 时只开启推理，不获取可读的思维链文字。 |
| `Headers` | 额外的请求头，类型为 `http.Header`。 |
| `Extra` | 最后合并进请求体的字段，用于传递厂商开关或新字段，例如 `{"enable_thinking": true}`。 |

对于声明了 `NoSampling` 的模型，SDK 会去掉采样参数。如果需要在单个 step 中临时修改参数，可以使用 `pre_model_request` hook 的 `ParamsPatch`，它还可以设置 `ToolChoice`，详见 [9.4](09-hooks.md#94-每个点的输入与输出)。

## 4.4 模型能力

SDK 不内置模型表。窗口大小、输出上限、是否支持采样参数等信息需要由你声明，或者在运行时查询。SDK 不会根据未知的能力改写请求，因此在没有声明的情况下，请求会按你设置的内容原样发送。

使用 `kernel.WithCapabilities` 声明能力：

```go
kernel.WithCapabilities(provider.APIAnthropicMessages, "claude-opus-5-5*", provider.Capabilities{
	ContextWindow:       1000000,
	ThinkingPrefixBound: true, // 如果修改前文（包括工具定义）会让思考签名失效，就需要声明此项
	Reasoning:           provider.ReasoningCaps{DefaultOn: true, Modes: []string{"adaptive"}},
})
```

第二个参数是模型名的 glob 模式，其中 `*` 匹配任意字符。同一个模型可以多次声明，后声明的值会逐字段覆盖先声明的值。

能力的来源有三种，优先级从高到低依次是 `WithCapabilities` 声明、运行时查询和默认值。默认值为窗口 200k、输出 64k。三种适配器都会到端点查询能力，其中 Anthropic 查询 Models API，另外两种查询 `GET {base}/models`，包括 LiteLLM 返回的 `model_info`。kernel 默认使用各个 Provider 自带的查询，不需要额外配置。

如果你想统一替换查询来源，例如改为查询你自己的模型表，可以使用 `kernel.WithCapabilityResolver(r)`。它会替换这个 agent 下所有 Provider 的查询。传入 `nil` 则关闭运行时查询。

`provider.Capabilities` 包含以下字段：

| 字段 | 含义 |
|---|---|
| `Family` | 模型家族名。切换模型时用它判断历史能否原样回传，不填时按模型名判断。 |
| `ContextWindow`、`MaxOutputTokens` | 上下文窗口大小与输出上限。 |
| `NoTools` | 模型不支持工具。 |
| `NoParallelTools` | 模型一次只会发起一个工具调用。 |
| `NoWebSearch`、`NoToolSearch` | 不发送中立的 web 搜索或工具检索。 |
| `NoToolsWithReasoning` | 有工具时必须关闭推理，OpenAI Chat 上的部分 GPT 模型属于这种情况。 |
| `NoSampling` | 去掉 temperature 和 top_p。 |
| `SamplingWithThinking` | 开启推理时仍可发送哪些采样参数，可选 `all`、`top_p_only` 和 `none`。 |
| `NoForcedToolChoice`、`NoToolChoiceNone` | 分别表示不支持强制调用工具，以及不支持 `tool_choice: none`。 |
| `NoImagesInToolResult` | 工具结果中不能包含图片。 |
| `NoMidConversationSystem` | 不能在对话中间插入 system 消息。 |
| `ThinkingPrefixBound` | 修改前文会让思考签名失效。声明后工具定义按 epoch 固定，切换到这种模型时会开启新的 epoch。 |
| `Reasoning` | 类型为 `ReasoningCaps`，包含 `Visible`（可选 `none`、`summary`、`raw`）、`Modes`、`DefaultOn`、`NoDisable`、`Efforts`、`BudgetMin` 和 `InterleavedBeta`。 |
| `PromptCache` | 类型为 `CacheCaps`，包含 `Explicit`、`MaxBreakpoints` 和 `MinPrefixTokens`。其中 `Explicit` 表示需要显式设置缓存断点。 |
| `SystemRole`、`MaxTokensField` | 仅用于 Chat 接口，分别表示 system 消息使用的 role，以及输出上限字段的名称。 |

如果接口因为输出上限过大或不认识某个字段名而报错，SDK 会根据报错调整请求并重试一次，同时发出 `model.notice` 事件，提示你补充声明。

`model.notice` 的 `Kind` 有以下取值：`capability_adjusted` 表示 SDK 根据能力修改了请求，`capability_lookup_failed` 表示运行时查询失败，`capability_hint` 表示建议你声明某项能力。其余取值包括 `fallback`、`reasoning_summary_disabled`、`reasoning_replay_fallback`、`thinking_dropped`、`reasoning_exhausted`、`sequence_regression`、`dangling_tool_call_healed`、`empty_reply_continue`、`hosted_tool_unsupported` 和 `hosted_tool_adjusted`。建议在开发阶段把这些事件记录到日志中。

## 4.5 思维链

思维链是模型在推理过程中产生的可读内容。推理是否开启由 `Reasoning.Mode` 控制。设为 `auto` 时，SDK 只在已知默认开启推理的模型上获取可读思维链，判断依据是 `ReasoningCaps.DefaultOn`。设为 `on` 表示明确要求推理，设为 `off` 表示明确关闭推理。

当能够获取思维链时，SDK 会以 `reasoning.started`、`reasoning.delta` 和 `reasoning.completed` 事件发出。事件中的 `Kind` 说明内容的性质，取值如下：

- `summary` 表示厂商提供的摘要。
- `raw` 表示思维链原文。
- `progress` 表示工具调用之间的进度说明。
- `omitted`、`redacted` 和 `encrypted` 表示只有签名或密文，没有文字内容。

Chat 接口会自动读取 `reasoning_content`、`reasoning`、`thinking_blocks` 和 `reasoning_details` 这些字段，不需要按厂商分别配置。用于开关思考的厂商参数请放进 `Params.Extra`。

思考签名、`encrypted_content` 等私有字段会原样保存，但只在回传给同一种接口、同一个模型家族时才会带上，Chat 接口还要求是同一个端点。如果换了接口或模型家族，这些字段就不再回传。模型仍然可以正常工作，只是看不到之前的思考过程。

推理设置的修改在下一轮生效，同一轮之内不会混用两种设置。

## 4.6 运行中切换模型

session 运行过程中可以通过 `sess.SwitchModel` 切换到另一个模型，新模型可以使用不同的适配器：

```go
err := sess.SwitchModel(ctx, provider.ModelRef{Provider: gpt, Model: "gpt-5.6-sol"})
```

切换在两轮之间生效，历史会按新接口的格式重新整理后发送。`ctx` 只用于约束尚未缓存的能力查询，能力已经查询过时，调用会立即返回。

切换的过程如下：

1. 在下一轮开始、切换真正生效之前，SDK 先触发 `provider_switch` hook，hook 可以阻断这次切换。hook 的输入包含 `DroppedReasoning` 和 `DroppedOpaque`，分别表示有多少思维链和厂商私有块无法发送给新模型。
2. 如果当前上下文放不进新模型的窗口，SDK 会以 `switch` 作为触发原因先压缩一次，前提是已经安装了压缩策略。如果压缩后仍然放不下，SDK 会拒绝切换并发出 `provider.switch_rejected{reason:"capacity"}`，这一轮继续使用旧模型。
3. 切换成功时发出 `provider.switched` 事件，其中 `CacheReset` 表示是否开启了新的 epoch。如果被 hook 阻断，则发出 `provider.switch_rejected{reason:"hook_blocked"}`。

切换到声明了 `ThinkingPrefixBound` 的模型时，会开启新的 epoch。系统提示词原样保留，工具定义则按新模型重新固定。

> 注意：有些接口对历史格式要求更严格。例如，带工具的 DeepSeek 要求每条助手消息都包含推理字段。如果切换过去之后请求被拒绝，请先执行 `/compact` 或 `/clear` 再重试。

## 4.7 服务端工具

服务端工具是由模型厂商在服务端执行的工具，目前包括 web 搜索和工具检索。这类工具通过 `WithHostedTool` 注册，支持 Anthropic Messages 和 OpenAI Responses 两种接口：

```go
kernel.WithHostedTool(message.HostedTool{
	Name:      "web_search",
	WebSearch: &message.WebSearch{MaxUses: 5, AllowedDomains: []string{"docs.python.org"}},
})
```

`message.HostedTool` 的以下三个字段只能设置其中一个：

| 字段 | 说明 |
|---|---|
| `WebSearch` | 中立的 web 搜索写法，适配器会把它映射成各家的原生工具，`Name` 必须是 `web_search`。可用的选项有 `MaxUses`、`AllowedDomains` 或 `BlockedDomains`（两者只能选一个）、`UserLocation` 和 `ContextSize`。`UserLocation` 包括城市、地区、国家代码和时区，`ContextSize` 可选 `low`、`medium` 和 `high`。 |
| `ToolSearch` | 中立的工具检索，`Name` 必须是 `tool_search`。注册后，`ToolSpec.Deferred` 为 true 的工具不会进入上下文，只有在模型检索到时才加载，详见 [7.8](07-tools.md#78-延迟加载与工具检索)。 |
| `Raw` | 按接口名提供原样的工具定义，接口名为 `anthropic.messages` 或 `openai.responses`，定义会直接追加进请求。 |

使用服务端工具时，请注意以下几点：

- 搜索调用和搜索来源在回复中以 `hosted_call` 和 `hosted_result` 块表示，引用信息在 `TextBlock.Citations` 中，调用次数记录在 `Usage.HostedCalls` 中。
- 服务端调用**不经过**审查和 `pre_tool_use` hook，因为这些调用根本不会回到你的进程。如果需要管控，可以选择不注册这类工具，或者使用域名过滤。
- Chat Completions 接口不支持服务端工具。请求发往 Chat 接口时不会携带这些工具，同时 SDK 会发出 `model.notice{hosted_tool_unsupported}`。

## 4.8 网关与自定义 HTTP

通过网关访问模型时，通常需要设置地址、请求头，或者对请求做额外处理。SDK 为不同的需求提供了相应的配置方式：

- 网关地址通过适配器的 `BaseURL` 设置。
- 固定的请求头，例如鉴权信息或路由标签，通过 `Params.Headers` 设置。
- 需要针对每个请求动态计算的请求头，例如带上 session id 让网关归组，通过 `kernel.WithRequestHeaders(func(kernel.RequestInfo) map[string]string)` 设置。回调可以读取 `RootSessionID`、`SessionID`、`TurnID`、`Bucket`、`API` 和 `Model`，其中 `Bucket` 的取值包括 main、compaction、side、subagent 等。与 `Params.Headers` 同名时，以回调返回的值为准。
- 如果需要记录响应头，例如网关返回的调用 id，可以使用 `kernel.WithCaptureHeaders("x-litellm-call-id")`。记录的值会写进每次调用的记录，以及回复消息的 `Producer.Headers`。
- 如果需要改写请求本身，例如签名、更换 URL 或走代理，可以为 `Config.Doer` 提供你自己的实现，在 `Do` 中修改请求后再转发。
- 首字超时和流的空闲超时在 `provider.RetryPolicy` 中调整，详见 [18](18-limits.md)。

> 注意：不要在 HTTP 客户端上设置总超时，否则会切断正常的长回复。

## 4.9 建议做法

- 建议把常用模型的能力写成配置，在启动时一次性通过 `WithCapabilities` 声明。运行时查询可以获得窗口和输出上限，但 `ThinkingPrefixBound`、`NoToolsWithReasoning` 这类行为特征只能通过声明提供。
- 开发阶段应订阅 `model.notice` 和 `model.retry` 并记录日志。前者告诉你哪项能力需要声明，后者反映端点的稳定性。
- 切换模型后，建议提示用户上下文可能已被整理。`provider.switched` 事件中的 `DroppedReasoning` 大于 0 时，模型看到的历史中缺少了之前的思考过程。
- 同一个网关前只使用一个 Provider 实例即可。Provider 是并发安全的，创建多个实例只会多出几份连接池。
- 厂商特有的字段通过 `Params.Extra` 传递，不要为此修改适配器。

---

[← 上一章：核心对象](03-core-objects.md) · [目录](README.md) · [下一章：发送输入与等待结果 →](05-input.md)
