[← 上一章：订阅事件](06-events.md) · [目录](README.md) · [下一章：审查 →](08-approval.md)

# 7. 注册工具

工具是模型调用你的代码的途径。本章介绍如何定义工具的输入和输出，如何设置并发方式、超时和后台运行，以及怎样把工具写得既便于模型使用，又能让使用者放心。读完本章，你可以为 agent 注册自己的工具，并控制它们的执行方式。

本章内容：[7.1 定义一个工具](#71-定义一个工具) · [7.2 ToolSpec](#72-toolspec) · [7.3 ToolContext](#73-toolcontext) · [7.4 ToolResult](#74-toolresult) · [7.5 执行顺序与并发](#75-执行顺序与并发) · [7.6 把一次调用转到后台](#76-把一次调用转到后台) · [7.7 动态定义与遮蔽](#77-动态定义与遮蔽) · [7.8 延迟加载与工具检索](#78-延迟加载与工具检索) · [7.9 建议做法](#79-建议做法)

## 7.1 定义一个工具

定义工具最简单的方式是调用 `kernel.NewTool`，传入工具定义 `ToolSpec` 和一个处理函数。处理函数的输入参数是一个带 json tag 的 Go 结构体，SDK 会把模型给出的输入解码到这个结构体中。下面的例子定义了一个订酒店的工具，并分别在 agent 级和 session 级注册：

```go
type bookIn struct {
	Hotel  string `json:"hotel"`
	Nights int    `json:"nights"`
}

book := kernel.NewTool(kernel.ToolSpec{
	Name:        "book_hotel",
	Description: "Book a hotel room.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"hotel":{"type":"string"},"nights":{"type":"integer"}},"required":["hotel","nights"],"additionalProperties":false}`),
	Concurrency: kernel.Serial,
}, func(ctx context.Context, tc *kernel.ToolContext, in bookIn) (kernel.ToolResult, error) {
	return kernel.TextResult(fmt.Sprintf("booked %s for %d nights", in.Hotel, in.Nights)), nil
})

kernel.WithTool(book)                       // 在 agent 级注册
sess.Registrar().Tool(book)                 // 在 session 级注册，可以在运行中进行
```

`NewTool` 会严格解码输入。键名必须与 json tag 完全一致，未知键和重复键都会被拒绝。这样可以保证工具执行时使用的输入，与审查规则和审查者看到的输入完全相同。如果解码失败，SDK 不会调用你的函数，模型会直接收到一个“输入不合法”的结果，其 `Outcome` 为 `invalid_input`。

如果你需要更多控制，也可以自己实现 `kernel.Tool` 接口：

```go
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, tc *ToolContext, input json.RawMessage) (ToolResult, error)
}
```

每轮开始冻结工具定义时，SDK 都会调用一次 `Spec()`。因此，枚举值等内容可以在 `Spec()` 中动态计算，具体做法见 [7.7](#77-动态定义与遮蔽)。

## 7.2 ToolSpec

`ToolSpec` 描述一个工具的名称、说明、输入格式和执行方式。各字段的含义如下：

| 字段 | 说明 |
|---|---|
| `Name` | 工具名，在同一 session 内必须唯一。这个名字会展示给模型，建议使用小写字母加下划线。 |
| `Description` | 展示给模型的说明。应写清楚什么时候该用这个工具，什么时候不该用。 |
| `InputSchema` | 输入的 JSON Schema。建议设置 `"additionalProperties": false`，并在 `required` 中列出全部字段。 |
| `Strict` | 在接口支持时，请求使用严格 schema。 |
| `Concurrency` | 设为 `kernel.Parallel` 时，可以与相邻的 Parallel 调用并行执行，适用于只读工具。设为 `kernel.Serial` 时单独执行并保持顺序，适用于有副作用的工具。 |
| `Timeout` | 为 0 时使用 `Limits.ToolTimeout`，默认 10 分钟。设为 `kernel.NoTimeout`（即任何负数）时不超时，适用于需要等待人回复或等待其他 agent 的工具。即使不超时，打断、停止和关闭仍然会取消它。 |
| `Tags` | 自定义标签，kernel 不解读其内容。 |
| `Background` | 允许模型传入 `background:true`，把这次调用转为后台任务。设置后，工具定义中会自动多出这个参数，但工具本身永远看不到它。详见 [7.6](#76-把一次调用转到后台)。 |
| `Deferred` | 当请求带有工具检索时，按需加载这个工具。详见 [7.8](#78-延迟加载与工具检索)。 |

## 7.3 ToolContext

处理函数的 `tc *kernel.ToolContext` 参数提供了这次调用的上下文信息，以及向外报告进度的方法：

| 方法 | 说明 |
|---|---|
| `CallID()` | 返回这次调用的 id，与事件和历史中的 `call_id` 一致。 |
| `Session()` | 返回调用所在的 session。在工具中可以调用哪些 session 方法，见 [19](19-callback-rules.md)。 |
| `Path()` | 调用来自 subagent 或后台任务时返回非空值，根 session 的直接调用返回空值。 |
| `InputMetadata()` | 返回触发这一轮的输入的 `Metadata`，可用于链路追踪。 |
| `Progress(text, data)` | 发出 `tool.progress` 事件，供 UI 显示进度。只在 `Call` 运行期间有效。 |
| `Emit(payload)` | 发出一条自定义事件，该事件归属于这次调用。 |

## 7.4 ToolResult

`ToolResult` 是工具返回给 SDK 的结果，其中一部分内容会发给模型，另一部分只供宿主程序使用。各字段如下：

| 字段 | 说明 |
|---|---|
| `Content` | 发给模型的内容，只能包含文本、图片和文档块。 |
| `IsError` | 把结果标记为错误，模型会看到这是一个错误结果。 |
| `Structured` | 机器可读的结果，会进入事件（`tool.completed.Structured`）和 hook，**但不会发给模型**。 |
| `Pin` | 压缩之后需要重新注入的内容，包含 `Key`、`Content` 和 `MaxTokens` 三个字段。`MaxTokens` 为 0 时使用默认值 5k。同一个 key 的新内容会覆盖旧内容。 |

SDK 提供两个辅助函数来构造结果：`kernel.TextResult(s)` 和 `kernel.ErrorResult(format, args...)`。

模型最终看到的内容，取决于工具以什么方式结束。下表列出了各种情况：

| 工具的行为 | 模型看到的内容 | 结果的 `Outcome` |
|---|---|---|
| 返回 `ToolResult` | 看到 `Content`，错误标记按 `IsError` 的设置。 | `success` 或 `error` |
| 返回 Go `error` | 看到错误文本，并标记为错误。 | `error` |
| 发生 panic | 看到 `panic: <值>`，不带堆栈，并标记为错误。 | `error` |
| 被打断，包括超过宽限期后被放弃和还没来得及启动的情况 | 看到固定的“已中断”文字，而不是工具自己的错误。 | `interrupted` |
| 超时 | 看到固定的“已超时”文字。 | `error` |
| 输入解码失败 | 看到固定的“输入不合法”文字。 | `invalid_input` |
| 被规则、审查者或 hook 拒绝 | 看到拒绝理由。 | `denied` |
| hook 直接给出了结果 | 看到 hook 给出的内容。 | `synthetic` |
| 进程重启前没有记下结果 | 看到固定的“结果丢失”文字，工具不会重新执行。 | `lost` |

由于返回的 `error` 文本会直接发给模型，错误信息中不要包含密钥、内部地址这类内容。

## 7.5 执行顺序与并发

模型在一次回复中可能给出多个工具调用。SDK 按以下规则安排它们的执行：

- 工具只在模型回复写入历史之后才执行。模型调用失败时只重试模型调用，因此同一个工具调用不会被执行两遍。
- 在一批调用中，连续的 `Parallel` 调用会并行执行，每组最多 `Limits.MaxParallelTools` 个，默认为 8。`Serial` 调用单独执行。各组之间按模型给出的顺序依次执行。
- 并行组中的工具及其 `post_tool_use` hook 是并发调用的，因此它们必须是并发安全的。
- 发生中断时，正在运行的工具的 ctx 会被取消。如果工具在宽限期 `Limits.ToolCancelGrace`（默认 3s）之后仍未返回，就记为放弃并发出 `tool.abandoned` 事件，模型会收到“已中断”的结果。
- 工具收到的 `ctx` 要一路向下传递，因为它携带了超时、打断信号以及 [19](19-callback-rules.md) 中所述的重入检测。

## 7.6 把一次调用转到后台

有些工具运行时间较长，你可能不希望它阻塞当前对话。为此，可以在工具定义中声明 `Background: true`。模型调用这类工具时可以加上 `background: true`，这次调用就会作为一个 `tool_exec` 类型（`kernel.ToolExecKind`）的任务在后台运行。工具调用会立即返回任务 id，工具结束时模型会收到通知，详见 [12](12-tasks.md)。工具本身的代码不需要任何修改。

宿主程序也可以在工具运行期间主动把它转到后台：

```go
info, err := sess.BackgroundCall(ctx, callID) // callID 取自 tool.started 事件中 Movable 为 true 的调用
```

如果找不到可以转移的调用，例如该工具不是 `Background` 工具、调用已经结束或者不在前台运行，`BackgroundCall` 会返回 `ErrNotMovable`。常见的用法是在 UI 发现某个工具已经运行很久时，向用户提供一个“转到后台”按钮。

## 7.7 动态定义与遮蔽

工具的定义和可见范围并不是固定不变的。SDK 支持以下三种调整方式。

**定义会变化的工具。** 如果工具的枚举值等内容来自数据库这类会变化的数据源，你可以自己实现 `Tool` 接口，在 `Spec()` 中计算定义。数据变化后调用 `kernel.ToolChanged(r, name)`，通知 session 从下一轮起使用新的定义。

**运行中增减工具。** `sess.Registrar().Tool(t)` 会返回一个 `*Registration`，调用它的 `Remove()` 即可撤销注册。这类变化从下一轮开始生效，此时 `registry.changed` 事件的 `Effective` 为 `turn`。对于声明了 `ThinkingPrefixBound` 的模型，变化从下一个 epoch 开始生效，`Effective` 为 `epoch`。无论哪种情况，一轮开始时冻结的工具定义在这一轮之内都不会改变。

**只对某个 session 隐藏工具。** 调用 `sess.Registrar().Hide(kernel.KindTool, "book_hotel")` 后，agent 级注册的这个工具在该 session 中将不可见。

**读取当前可见的工具。** `sess.Tools()` 返回当前 session 可见的函数工具定义，按名称排序。session 级注册会遮蔽 agent 级的同名工具，被 `Hide` 的工具不会出现；`Spec()` 发生 panic 的工具会被跳过并记日志。返回的是副本，修改它不会影响注册表，适合在握手时告诉宿主有哪些工具。provider 托管的工具（例如 `web_search`）不在其中，用 `sess.HostedTools()` 读取。

## 7.8 延迟加载与工具检索

当工具数量很多时，把所有工具定义都放进上下文会占用大量 token。这时可以让一部分工具默认不进入上下文，只有在模型检索到它们时才加载。启用方法分两步：

1. 注册中立的工具检索：`kernel.WithHostedTool(message.HostedTool{Name: "tool_search", ToolSearch: &message.ToolSearch{}})`。
2. 把不常用的工具标记为 `Deferred: true`。

目前 Anthropic Messages 和 OpenAI Responses 支持这一功能，Chat Completions 不支持。在 Chat Completions 上，延迟工具会被当作普通工具发送，同时发出 `model.notice{hosted_tool_unsupported}` 事件。如果模型本身不支持检索，可以用 `Capabilities.NoToolSearch` 声明。ext/mcp 的 `Native` 检索模式就是基于这一机制实现的，见 [14.8](14-builtin-extensions.md#148-extmcp)。

## 7.9 建议做法

- 工具描述应当用英文和中性措辞编写。描述是写给模型看的，通常用一句话说明工具做什么，再用一句话说明什么时候该用、什么时候不该用，必要时再说明返回什么。
- 建议尽量收紧输入 schema。设置 `additionalProperties: false`，为每个字段写 `description`，能用枚举的地方使用枚举，这样模型填错参数的概率会明显下降。
- 建议把只读工具标为 `Parallel`，把有副作用的工具标为 `Serial`。例如模型一次给出“查天气、查汇率、订酒店”三个调用时，前两个会并行执行，第三个会等它们完成后单独执行。
- 不要让工具结果过大。工具结果会一直留在上下文中，直到发生压缩，因此几十 KB 的输出应先在工具内裁剪或分页，把大部分数据放进 `Structured` 交给 UI。
- 必须保留的结论用 `Pin` 保存。例如把“当前工作目录”或“用户选定的方案”放进 Pin，压缩之后模型仍然能看到它们。
- 运行时间较长的工具应当报告进度。可以用 `Progress` 发送进度，也可以标记 `Background`，让模型自己决定是否等待。
- 工具应当具备幂等性。模型偶尔会重复调用同一个工具，因此带外部副作用的工具（例如下单、发邮件）应在输入中要求一个幂等键，或者在审查规则中把它标为 `ask`。
- 错误信息是写给模型看的，应说明哪里不对以及如何修改，例如“nights must be between 1 and 30”，模型会据此修正后重试。不要只返回堆栈。
- 返回图片结果之前，先确认模型支持图片，即 `NoImagesInToolResult` 为 false。模型不支持时，图片会被去掉。

---

[← 上一章：订阅事件](06-events.md) · [目录](README.md) · [下一章：审查 →](08-approval.md)
