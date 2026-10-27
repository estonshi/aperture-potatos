[← 上一章：回调中的调用限制](19-callback-rules.md) · [目录](README.md) · [下一章：测试 →](21-testing.md)

# 20. 错误

本章列出 SDK 可能返回的错误以及各自出现的情况。读完本章，你可以使用 `errors.Is` 和 `errors.As` 对不同的错误分别处理。

本章内容：[20.1 哨兵错误](#201-哨兵错误) · [20.2 结构化错误](#202-结构化错误) · [20.3 模型调用错误](#203-模型调用错误) · [20.4 错误事件](#204-错误事件)

## 20.1 哨兵错误

kernel 导出了以下哨兵错误，你可以用 `errors.Is` 判断：

| 错误 | 出现的情况 |
|---|---|
| `ErrSessionClosed`、`ErrAgentClosed` | 对已经关闭的 session 或 agent 调用方法。 |
| `ErrSessionClosing` | `Close` 打断正在运行的 turn 时，作为 turn ctx 的取消原因。 |
| `ErrQueueFull` | 输入队列已满，或者给任务发送指令时它的收件箱已满。reminder 会先被合并，因此只有 human、command 和 agent 输入会遇到队列已满的情况。调用方发送请求的速度远超 session 的处理速度时，`Withdraw` 和 `InterruptWith` 也可能返回这个错误。 |
| `ErrNotQueued` | 调用 `Withdraw` 或 `InterruptWith` 时，指定的输入不在队列中，例如已经被某一轮取走、已经被丢弃，或者 id 指向排队中的命令。对任务通知调用 `Withdraw` 也会返回这个错误。这个错误只描述调用的那一刻，被取走但还没送达的输入有可能回到队列，见 [5.7](05-input.md#57-撤回与立即发送排队中的输入)。 |
| `ErrReentrantWait` | 在回调中等待运行自己的 session，见 [19](19-callback-rules.md)。 |
| `ErrBusy` | `RejectBusy` 命令遇到正在运行的 turn。这个错误需要从回执的 `TurnResult.Err` 中获取，`Send` 和 `Command` 本身不会返回它。 |
| `ErrDuplicate`、`ErrInvalidName` | 重复注册，或者名字不合法。 |
| `ErrInvalidOrigin`、`ErrEmptyInput`、`ErrInvalidMode` | 传给 `Send` 的输入不合法。 |
| `ErrUnknownCommand` | 调用的命令不存在。 |
| `ErrCapability` | 模型能力不允许当前的配置。 |
| `ErrInvalidWindow` | 传给 `SetContextWindow` 或 `SessionContextWindow` 的窗口是负数。 |
| `ErrNoMatcher`、`ErrAsyncNotAllowed` | 前者表示这个 hook 点不支持匹配器，后者表示这个 hook 点只接受同步 hook。 |
| `ErrBlocked` | 操作被 hook 阻断。`Compact` 被 `pre_compact` 阻断时直接返回这个错误。命令被 `command_invoke` 阻断时，这个错误出现在回执的 `TurnResult.Err` 中。 |
| `ErrSessionOpen`、`ErrSessionNotFound`、`ErrSideChat`、`ErrChildSession` | `NewSession` 或 `Resume` 遇到不允许的情况，见 [16.2](16-storage.md#162-恢复会话)。 |
| `ErrNoLabelStore` | 存储没有实现 `store.LabelStore`，因此不能调用 `SetLabels` 和 `FindSessions`，见 [16.5](16-storage.md#165-会话标签)。 |
| `ErrSideChatUnsupported` | 在旁路对话中再打开旁路对话，或者给 `SideChat` 传入了 `ForkAt`，见 [15.4](15-context.md#154-分叉与旁路对话)。 |
| `ErrTurnNotFound` | `TurnCalls` 查不到指定的轮次，即这一轮超出了内存中保留的最近 100 轮，并且没有配置 `CallStore`。 |
| `ErrNoCostSource` | 没有配置费用来源就调用了 `TurnCost`。 |
| `ErrNotMovable` | `BackgroundCall` 找不到可以转入后台的调用。 |
| `ErrTaskNotFound`、`ErrTaskNotContinuable`、`ErrTaskNoInstructions`、`ErrTaskNotRestartable`、`ErrTaskLimit`、`ErrTaskBusy` | 任务相关的错误。`ErrTaskNotFound` 表示任务不存在。`ErrTaskNotContinuable` 表示任务已经结束，且它的类型不支持续接，这时应改用重启。`ErrTaskNoInstructions` 表示这种类型的任务不接受追加指令。`ErrTaskNotRestartable` 表示这种类型的任务不支持重启。`ErrTaskLimit` 表示正在运行的任务数已达上限。`ErrTaskBusy` 表示任务正在重启或续接，稍后重试即可。 |
| `ErrNoCompactor` | 没有注册压缩策略就调用了 `Compact`，例如没有安装 `std.Defaults()`。 |
| `ErrNothingToCompact`、`ErrUnknownTurn`、`ErrUnknownInput`、`ErrUnknownPin`、`ErrEmptyQuery`、`ErrUnknownLine` | 上下文管理接口的参数或当前状态不允许这项操作。`ErrUnknownInput` 和 `ErrUnknownTurn` 都由 `Rewind` 和 `ForkAt` 返回：id 既不是输入也不是轮次时，错误同时匹配这两个哨兵。 |
| `ErrSubscriberStalled`、`ErrSubscriberOverflow`、`ErrChannelClosed` | 订阅被摘除的原因，可以从 `Subscription.Err()` 获取。 |
| `ErrSeqConflict`、`ErrIncompatibleStore` | 前者表示存储中的行与内存中的状态不一致，通常是因为存在第二个写入者。后者表示存储格式不兼容。出现这两个错误时会话会随之失败。它们是 `store` 包中错误的重新导出。 |

## 20.2 结构化错误

以下错误类型带有结构化字段，需要用 `errors.As` 取出：

| 类型 | 字段 | 出现的情况 |
|---|---|---|
| `*kernel.InputError` | `Detail` | `NewTool` 解码工具输入失败。kernel 会把它转换成 `invalid_input` 结果，不会调用工具。 |
| `*kernel.InterruptError` | `Reason` | 作为被打断的 turn 的 ctx 取消原因。 |
| `*kernel.HookError` | `Kind`（timeout、error 或 panic）、`Err` | hook 失败时传给 hook 点的 `Closed`。模型只会看到 `Kind`。 |
| `*kernel.LeakError` | `Workers` | `Close` 超时，仍有 worker 没有退出。 |
| `*kernel.TaskDeniedError` | `Text` | 任务启动被 `task_start` hook 拒绝，或者任务重启被审查拒绝。`Text` 是写给模型看的说明。 |
| `*kernel.CostError` | `Status`、`Message` | `CostSource` 用它报告计费后端返回的 HTTP 状态。 |
| `*provider.Error` | 见 [20.3](#203-模型调用错误) | 模型调用失败。 |

## 20.3 模型调用错误

模型调用失败时，`turn.completed` 事件的 `Error` 和 `TurnResult.Err` 都带有错误分类。你可以用 `provider.AsError(err)` 取出 `*provider.Error`，它包含以下字段：

| 字段 | 说明 |
|---|---|
| `Class` | 错误分类，取值见下一张表。 |
| `API`、`Status`、`Type`、`Code`、`Message` | 出错的接口、HTTP 状态、接口返回的错误类型和错误码，以及错误消息。 |
| `RetryAfter` | 接口要求的等待时间。 |
| `RequestID` | 请求 id，联系厂商支持时使用。 |
| `MidStream` | 为 true 时表示流在中途断开。 |
| `NoRetry` | 为 true 时表示服务端明确要求不要重试。 |
| `Limit` | 发生输出上限错误时，从错误消息中解析出的上限值。 |
| `Raw` | 原始的错误响应体。 |

`Class` 的取值如下：

| `Class` | 含义 | 是否重试 |
|---|---|---|
| `transient` | 临时故障，例如 5xx 或连接断开。 | 是 |
| `rate_limited` | 请求被限流。 | 是 |
| `quota` | 配额已用尽。 | 否 |
| `auth` | 鉴权失败。 | 否 |
| `invalid_request` | 请求不合法。 | 否 |
| `context_overflow` | 上下文超出窗口。kernel 会先压缩上下文再重试。 | 压缩后重试一次 |
| `reasoning_replay` | 思维链回传被拒绝。kernel 会降级回传方式后再试一次。 | 重试一次 |
| `not_found` | 模型不存在。 | 否 |
| `capability` | 本地根据能力声明拒绝了当前配置。 | 否 |
| `canceled` | 调用被取消。 | 否 |

调用 `err.Retryable()` 可以判断错误是否可以重试。

## 20.4 错误事件

`error` 事件对应 `event.Error`，包含 `Code`、`Message`、`Fatal` 和 `Detail` 字段。`Fatal` 为 true 时，session 会因为这个错误而关闭。常见的 `Code` 是 `store_failed`，表示存储写入失败，在 `StoreWarn` 策略下会重试。

> 注意：模型调用的重试通过 `model.retry` 事件通知，不会发出 `error` 事件。

---

[← 上一章：回调中的调用限制](19-callback-rules.md) · [目录](README.md) · [下一章：测试 →](21-testing.md)
