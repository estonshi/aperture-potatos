[← 上一章：发送输入与等待结果](05-input.md) · [目录](README.md) · [下一章：注册工具 →](07-tools.md)

# 6. 订阅事件

agent 运行过程中发生的一切都会以事件的形式发出，包括流式文本、思维链、工具调用、任务进度和用量等。本章介绍事件的结构、订阅方式、背压行为和完整的事件清单。读完本章，你可以据此构建 UI、记录日志或实现监控。

本章内容：[6.1 事件结构](#61-事件结构) · [6.2 订阅方式](#62-订阅方式) · [6.3 背压](#63-背压) · [6.4 事件的归属](#64-事件的归属) · [6.5 事件清单](#65-事件清单) · [6.6 UI 渲染规则](#66-ui-渲染规则) · [6.7 跨进程传递](#67-跨进程传递) · [6.8 发送自定义事件](#68-发送自定义事件) · [6.9 建议做法](#69-建议做法)

## 6.1 事件结构

每个事件都是一个 `event.Event`。它是一个信封，携带类型、序号、归属等公共信息，具体内容放在 `Data` 字段中。`event.Event` 包含以下字段：

| 字段 | 含义 |
|---|---|
| `Type` | `domain.action` 形式的字符串，例如 `message.delta`。 |
| `Seq` | 在同一个根 session 内从 1 开始单调递增。不同 session 之间的序号没有顺序关系。 |
| `Time` | 事件的发出时间。 |
| `SessionID`、`Agent` | 发出事件的 session，以及 agent 的昵称。 |
| `TurnID`、`Step`、`Attempt` | 事件所属的轮次、第几步和第几次重试。不在 turn 中的事件这几个字段为空。 |
| `Path` | 嵌套路径，为空表示是本 session 自己的事件，详见 [6.4](#64-事件的归属)。 |
| `Data` | 事件的负载，是 `event` 包中某个结构体的指针，需要通过类型断言获取。 |

整个 `Event` 可以直接用 `json.Marshal` 序列化，格式与 `spec/schema/event.schema.json` 一致。

## 6.2 订阅方式

SDK 提供了几种订阅事件的方式。你可以在创建 session 时挂上 channel，也可以之后再订阅 channel、注册回调，或者在当前 goroutine 中迭代事件：

```go
// 1. 创建 session 时就挂上 channel，保证能收到 session.started
events := make(chan event.Event, 64)
sess, _ := ag.NewSession(ctx, kernel.WithSessionSink(events, kernel.WithCloseOnDone()))

// 2. 创建之后再订阅 channel
sub, _ := sess.Subscribe(ch, kernel.WithTypes("message.", "tool."))

// 3. 回调在专用 goroutine 上按顺序调用，返回 error 即结束订阅
sub, _ = sess.SubscribeFunc(func(ctx context.Context, ev event.Event) error { ...; return nil })

// 4. 在当前 goroutine 上迭代，break 即取消订阅
for ev, err := range sess.Events(ctx) { ... }
```

`ag.Subscribe` 和 `ag.SubscribeFunc` 订阅这个 agent 的全部 session，包括已有的和之后新建的。`kernel.WithEventSink` 在创建 agent 时就挂上，同样可以保证收到每个 session 的 `session.started`。

订阅时可以使用以下选项：

| 选项 | 作用 |
|---|---|
| `WithTypes(prefixes...)` | 只接收类型以这些前缀开头的事件，例如 `"message."`、`"tool."`、`"turn."`。 |
| `WithDropDeltas()` | 不接收增量事件，只接收完整的终结事件。增量事件包括 `message.delta`、`reasoning.delta`、`tool_call.delta`、`tool.progress`、`task.progress`、`command.output` 和 `provider.raw`。 |
| `WithMaxDepth(n)` | 只接收嵌套不超过 n 层的事件。为 0 时只接收本 session 自己的事件。 |
| `WithBuffer(n)` | 设置订阅队列的高水位，默认为 512。 |
| `WithStallTimeout(d)` | 订阅者超过这段时间不消费就会被摘除，默认为 60s，设为负数则不摘除。 |
| `WithCloseOnDone()` | 在最后一个事件之后关闭 channel。只适用于没有其他写入方的 channel。 |
| `WithNative()` | 在 `message.completed` 中保留模型接口的原始数据。 |

订阅方法返回 `*kernel.Subscription`，它提供以下方法：

- `Unsubscribe()` 取消订阅，调用不会阻塞。
- `Done()` 在订阅结束后关闭，此后 SDK 不会再访问你的 channel 或回调。
- `Err()` 返回订阅被摘除的原因，可能是 `ErrSubscriberStalled`、`ErrSubscriberOverflow`、`ErrChannelClosed`、回调返回的错误，或者 `ErrSessionClosed`。主动调用 `Unsubscribe` 之后返回 nil。
- `Dropped()` 返回被丢弃的事件数。

## 6.3 背压

事件是有序的，默认不会丢弃。当订阅者消费较慢时，只会让模型流等待订阅者，**不会卡住 session 本身**。`Send`、`Interrupt` 和 `Snapshot` 仍然会立即返回。

关于背压，需要了解以下几点：

- 只有增量类事件，即负载实现了 `event.Bulk` 的事件，才参与背压。`turn.completed` 这类终结事件不会被丢弃。
- 如果不想拖慢模型，可以使用 `WithDropDeltas`，或者调大 `WithBuffer`。
- 超过 `WithStallTimeout` 仍不消费的订阅者会被摘除，其 `Err()` 返回 `ErrSubscriberStalled`。其他订阅者会收到一条 `error{code:subscriber_detached}` 事件，监控系统可以据此告警。

> 注意：不要在订阅回调中同步等待本 session 的 turn。回调在等待 turn，而 turn 又在等待回调消费事件，两者会形成死锁。这种情况下 kernel 会返回 `ErrReentrantWait`，详见 [19](19-callback-rules.md)。

## 6.4 事件的归属

`ev.Path` 为空表示事件属于本 session 自己。subagent、进程内 peer 和后台任务产生的事件会带有 `Path`，其中每一跳是一个 `event.Hop`，包含以下字段：

| 字段 | 含义 |
|---|---|
| `Kind` | 取值为 `task` 时表示由任务管理器写入，也可以是调用方自定的标签，例如 `side`。 |
| `ID`、`Name` | 这一跳的 id 和名字，例如任务 id 或 subagent 的类型名。 |
| `SessionID` | 这一跳挂在哪个 session 之下。 |
| `CallID`、`TurnID` | 引出这一跳的工具调用和 turn。 |

默认情况下，所有嵌套事件都会转发，包括增量事件。如果只想渲染主对话的文本，可以判断 `len(ev.Path) == 0`。如果想把 subagent 的输出折叠显示，可以按 `Path[0].ID` 分组。

## 6.5 事件清单

下表按类别列出了 kernel 发出的事件。部分事件的字段在其他章节中有详细说明，表中给出了相应的链接。

| 类别 | 事件类型 | 负载类型 | 关键字段 |
|---|---|---|---|
| 会话 | `session.started` | `SessionStarted` | `Source`（new、resume、fork 等）、`Model`、`Tools`、`Commands`、`Extensions` |
| | `session.state_changed` | `SessionStateChanged` | `From`、`To`（initializing、idle、running、maintenance、closing、closed） |
| | `session.closed` | `SessionClosed` | `Reason`、`LeakedWorkers` |
| | `system.rendered` | `SystemRendered` | `Epoch`、`Sections`（每段的 slot、名字和字符数）、`Hash` |
| 输入 | `input.accepted`、`input.injected`、`input.dropped` | 见 [5.9](05-input.md#59-输入相关的事件) | |
| 轮次与步骤 | `turn.started`、`turn.interrupt_requested`、`turn.completed` | 见 [5.9](05-input.md#59-输入相关的事件) | |
| | `step.started` | `StepStarted` | `MessageID`、`API`、`Model`、`Attempt` |
| | `step.completed` | `StepCompleted` | `StopReason`、`Usage`、`TTFTMs`（首字耗时）、`DurationMs` |
| 模型 | `model.retry` | `ModelRetry` | `Attempt`、`Error`、`DelayMs` |
| | `model.notice` | `ModelNotice` | `Kind`、`Detail`，见 [4.4](04-models.md#44-模型能力) |
| | `model.anomaly` | `ModelAnomaly` | 流式输出中发现的重复，字段为 `Kind`（repetition）、`Channel`、`Offset`、`Detail`（`Mode`、`UnitChars`、`Repetitions`、`Sample`），见本节末尾 |
| | `provider.raw` | `ProviderRaw` | 接口的原始事件，需要开启 `kernel.WithRawProviderEvents()` |
| 消息 | `message.started` | `MessageStarted` | `MessageID`、`Role` |
| | `message.delta` | `MessageDelta` | `MessageID`、`BlockIndex`、`Channel`（text 或 refusal）、`Text` |
| | `message.block` | `MessageBlock` | 在非文本块结束时发出，字段为 `BlockType`、`HostedCall`、`HostedResult`、`Opaque` |
| | `message.completed` | `MessageCompleted` | `Message` 为完整消息，`Partial` 表示被打断且只包含已结束的块 |
| | `message.discarded` | `MessageDiscarded` | `MessageID`、`Reason` |
| 思维链 | `reasoning.started`、`reasoning.delta`、`reasoning.completed` | `Reasoning*` | `Kind`、`Text`，`completed` 另有 `Replayable` |
| 工具调用（模型生成中） | `tool_call.started`、`tool_call.delta`、`tool_call.completed` | `ToolCall*` | `CallID`、`Name`、`ArgsDelta` 或 `Input`、`InputError` |
| 工具执行 | `tool.started` | `ToolStarted` | `CallID`、`Name`、`Input`、`Group`（并行组号）、`Movable`（可转入后台） |
| | `tool.progress` | `ToolProgress` | `CallID`、`Text`、`Data` |
| | `tool.completed` | `ToolCompleted` | `CallID`、`Name`、`Outcome`、`IsError`、`Content`、`Structured`、`DurationMs`、`OriginalChars`（结果文本超过 `Limits.MaxToolResultChars` 被截断时的原始长度，否则为 0） |
| | `tool.abandoned` | `ToolAbandoned` | 表示打断后在宽限期内没有退出的调用 |
| 审查 | `approval.requested`、`approval.resolved` | 见 [8.4](08-approval.md#84-事件) | |
| 任务 | `task.started`、`task.progress`、`task.status_changed`、`task.instruction`、`task.finished`、`tasks.snapshot` | 见 [12.5](12-tasks.md#125-事件) | |
| hook | `hook.started`、`hook.completed`、`hook.failed` | 见 [9.5](09-hooks.md#95-事件) | |
| 用量 | `usage.updated` | `UsageUpdated` | `Bucket`、`StepUsage`、`SessionTotal`、`Context`（窗口占用） |
| 上下文 | `context.injected` | `ContextInjected` | 在提醒送达时发出，字段为 `Sources`、`Count`、`Truncated` |
| | `context.compaction_started`、`context.compacted`、`context.compaction_failed` | `ContextCompact*` | `Trigger`、`Strategy`、`PreTokens`、`PostTokens`、`Epoch` |
| | `context.cleared`、`context.rewound` | `ContextCleared`、`ContextRewound` | 前者为 `Epoch`，后者为 `TurnID`、`InputID`、`Dropped` |
| | `side_chat.dropped` | `SideChatDropped` | 主会话清空或回退后旁路对话被丢弃时发出，字段为 `Name`、`Reason`，见 [15.4](15-context.md#154-分叉与旁路对话) |
| 命令 | `command.started`、`command.output`、`command.completed` | `Command*` | `CommandID`、`Name`、`Args`、`Text`、`Data`、`Error` |
| 模型切换 | `provider.switched`、`provider.switch_rejected` | 见 [4.6](04-models.md#46-运行中切换模型) | |
| 其他 | `registry.changed` | `RegistryChanged` | 在注册内容变化时发出，字段为 `Scope`、`Kind`、`Name`、`Op`、`Effective` |
| | `notice` | `Notice` | 承载 hook 的 `SystemMessage`，字段为 `Source`、`Text`；内核自己发出的通知还带 `Kind` 和 `Detail`，宿主可据此翻译 |
| | `error` | `Error` | `Code`（例如 `store_failed`）、`Message`、`Fatal`、`Detail` |

每种负载都是 `event` 包中的一个结构体，例如 `*event.MessageDelta`，需要通过类型断言获取。`event.Type*` 常量是这些类型名在 Go 中的写法，例如 `event.TypeMessageDelta`。

`model.anomaly` 是 advisory 事件。检测器跟着流式增量跑，发现短周期复读（`Detail.Mode` 为 `suffix`）、整行复读（`line`）或整段复读（`block`）时发一条：`Offset` 是块内已输出的字符数，`Sample` 是截断到 256 字符的尾部样本，每个块每种模式至多一条。它不改变回复、不结束 turn，也不写进历史。要不要干预由你决定，例如收到后调用 `sess.InterruptTurn(turnID, "repetition")`，或等 `message.completed` 后拒绝整条回复。检测默认开启，阈值用 `kernel.WithOutputAnomalyDetection` 调整，`AnomalyConfig{Disabled: true}` 关闭。它只看文本和 reasoning，不检测工具参数，也识别不了换一种说法的语义重复。

## 6.6 UI 渲染规则

渲染流式输出的 UI 需要遵守以下两条规则：

- 每一串增量事件之后**一定会有一个带完整内容的终结事件**，即 `message.completed`、`reasoning.completed` 或 `tool_call.completed`。渲染时按 `MessageID` 和 `BlockIndex` 累加增量，收到终结事件后再用它的完整内容覆盖一次，这样可以修正丢失的增量。
- 当模型调用被重试、被拒答或被 hook 拒绝时，**已经发出的增量之后会跟一个 `message.discarded`**。UI 应据此清除已经渲染的部分。

一条 assistant 消息及其后续工具执行的事件按以下顺序发出：

1. 首先是 `message.started`。
2. 然后是若干 `reasoning.*`、`message.delta`、`tool_call.*` 和 `message.block` 事件。
3. 消息以 `message.completed` 结束，或者以 `message.discarded` 结束。
4. 接着，这批工具中的每个调用分别发出 `tool.started`、`tool.progress` 和 `tool.completed`。
5. 最后是下一个 `step.started`。

## 6.7 跨进程传递

事件经过 `json.Marshal` 序列化后，可以发送到其他进程或前端。如果另一端也使用 Go，可以用解码器把事件还原：

```go
dec := event.NewDecoder(skills.Payloads()...) // 扩展的事件类型需要告诉解码器
ev, err := dec.Decode(raw)
```

解码器认识全部 kernel 事件。不认识的类型会解码为 `*event.Unknown`，再次编码时原样写回，因此经过中转不会丢失数据。前端可以直接按 `type` 字段分发事件，各事件的 JSON 字段见 `spec/schema/event.schema.json`。

## 6.8 发送自定义事件

扩展和工具可以发出自定义事件，供 UI 或其他订阅者使用。发送方式如下：

- 在工具中调用 `tc.Emit(payload)`，事件会归到这次工具调用名下。
- 在其他地方调用 `sess.Emit(payload)`。

`payload` 可以是任何实现了 `EventType() event.Type` 的类型。类型名建议采用 `<扩展名>.<动作>` 的形式，以便与内置事件区分开。

内置扩展的事件也通过同样的方式发出，例如 `skills.Activated`、`memory.Saved` 和 `mcp.ServerStatusEvent`。这些事件定义在各个扩展包中，JSON 格式见 `spec/ext/<x>/events.schema.json`。

## 6.9 建议做法

- 构建聊天界面时，建议订阅 `message.`、`reasoning.`、`tool.`、`turn.` 和 `approval.`，并按 [6.6](#66-ui-渲染规则) 的规则渲染。如果只显示主对话，请过滤出 `len(ev.Path) == 0` 的事件。
- 记录日志时，建议使用 `WithDropDeltas()`，这样一轮只剩几十条事件。`message.completed` 中有完整的文本，`tool.completed` 中有完整的结果。
- 实现监控时，建议在 agent 级通过 `ag.SubscribeFunc` 订阅 `turn.completed`、`usage.updated`、`model.retry`、`error` 和 `session.closed`，并按 `ev.SessionID` 聚合。
- 建议每个消费者使用一个独立的订阅，不要让多个 goroutine 争抢同一个 channel。
- 建议在回调中只做耗时短的操作，例如写缓冲区或发送到 channel，耗时的操作交给其他 goroutine。否则模型流会被你的回调拖慢。

---

[← 上一章：发送输入与等待结果](05-input.md) · [目录](README.md) · [下一章：注册工具 →](07-tools.md)
