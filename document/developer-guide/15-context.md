[← 上一章：内置扩展](14-builtin-extensions.md) · [目录](README.md) · [下一章：存储与恢复 →](16-storage.md)

# 15. 上下文管理

本章介绍如何控制对话历史在模型窗口中的呈现方式，内容包括自动压缩、手动压缩、清空、回退、分叉、旁路对话、检索历史和查看 token 明细。当对话变得很长，或者你需要实现“撤销”、“分支”一类的功能时，可以参考本章。

本章内容：[15.1 自动压缩](#151-自动压缩) · [15.2 手动压缩与清空](#152-手动压缩与清空) · [15.3 回退](#153-回退) · [15.4 分叉与旁路对话](#154-分叉与旁路对话) · [15.5 检索历史](#155-检索历史) · [15.6 自定义压缩策略](#156-自定义压缩策略) · [15.7 Pin 与 SideCall](#157-pin-与-sidecall) · [15.8 查看上下文](#158-查看上下文) · [15.9 建议做法](#159-建议做法)

## 15.1 自动压缩

可用窗口等于模型窗口减去最大输出。当上下文超过可用窗口的 90% 时，kernel 会在两次模型调用之间自动压缩。压缩时，kernel 让压缩策略生成一份摘要，然后开启一个新的 epoch，把摘要和 Pin 放在新 epoch 的开头。默认的压缩策略是 `ext/compact`，它让模型来写摘要。

自动压缩要求已经安装压缩策略，`std.Defaults()` 已经包含了它。如果没有安装压缩策略，kernel 不会自动压缩，窗口占满时当前这一轮会以错误结束。

压缩开始时会发出 `context.compaction_started` 事件。压缩成功后发出 `context.compacted`，其中包含 `Trigger`、`Strategy`、`PreTokens`、`PostTokens` 和 `Epoch` 字段。压缩失败则发出 `context.compaction_failed`。压缩前后分别触发 `pre_compact` 和 `post_compact` hook。新 epoch 开始时还会触发 `session_start` hook，其 `Source` 为 `compact`。

你可以通过 `kernel.ContextPolicy` 调整自动压缩的行为：

```go
kernel.WithContextPolicy(kernel.ContextPolicy{
	MaxContextTokens: 150000, // 设置一个比模型窗口更小的上限
	AutoCompactRatio: 0.85,
})
```

`ContextPolicy` 的字段如下。

| 字段 | 默认值 | 说明 |
|---|---|---|
| `MaxContextTokens` | 模型窗口 | 人为设置一个更小的窗口。 |
| `AutoCompactRatio` | 0.9 | 上下文占可用窗口的比例达到该值时触发自动压缩。 |
| `TargetRatio` | 0.1 | 摘要的目标长度占可用窗口的比例。 |
| `MinSummaryTokens`、`MaxSummaryTokens` | 20k、50k | 摘要长度的下限和上限。 |
| `PinTokens`、`PinTotalTokens` | 5k、25k | 单个 Pin 的上限和全部 Pin 合计的上限。 |
| `MaxAutoCompactsPerTurn` | 2 | 一轮之内最多自动压缩的次数。 |
| `MaxConsecutiveFailures` | 3 | 连续失败达到该次数后，不再尝试自动压缩。 |
| `Disabled` | false | 设为 true 时关闭自动压缩。 |
| `StopTasksOnClear` | false | 设为 true 时，执行 `/clear` 会停止后台任务。 |

默认情况下，kernel 估算上下文的 token 数。使用 `kernel.WithTokenCounter(tc)` 后，当上下文接近阈值时，kernel 会改用精确计数。Anthropic 适配器自带 `CountTokens`，其他接口则需要你自己提供计数器。

## 15.2 手动压缩与清空

除了自动压缩，你也可以主动压缩上下文或将其清空。`Compact` 默认压缩全部上下文，也可以通过选项只压缩某一轮之前或之后的部分。`Clear` 则直接开启一个新的 epoch。

```go
report, err := sess.Compact(ctx, "Keep every decision about the budget.")        // 压缩全部上下文
report, err = sess.Compact(ctx, "", kernel.CompactUpTo(turnID))                   // 只压缩这一轮之前的部分
report, err = sess.Compact(ctx, "", kernel.CompactFrom(turnID))                   // 只压缩这一轮之后的部分
err = sess.Clear(ctx)                                                              // 清空上下文并开启新 epoch
```

`Compact` 返回的 `CompactReport` 包含 `Strategy`、`PreTokens`、`PostTokens` 和 `Epoch` 字段。如果没有可压缩的内容，返回 `ErrNothingToCompact`。如果没有安装压缩策略，返回 `ErrNoCompactor`。

`Clear` 会开启一个新的 epoch 并重新渲染系统提示词，同时清空 Pin 集合，丢弃之前排队的提醒。后台任务默认继续运行。新 epoch 开始时会触发 `session_start` hook，其 `Source` 为 `clear`。

这两个方法都是等待型方法。如果有 turn 正在运行，调用会排队，等它结束后再执行。在忙时策略为 `WaitIdle` 的命令中调用时则立即执行。如果在工具或 hook 内部调用，会返回 `ErrReentrantWait`，这时应改用非阻塞的 `sess.Command("compact", ...)`。

## 15.3 回退

回退把上下文恢复到某一轮开始之前的状态。你可以先用 `Turns` 获取可回退的轮次，再把选中轮次的 ID 传给 `Rewind`。

```go
turns := sess.Turns()                       // 可回退的轮次，最早的排在前面
report, err := sess.Rewind(ctx, turns[len(turns)-1].ID) // 回到最后一轮开始之前
```

`TurnInfo` 包含 `ID`、`Seq`、`Input` 和 `Started` 字段。`Seq` 是这一轮第一行的序号。`Input` 是那一轮的输入内容，UI 可以用它让用户编辑后重新发送。只有当前 epoch 中的轮次可以回退。压缩之前的历史无法回退，只能检索。

`Rewind` 返回的 `RewindReport` 包含 `TurnID`、`Epoch`、`Dropped` 和 `Input` 字段，其中 `Dropped` 表示丢弃了多少行。回退之后会发出 `context.rewound` 事件，并开启一个新的 epoch，同时触发 `session_start` hook，其 `Source` 为 `rewind`。系统提示词保持不变。如果传入的轮次不存在，返回 `ErrUnknownTurn`。

> 注意：回退只改变上下文。后台任务会继续运行，排队的输入和任务通知会保留，工具已经造成的外部副作用也不会被撤销。此外，在回退点之前分出的旁路对话会被丢弃，详见下一节。

## 15.4 分叉与旁路对话

分叉和旁路对话都从当前会话出发，但用途不同。分叉把当前上下文复制成一个独立的会话，之后两边互不相关。旁路对话挂在主会话上，每一轮都会带上主会话此刻的上下文，适合在主会话工作的同时随时向它提问。

**分叉**会把当前上下文复制成一个新的、独立的 session。新 session 有自己的 id，可以像其他 session 一样被恢复。

```go
branch, err := sess.Fork(ctx,
	kernel.ForkAt(turnID),                   // 从这一轮开始之前分叉，默认使用当前稳定的历史
	kernel.ForkTools("read_file", "search"), // 分支中只允许调用这些工具，工具定义照常发送，调用时再拦截
	kernel.ForkSink(ch),                     // 创建时就挂上订阅，ForkFunc(fn) 是对应的回调版本
)
```

分叉出来的 session 在新 epoch 开始时会触发 `session_start` hook，其 `Source` 为 `fork`。

**旁路对话（side chat）**是挂在主会话上、按名字管理的对话。它保存到存储中，可以反复打开。

```go
side, err := sess.SideChat(ctx, "notes")   // 打开名为 notes 的旁路对话，之后再打开会返回同一个
names, err := sess.SideChats(ctx)          // 返回已打开的旁路对话，以及存储中尚未丢弃的旁路对话
err = sess.DropSideChat(ctx, "notes")      // 丢弃旁路对话，下次打开时从头开始
```

旁路对话自己不保存主会话的内容。每一轮开始时，SDK 取一次主会话当前的上下文，放在旁路对话自己的历史前面，再加上这一轮的输入，一起发给模型。因此旁路对话总能看到主会话的最新进展，包括打开之后主会话才说的内容。如果主会话正在等待某个工具的结果，请求中会为这次调用补一个"尚无结果"的占位结果，这个占位结果不会写入任何存储。同一轮的各个步骤使用同一份主会话上下文，主会话在这一轮中的新进展要到下一轮才能看到。如果主会话在两轮之间发生了变化，SDK 会在下一轮的输入前加一条提醒，告诉模型主会话已经变了。

这部分主会话内容与主会话自己下一次请求的前缀相同。只要旁路对话和主会话使用同一个模型、相同的系统提示词和工具定义，这部分就能直接读取主会话的缓存，只有旁路对话自己的内容需要重新写入缓存。

旁路对话的上下文用量把主会话部分计算在内，`Context()` 的明细中有一项 `parent` 表示这部分的大小。对旁路对话执行压缩、清空或回退时，只影响它自己的对话，主会话部分在每一轮照常带上。如果主会话部分本身就占满了窗口，旁路对话要等主会话压缩之后才能继续。如果主会话没有先关闭旁路对话就意外退出了，旁路对话的下一轮会失败，旁路对话随即以 `main_closed` 为原因关闭。

旁路对话可以用 `ForkTools` 限制可调用的工具，规则与分叉相同。在旁路对话中调用 `search_history` 并传入 `parent: true` 会得到一条错误，因为主会话的当前上下文已经在请求中了。

旁路对话是一个独立的 session，它的事件发在自己的事件流上，不会转发到主会话。你需要直接订阅它，也可以在打开时传入 `ForkSink` 或 `ForkFunc`，在创建前挂上订阅。旁路对话必须从主会话通过 `SideChat` 打开，如果对它的 id 调用 `Resume`，会返回 `ErrSideChat`。

主会话执行清空或回退之后，在此之前打开的旁路对话会被关闭并标记为已丢弃，同时发出 `side_chat.dropped{Name, Reason}` 事件，下次按同一个名字打开时从头开始。主会话压缩或切换模型不会丢弃旁路对话，旁路对话的下一轮会带上压缩或切换之后的上下文。

旁路对话中不能再打开旁路对话，`SideChat` 也不接受 `ForkAt`，这两种情况都返回 `ErrSideChatUnsupported`。在旁路对话中执行 `/fork` 命令会被拒绝。如果想把旁路对话变成一个独立的会话，请在旁路对话上调用 `Fork`。得到的会话包含主会话当前的上下文和旁路对话自己的内容，之后与两者都不再关联。如果不再需要原来的旁路对话，再调用 `DropSideChat` 即可：

```go
standalone, err := side.Fork(ctx)       // 复制主会话当前的上下文和旁路对话的内容
err = sess.DropSideChat(ctx, "notes")   // 需要时再丢弃原来的旁路对话
```

## 15.5 检索历史

即使历史已经被压缩出上下文，你仍然可以检索它。`SearchHistory` 按关键词查找历史行，`HistoryLine` 按序号读取某一行的完整内容。

```go
hits, err := sess.SearchHistory(ctx, kernel.HistoryQuery{Words: []string{"budget", "hotel"}, Limit: 10})
line, inContext, err := sess.HistoryLine(ctx, hits[0].Seq)
```

`HistoryQuery` 的字段如下。

| 字段 | 说明 |
|---|---|
| `Words` | 检索词，每个词都必须出现，不区分大小写。为空时返回 `ErrEmptyQuery`。 |
| `Limit` | 返回的结果数，默认为 10，最多为 50。 |
| `Current` | 设为 true 时只查询当前上下文中的行。 |

每条结果是一个 `HistoryHit`，包含 `Seq`、`Epoch`、`Role`、`TurnID`、`Current` 和 `Snippet` 字段，其中 `Current` 表示该行是否还在当前上下文中。检索结果包括压缩之前的行，按从新到旧的顺序排列。`HistoryLine` 读取整行内容，找不到时返回 `ErrUnknownLine`。模型使用的对应工具是 `search_history`，详见 [14.2](14-builtin-extensions.md#142-exthistory)。

## 15.6 自定义压缩策略

如果只想更换摘要提示词，使用 `compact.Options` 即可，详见 [14.1](14-builtin-extensions.md#141-extcompact)。如果需要完全不同的压缩方式，可以实现 `kernel.Compactor` 接口，并通过 `Registrar.Compactor` 注册。每个作用域只能注册一个压缩策略，session 级的策略会遮蔽 agent 级的策略。

```go
type Compactor interface {
	Compact(ctx context.Context, req CompactRequest) (CompactResult, error)
}
```

`CompactRequest` 的字段如下。

| 字段 | 说明 |
|---|---|
| `Trigger` | 触发原因，取值为 `manual`、`auto`、`overflow` 或 `switch`。`overflow` 表示窗口已经溢出，必须压缩。`switch` 表示切换模型时上下文放不进新窗口，详见 [4.6](04-models.md#46-运行中切换模型)。 |
| `Instructions` | 本次压缩的说明，来自 `/compact` 命令的参数或 `Compact` 方法的参数。 |
| `History`、`HeadLen` | `History` 是当前上下文，不含 system 行。其中前 `HeadLen` 行是 epoch 的开头，kernel 会重建这部分，因此不要把它们放进 `Replace`。 |
| `PrevSummary` | 本 epoch 开始时的摘要，你可以在它的基础上续写。 |
| `CurrentTurn` | 正在运行的 turn 从第几行开始。为 -1 时表示当前空闲。 |
| `Range` | 只压缩一部分上下文时的范围。 |
| `Tokens`、`Target`、`Threshold` | 分别是当前的 token 数、摘要的目标 token 数和自动压缩的阈值。 |
| `Caps` | 当前模型的能力。 |
| `Estimate(msgs)` | 估算一组消息的 token 数。 |
| `Summarize(ctx, msgs, instructions)` | 用当前模型发送一次不进历史的请求。`msgs` 为 nil 时发送整个当前上下文，这样可以复用主循环的缓存前缀。 |

`CompactResult` 包含三个字段。`Summary` 是放在新 epoch 开头的摘要。`Replace` 是需要原样保留、放在摘要之后的行，例如正在运行的 turn，为 nil 时一行也不保留。`Strategy` 是记录到 epoch 边界中的策略名。

## 15.7 Pin 与 SideCall

**Pin** 是压缩之后需要重新注入上下文的内容。它来自工具结果的 `Pin` 字段或命令结果的 `Pin`，详见 [7.4](07-tools.md#74-toolresult)。`sess.Pins()` 返回当前的全部 Pin，最早的排在前面。`sess.Unpin(ctx, key)` 撤销一个 Pin，撤销后当前 epoch 中已有的文字仍然保留，但下一个 epoch 不再注入。按照 `ContextPolicy` 的默认值，每个 Pin 最多 5k token，全部 Pin 合计最多 25k token。

**`SideCall`** 使用本 session 的系统提示词、工具和模型发送一次请求，但结果不会写入历史。这类调用适合用来自己做摘要、分类或打分。

```go
msg, usage, err := sess.SideCall(ctx, kernel.SideRequest{
	IncludeHistory: true,                                  // 带上当前稳定的历史
	Messages:       []message.Message{message.UserText("Summarize the decisions so far in 3 bullets.")},
	NoTools:        true,                                  // 不允许模型调用工具
	MaxOutputTokens: 1000,
	Bucket:         "summary",                             // 用量记到这个桶中
})
```

`SideRequest` 还有以下字段：`Preamble` 是放在你的消息之前的提醒，`Hop` 指定事件归属于哪一跳，`Emit` 决定是否发出流式事件。

## 15.8 查看上下文

`sess.Context()` 返回一个 `ContextReport`，描述当前上下文的 token 占用情况，字段如下。

| 字段 | 说明 |
|---|---|
| `Model`、`Epoch` | 当前的模型和 epoch。 |
| `Window`、`Usable`、`Threshold`、`Used` | 分别是窗口大小、可用窗口（即减去最大输出后的部分）、自动压缩阈值和已用 token 数。 |
| `Basis` | 已用 token 数的来源。`reported` 表示模型上次报告的值，`estimated` 表示估算值，`counted` 表示精确计数。 |
| `Parts` | 按来源列出的明细，每项包含 `Kind`、`Name`、`Tokens` 和 `Count`。`Kind` 的取值有 system、tool、summary、pin、epoch_head、user、assistant、reasoning、tool_call、tool_result 和 reminder。旁路对话还有一项 `parent`，表示它带上的主会话部分。 |

`/context` 命令显示的就是这份报告。此外，`usage.updated` 事件的 `Context` 字段记录了每次模型调用后的上下文占用。

## 15.9 建议做法

- 不要关闭自动压缩。在长对话中，它是防止窗口溢出的唯一保障，需要调整时应修改 `AutoCompactRatio` 和 Pin。
- 必须记住的事实应做成 Pin。摘要不一定会保留这些事实，Pin 则会在每次压缩后重新注入。
- 建议为用户提供“撤销上一轮”的功能。用 `Turns()` 取得最后一轮，调用 `Rewind` 后，把 `RewindReport.Input` 填回输入框即可。
- 想在主会话工作的同时问问它进展到哪一步，用旁路对话即可，它每一轮都能看到主会话的最新状态。想“试一下另一种方案”时，用分叉。
- 摘要、分类这类辅助调用适合用 `SideCall` 完成。它的用量会记入 session 的账本，结果也不会污染历史。
- 在压缩策略中调用 `Summarize` 时，建议传入 nil。这样它会发送整个上下文，从而命中主循环的缓存。

---

[← 上一章：内置扩展](14-builtin-extensions.md) · [目录](README.md) · [下一章：存储与恢复 →](16-storage.md)
