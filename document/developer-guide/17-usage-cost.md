[← 上一章：存储与恢复](16-storage.md) · [目录](README.md) · [下一章：限制与默认值 →](18-limits.md)

# 17. 用量、调用记录与费用

本章介绍如何查询每一轮对话消耗的 token、实际发出的模型请求以及产生的费用，并说明如何让这些记录与网关日志一一对应。如果你要实现计费、配额控制或成本看板，可以从本章入手。

本章内容：[17.1 用量](#171-用量) · [17.2 调用记录](#172-调用记录) · [17.3 为请求添加标记](#173-为请求添加标记) · [17.4 查询费用](#174-查询费用) · [17.5 建议做法](#175-建议做法)

## 17.1 用量

`message.Usage` 描述一次模型调用的用量，包含以下字段：

| 字段 | 说明 |
|---|---|
| `InputTokens` | 整个输入的 token 数，**包括**命中缓存的部分。 |
| `CacheReadTokens`、`CacheWriteTokens` | 读取缓存和写入缓存的 token 数。 |
| `OutputTokens` | 输出的 token 数，其中包括推理 token。 |
| `ReasoningTokens` | 输出中推理部分所占的 token 数。 |
| `HostedCalls` | 服务端工具（例如 web 搜索）被调用的次数，按工具名分别计数。 |
| `Estimated` | 为 true 时表示用量是估算值，出现在流中断或接口没有报告用量的情况下。 |
| `Raw` | 接口报告的原始用量 JSON。有些网关会在其中附带费用、速度等信息。 |

你可以从三个地方获取用量。`usage.updated` 事件在每次模型调用后发出，携带 `Bucket`、`StepUsage`、`SessionTotal`、`Context` 和 `Forwarded` 字段，其中 `Context` 表示上下文窗口的占用情况，`Forwarded` 表示这份用量来自子 session。`TurnResult.Usage` 是这一轮的合计用量。`sess.Snapshot().Usage` 返回一个 `UsageReport`，表示 session 的累计用量。

`UsageReport` 包含以下内容：

- `Total` 是总用量。
- `Buckets` 按用途分别统计，用途包括 main、compaction、side、subagent、peer 等。
- `ByModel` 按 `api/model` 分别统计。
- `LastStep` 是最近一步的用量。
- `Turns`、`Steps` 和 `Retries` 分别是轮次数、步数和重试次数。
- `Discards` 是被丢弃的回复数，`Abandons` 是被放弃的工具数。

`UsageReport` 只增不减，与每一轮的用量数据相互独立。

## 17.2 调用记录

kernel 为每一次 HTTP 请求记录一条 `ModelCall`。被重试替代的请求同样会被记录，因此调用记录可以与网关日志逐条对应。`ModelCall` 的字段如下：

| 字段 | 说明 |
|---|---|
| `Seq` | 这条记录在账本中的序号。 |
| `TurnID`、`SessionID`、`RootSession`、`MessageID` | 这条调用的归属。`RootSession` 是最顶层的 session，subagent 发出的调用也归在它下面。 |
| `Bucket` | 这次调用的用途。 |
| `API`、`Endpoint`、`Model` | 请求发往的接口、端点和模型。 |
| `ResponseID`、`RequestID` | `ResponseID` 是接口返回的响应 id，LiteLLM 等网关的费用日志按它检索。`RequestID` 是请求 id，联系厂商支持时使用。 |
| `Headers` | 由 `WithCaptureHeaders` 指定采集的响应头。 |
| `Outcome` | 调用结果。`committed` 表示回复已写入历史，`discarded` 表示回复被丢弃，`failed` 表示调用失败。 |
| `Usage`、`StartedAt` | 这次调用的用量和开始时间。 |
| `Step`、`Attempt` | 主循环中的第几步和第几次尝试。`Attempt` 大于 1 表示这是一次重试。 |
| `ParentSeq` | 只用于旁路对话的调用，表示这次请求带上的主会话内容截止到主会话的哪一行。 |

使用 `TurnCalls` 查询某一轮的调用记录：

```go
calls, err := sess.TurnCalls(turnID)
```

这个方法不会调用模型。最近 100 轮的记录直接从内存返回，更早的记录从 `CallStore` 读取（见 [16.4](16-storage.md#164-实现自定义存储后端)）。如果没有配置 `CallStore`，查询更早的轮次会返回 `ErrTurnNotFound`。返回结果包含这一轮的全部请求，压缩摘要的请求、这一轮调起的 subagent 以及进程内 peer 的请求都在其中。如果 `TurnCalls.Pending` 为 true，说明这一轮或它启动的任务仍在运行，你可以稍后再次查询。

如果你在 SDK 之外替本 session 发出了请求，例如自己直接调用了模型，可以用 `sess.RecordCalls(bucket, turnID, calls)` 把这些调用记入账本，它们的用量也会计入统计。

## 17.3 为请求添加标记

以下两个选项用于给模型请求附加请求头，以及从响应中采集指定的响应头，便于在网关或厂商一侧关联请求：

| 选项 | 作用 |
|---|---|
| `kernel.WithRequestHeaders(fn)` | 每个模型请求发出前调用 `fn(RequestInfo)`，并把返回的请求头加到请求上。`RequestInfo` 包含 `RootSessionID`、`SessionID`、`TurnID`、`Bucket`、`API` 和 `Model`。压缩、side call 和 subagent 的请求也会经过这个函数。 |
| `kernel.WithCaptureHeaders(names...)` | 把指定名称的响应头记录到 `ModelCall.Headers` 和回复消息的 `Producer.Headers` 中。 |

## 17.4 查询费用

配置费用来源之后，可以调用 `sess.TurnCost(ctx, turnID)` 查询这一轮每条请求的费用并求和。如果没有配置费用来源，这个方法返回 `ErrNoCostSource`。

```go
cost, err := sess.TurnCost(ctx, res.TurnID)
// cost.Total 是总额（Amount、Currency），cost.Priced 是每条请求的 ResponseID 和 Cost，cost.Missing 是直到 ctx 结束仍未查到费用的响应 id
```

网关的费用日志通常是异步写入的。对于暂时查不到费用的请求，`TurnCost` 会按退避策略反复重试，直到 ctx 结束。因此建议为 `ctx` 设置一个合理的超时，并在返回后检查 `Missing`。

通过 LiteLLM 网关调用模型时，可以这样配置：

```go
lo := litellm.Options{
	BaseURL:       os.Getenv("LITELLM_URL"),
	APIKey:        os.Getenv("LITELLM_MASTER_KEY"), // 需要使用有权读取 spend logs 的 key
	SessionHeader: "x-litellm-session-id",          // 留空时不给请求打标记，费用只能逐条查询
}

kernel.WithCaptureHeaders("x-litellm-call-id"),
kernel.WithRequestHeaders(litellm.RequestHeaders(lo)),
kernel.WithCostSource(litellm.CostSource(lo)),
```

如果要对接其他计费系统，请实现 `kernel.CostSource` 接口：

```go
type CostSource interface {
	// 按调用在切片中的下标返回费用。暂时查不到的调用可以先不返回，TurnCost 会再次询问
	Price(ctx context.Context, calls []ModelCall) (map[int]Cost, error)
}
```

当计费后端返回 HTTP 错误时，`Price` 应当返回 `*kernel.CostError{Status, Message}`。`TurnCost` 对状态码 429 和 5xx 会重试，对其他状态码则放弃查询。

## 17.5 建议做法

- 实现配额控制时，可以直接使用 `usage.updated` 事件中的 `SessionTotal`。这个事件是实时发出的，不需要轮询。
- 为租户计费时，按 `ModelCall.RootSession` 汇总即可。subagent、旁路对话和压缩产生的开销都会归到根 session 下，你还可以再用 `Bucket` 区分对话本身的开销和辅助开销。
- 每次调用的 `ResponseID` 应当保存下来。对账时，它是与网关和厂商后台记录对应的唯一键。
- 建议在轮次结束之后异步查询费用，并给 ctx 留出几十秒的时间。如果 `Missing` 不为空，可以稍后再查一次。
- 注意重试同样产生费用。`Outcome` 为 `failed` 或 `discarded` 的调用，厂商一样会收费。

---

[← 上一章：存储与恢复](16-storage.md) · [目录](README.md) · [下一章：限制与默认值 →](18-limits.md)
