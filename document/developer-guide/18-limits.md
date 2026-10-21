[← 上一章：用量、调用记录与费用](17-usage-cost.md) · [目录](README.md) · [下一章：回调中的调用限制 →](19-callback-rules.md)

# 18. 限制与默认值

本章列出 SDK 中所有可以调整的上限和超时，以及它们的默认值。当你遇到 agent 意外停止或者等待时间过长的情况时，建议先查阅本章。

本章内容：[18.1 Limits](#181-limits) · [18.2 重试策略](#182-重试策略) · [18.3 上下文策略](#183-上下文策略) · [18.4 其他 agent 级选项](#184-其他-agent-级选项) · [18.5 建议做法](#185-建议做法)

所有默认值都来自 `spec/data/limits.json`。字段为零值时取默认值，为负值时表示不限制。`MaxTruncationContinues` 和 `ReadAhead` 是例外，它们为负值时表示关闭对应的功能。

## 18.1 Limits

`kernel.Limits` 汇总了 agent 运行过程中的各项上限和超时，通过 `kernel.WithLimits` 设置：

```go
kernel.WithLimits(kernel.Limits{MaxSteps: 50, ToolTimeout: 2 * time.Minute})
```

| 字段 | 默认值 | 说明 |
|---|---|---|
| `MaxSteps` | 不限 | 一轮中最多调用模型的次数。达到上限时，这一轮以 `max_steps` 结束。在没有人值守的场景（后台任务、定时运行）中，建议设置一个上限，避免模型陷入循环时持续运行。 |
| `ToolTimeout` | 10 分钟 | 单个工具调用的超时。可以用 `ToolSpec.Timeout` 为每个工具单独覆盖。 |
| `ToolCancelGrace` | 3s | 打断后等待工具退出的宽限时间。超过这个时间仍未退出的工具记为放弃。 |
| `MaxParallelTools` | 8 | 一个并行组中同时运行的工具数。 |
| `MaxConsecutiveAutoTurns` | 8 | 只由任务通知和提醒触发的连续轮次上限，异步 hook 注入的内容也计算在内。达到上限后，agent 会等待用户输入。 |
| `MaxStopContinuations` | 8 | 一轮内 stop hook 要求继续运行的最大次数。 |
| `MaxTruncationContinues` | 1 | 回复被截断后自动续写的次数。需要同时开启 `WithAutoContinueTruncatedText`。 |
| `MaxQueuedInputs` | 256 | 输入队列的容量。队列满时，`Send` 返回 `ErrQueueFull`。 |
| `MaxRunningTasks` | 8 | 一个 session 中尚未结束的任务数上限，前台任务和后台任务都计算在内。 |
| `MaxToolResultChars` | 100000 | 单个工具结果交给模型的文本字符数上限。超过时保留开头和结尾各一半，中间换成一段说明，`tool.completed` 的 `OriginalChars` 给出原始长度。负值表示不限。 |
| `MaxAsyncHooks` | 64 | 同时运行的异步 hook 数上限。 |
| `HookGrace` | 1s | 同步 hook 超时或被取消后，继续等待它返回的宽限时间。超过这个时间后放弃等待。 |
| `ReadAhead` | 64 | 从模型流中预读的事件数，使较慢的事件消费者不会拖慢网络读取。 |
| `InitTimeout` | 30s | session 初始化的超时，初始化包括渲染提示词、安装扩展和运行 `session_start` hook。 |
| `CloseTimeout` | 10s | `Close` 等待 worker 退出的时间。 |

## 18.2 重试策略

模型调用的重试行为由 `provider.RetryPolicy` 控制，通过 `kernel.WithRetryPolicy` 设置：

```go
kernel.WithRetryPolicy(provider.RetryPolicy{MaxAttempts: 6, FirstEventTimeout: 10 * time.Minute})
```

| 字段 | 默认值 | 说明 |
|---|---|---|
| `MaxAttempts` | 4 | 一次模型调用最多尝试的次数。 |
| `BaseDelay`、`MaxDelay` | 1s、30s | 退避等待的起始值和上限，实际等待时间带有随机抖动。如果接口返回了 `Retry-After`，则按它的要求等待。 |
| `MaxElapsed` | 3 分钟 | 所有重试加起来的总时长上限。 |
| `FirstEventTimeout` | 2 分钟 | 从发出请求到收到第一个字节的最长等待，建立连接和等待响应头的时间都计算在内。推理模型的思考内容通常在几秒到十几秒内开始输出。设为负数表示不限制。 |
| `IdleTimeout` | 2 分钟 | 流中相邻两帧之间的最长间隔，注释行也算作一帧。设为负数表示不限制。 |

只有可重试的错误才会触发重试，包括限流、过载、临时故障和流中断。每次重试都会发出 `model.retry` 事件。鉴权失败、参数错误等不可重试的错误会直接导致调用失败。错误的分类见 [20.3](20-errors.md#203-模型调用错误)。

## 18.3 上下文策略

上下文策略通过 `kernel.WithContextPolicy` 设置，它的字段和默认值见 [15.1](15-context.md#151-自动压缩)。如果没有声明模型能力，SDK 按 200k 的上下文窗口和 64k 的最大输出计算。

## 18.4 其他 agent 级选项

以下选项在创建 agent 时设置，作用于整个 agent：

| 选项 | 作用 |
|---|---|
| `WithAutoContinueTruncatedText(true)` | 纯文本回复因输出上限被截断时，自动续写一次。默认关闭。 |
| `WithLogger(l)` | 设置一个 `*slog.Logger`，用于输出 kernel 和扩展的告警。 |
| `WithClock(c)`、`WithIDGenerator(g)` | 固定时间和 id，供测试使用，见 [21](21-testing.md)。 |
| `WithTokenCounter(tc)` | 在接近压缩阈值时用它精确计算 token 数。 |
| `WithRawProviderEvents()` | 发出 `provider.raw` 事件，其中是接口的原始流，供调试使用。 |
| `WithPanicOnInternalError(true)` | kernel 内部出现 bug 时直接 panic，而不是关闭 session。建议在测试时开启。 |
| `WithStorePolicy(p)` | 设置存储写入失败时的处理方式，见 [16.1](16-storage.md#161-选择存储)。 |
| `WithInputMode(m)` | 设置默认的输入模式，见 [5.4](05-input.md#54-agent-正忙时的三种模式)。 |

## 18.5 建议做法

- 建议通过 `ToolSpec.Timeout` 为每个工具单独设置超时。全局的 `ToolTimeout` 只用来应对没有预料到的情况。
- 以下两种情况应调大 `FirstEventTimeout`：输入很长且没有命中缓存，prefill 可能需要一两分钟；或者模型在答案生成之前不发送任何内容。否则请求会被判定为超时并重试，每次重试都要重新支付 prefill 的费用。
- 不要把 `MaxSteps` 设得太小。模型每发起一批工具调用就是一步，复杂任务用到二三十步是很正常的。如果你要限制的是时间和费用，应当使用工具超时和用量事件。
- 在生产环境中，`CloseTimeout` 应设置得足够长，至少能让最慢的工具退出，否则 `Close` 会返回 `LeakError`。
- `MaxConsecutiveAutoTurns` 用于防止 agent 失控。当后台任务不断发出通知、模型不断回应时，agent 会在 8 轮之后停下来等待用户输入。

---

[← 上一章：用量、调用记录与费用](17-usage-cost.md) · [目录](README.md) · [下一章：回调中的调用限制 →](19-callback-rules.md)
