[← 上一章：快速开始](02-quickstart.md) · [目录](README.md) · [下一章：接入模型 →](04-models.md)

# 3. 核心对象

本章介绍你在代码中会反复用到的几个对象和术语。建议在阅读后面的章节之前先熟悉它们。本章中的方法速查表也可以作为索引使用，在需要时回来查找某个方法所在的章节。

本章内容：[3.1 主要对象](#31-主要对象) · [3.2 术语](#32-术语) · [3.3 Agent 的方法](#33-agent-的方法) · [3.4 Session 的方法速查](#34-session-的方法速查) · [3.5 创建 session 时的选项](#35-创建-session-时的选项) · [3.6 查看状态](#36-查看状态) · [3.7 并发与生命周期](#37-并发与生命周期)

## 3.1 主要对象

使用 potatOS 时，你主要和以下四个对象打交道。

**`*kernel.Agent`**：一份配置，包含昵称、提示词、模型，以及注册的工具和扩展。一个 Agent 可以创建多个 session。通过 `kernel.New(opts...)` 创建。

**`*kernel.Session`**：一段对话，持有对话历史、正在运行的任务和订阅者。同一时刻一个 Session 最多运行一个 turn。通过 `ag.NewSession(ctx, opts...)` 新建，或通过 `ag.Resume(ctx, id)` 从存储中恢复。

**`*kernel.Receipt`**：一条输入或命令的回执，用来等待它被处理完成。它是 `sess.Send(...)` 和 `sess.Command(...)` 的返回值。

**`event.Event`**：一条事件，包含类型、序号、时间、来源 session、turn、`Path` 和负载 `Data`。你通过订阅获得事件，见第 [6](06-events.md) 章。

## 3.2 术语

**turn（一轮）**：从一批输入送达开始，到模型给出最终回答为止。一轮中可能包含多次模型调用。

**step**：一次模型调用，以及由它引出的那一批工具执行。

**epoch**：两次压缩、清空或回退之间的历史区间。在一个 epoch 内，系统提示词和工具定义保持不变，因此能够命中模型的 prompt 缓存。

**输入**：你交给 session 的一条内容，可以是用户的话、命令或提醒。输入的来源（`Origin`）标明它由谁提供。

**提醒（reminder）**：运行时提供给模型的背景信息。模型知道提醒不是用户说的话，也不代表任何授权。

**任务**：一种后台工作。任务启动后立即返回 id，在后台运行，结束时主动通知模型。

**扩展**：一组打包在一起的注册项，可以包含工具、命令、提示词分段、hook 等。内置能力和你自己编写的扩展属于同一种机制。

**旁路对话（side chat）**：挂在主会话上、按名字管理的对话，每一轮都带着主会话当前的上下文，通过 `SideChat` 打开，见 [15.4](15-context.md#154-分叉与旁路对话)。

## 3.3 Agent 的方法

| 方法 | 作用 |
|---|---|
| `kernel.New(opts...)` | 按选项创建一个 agent。如果选项不合法，例如工具名重复或审查规则有误，则返回错误。 |
| `ag.NewSession(ctx, opts...)` | 新建会话。方法返回时第一段历史已经写好，可以直接发送输入。 |
| `ag.Resume(ctx, id, opts...)` | 从存储中恢复会话，见 [16.2](16-storage.md#162-恢复会话)。 |
| `ag.FindSessions(ctx, labels)` | 按标签查找存储中的顶层会话，见 [16.5](16-storage.md#165-会话标签)。 |
| `ag.Derive(o)` | 派生一个新的 agent。它共享父 agent 的注册，但可以有自己的昵称、指令和工具子集，见 [13.6](13-extensions.md#136-派生-agent)。 |
| `ag.Subscribe(ch, opts...)`、`ag.SubscribeFunc(fn, opts...)` | 订阅这个 agent 下所有 session 的事件。 |
| `ag.Announce(source, text, payload)` | 向所有打开的 session 发送一条不唤醒的提醒，见 [5.5](05-input.md#55-发送不唤醒-agent-的提醒)。 |
| `ag.Nickname()`、`ag.SetNickname(name)`、`ag.Description()` | 读取或修改昵称，读取描述。修改昵称只影响之后新建的 session。 |
| `ag.Logger()` | 返回 `WithLogger` 配置的日志器，扩展用它输出告警。 |
| `ag.Close(ctx)` | 并发关闭所有 session，然后结束 agent 级的订阅。 |

## 3.4 Session 的方法速查

下面按用途对 Session 的方法分组，每组标出详细说明所在的章节。

**发送与等待**（[5](05-input.md)）

| 方法 | 说明 |
|---|---|
| `Send(in, opts...) (*Receipt, error)` | 发送一条输入，立即返回回执。 |
| `Run(ctx, in, opts...) iter.Seq2[event.Event, error]` | 发送一条输入，并迭代这一轮的事件直到本轮结束。 |
| `RunCancelable(ctx, in, opts...) iter.Seq2[event.Event, error]` | 与 `Run` 相同，但提前结束迭代会停止这次输入启动的工作：已开始的 turn 被打断，还在排队的输入被撤回。 |
| `Command(name, args) (*Receipt, error)` | 直接执行一条命令，不经过文本解析。 |
| `WaitQuiet(ctx) (TurnResult, error)` | 一直等待，直到 session 中不再有任何会自行继续的工作。也就是 turn 已结束、排队的命令已执行完、任务都已结束，并且任务通知也已发出。 |

**打断与撤回**（[5.6](05-input.md#56-打断)、[5.7](05-input.md#57-撤回与立即发送排队中的输入)）

| 方法 | 说明 |
|---|---|
| `Interrupt(reason) bool` | 打断正在运行的 turn。 |
| `InterruptAndDrop(reason) bool` | 打断正在运行的 turn，并丢弃队列中的 human、command 和 agent 输入。 |
| `InterruptTurn(turnID, reason) bool` | 只打断指定的那一轮。 |
| `Withdraw(inputID) error` | 撤回一条还在排队的输入。 |
| `InterruptWith(inputID) error` | 打断正在运行的 turn，立即处理一条还在排队的输入。 |

**订阅事件**（[6](06-events.md)）

| 方法 | 说明 |
|---|---|
| `Subscribe(ch, opts...)`、`SubscribeFunc(fn, opts...)` | 订阅这个 session 的事件。 |
| `Events(ctx, opts...)` | 在当前 goroutine 上迭代事件。 |
| `Emit(payload)` | 发出一条你自己定义的事件。 |
| `Announce(source, text, payload)` | 发送一条不唤醒的提醒，可以同时附带一个事件。 |

**查看状态**（[3.6](#36-查看状态)）

| 方法 | 说明 |
|---|---|
| `Info() SessionInfo` | 返回 id、父 session、所属 agent、标签和当前 epoch。 |
| `SetLabels(ctx, changes)` | 按键合并修改标签，值为空字符串表示删除，见 [16.5](16-storage.md#165-会话标签)。 |
| `Snapshot() Snapshot` | 返回当前阶段、turn、用量、上下文占用和正在运行的 goroutine。可以随时调用，不会阻塞。 |
| `Model() provider.ModelRef` | 返回下一轮将使用的模型。 |
| `Messages() []message.Message` | 返回当前上下文中的全部历史行。返回的是共享切片，只能读取。 |
| `Context() ContextReport` | 返回上下文的 token 明细，按来源分类。 |
| `Commands() []CommandInfo` | 返回可用的命令。 |
| `Tools() []ToolSpec` | 返回当前可见的函数工具定义，已经去掉被隐藏和派生过滤掉的工具。 |
| `HostedTools() []message.HostedTool` | 返回当前可见的服务端工具。 |
| `ApprovalRules() []ApprovalRuleInfo` | 按求值顺序返回生效的审批规则，每项带来源 scope 和是否可写入规则文件。 |
| `Parent() *Session` | 对旁路对话返回其主会话，对任务子会话返回其所属会话。根 session 返回 nil。 |

**上下文管理**（[15](15-context.md)）

| 方法 | 说明 |
|---|---|
| `Compact(ctx, instructions, opts...)` | 手动压缩上下文。 |
| `Clear(ctx)` | 清空上下文。 |
| `SetContextWindow(tokens)` | 设置这个会话的上下文窗口，0 表示回到模型能力；从下一次模型调用起生效，见 [15.1](15-context.md#151-自动压缩)。 |
| `Turns()`、`Rewind(ctx, turnID)` | 列出可以回退到的轮次，以及回退到指定轮次。 |
| `Fork(ctx, opts...)` | 把当前上下文复制成一个新的 session。 |
| `SideChat(ctx, name, opts...)`、`SideChats(ctx)`、`DropSideChat(ctx, name)` | 打开、列出和删除旁路对话。 |
| `SearchHistory(ctx, q)`、`HistoryLine(ctx, seq)` | 检索历史，包括压缩之前的历史。 |
| `Pins()`、`Unpin(ctx, key)` | 查看和移除压缩后需要重新注入的内容。 |
| `SideCall(ctx, sr)` | 使用本 session 的提示词和模型发送一次请求，这次请求不写入历史。 |

**任务**（[12](12-tasks.md)）

| 方法 | 说明 |
|---|---|
| `Tasks() *Tasks` | 返回任务管理器，它提供 Start、List、Get、Inspect、Send、Stop、Restart 和 Wait。 |
| `BackgroundCall(ctx, callID)` | 把一个正在前台运行的工具调用转到后台。 |
| `TaskErrorResult(err)` | 在任务启动失败时，生成工具应当返回给模型的结果。 |

**模型与账本**（[4.6](04-models.md#46-运行中切换模型)、[17](17-usage-cost.md)）

| 方法 | 说明 |
|---|---|
| `SwitchModel(ctx, ref)` | 切换模型，从下一轮开始生效。 |
| `TurnCalls(turnID)`、`TurnCost(ctx, turnID)` | 查询这一轮发出了哪些请求，以及花费了多少。 |
| `RecordCalls(bucket, turnID, calls)` | 把在其他地方代替本 session 发出的请求记入账本。 |

**注册与关闭**

| 方法 | 说明 |
|---|---|
| `Registrar() Registrar` | 返回 session 作用域的注册入口，用于在运行中添加工具、命令等，见 [13.1](13-extensions.md#131-extension-与-registrar)。 |
| `Close(ctx)` | 打断正在运行的 turn，等待 worker 退出，然后发出 `session.closed`。 |

## 3.5 创建 session 时的选项

`ag.NewSession(ctx, opts...)` 和 `ag.Resume(ctx, id, opts...)` 接受同一组选项：

| 选项 | 作用 |
|---|---|
| `kernel.WithSessionSink(ch, subOpts...)` | 在创建会话的同时挂上事件 channel，保证能收到 `session.started`。 |
| `kernel.WithSessionFunc(fn, subOpts...)` | 作用与上一项相同，但使用回调函数接收事件。 |
| `kernel.WithLabels(map)` | 为 session 设置标签。kernel 本身不解读标签，由扩展按约定读取，例如 memory 会读取 `user_id` 和 `project_id`。`WithInstructionsFunc` 也能读到这些标签。创建之后可以用 `SetLabels` 修改，见 [16.5](16-storage.md#165-会话标签)。 |
| `kernel.SessionInstructions(text)` | 设置这个 session 的用户指令段，它会覆盖 agent 级的设置。 |
| `kernel.SessionContextWindow(tokens)` | 创建或恢复会话时直接设置它的上下文窗口，作用与 `SetContextWindow` 相同。 |
| `kernel.SessionApprovalRules(rules...)` | 为这个 session 追加审查规则。 |
| `kernel.SessionInputMode(mode)` | 设置这个 session 的默认输入模式。 |
| `kernel.SessionUse(exts...)` | 指定只为这个 session 装配的扩展。 |
| `kernel.ChildOf(rt)` | 在任务中创建子 session 时使用，子 session 的事件和账本会归到该任务名下，见 [12.2](12-tasks.md#122-taskcaps-与-taskruntime)。 |

## 3.6 查看状态

调用 `sess.Snapshot()` 可以随时获取 session 的运行状态。它返回的 `Snapshot` 包含以下字段：

| 字段 | 含义 |
|---|---|
| `Phase` | 当前阶段，取值为 `initializing`、`idle`、`running`、`maintenance`、`closing` 或 `closed`。其中 `maintenance` 表示正在压缩、清空或回退。 |
| `TurnID`、`Step` | 正在运行的 turn，以及它进行到第几个 step。 |
| `Epoch` | 当前 epoch 的编号。 |
| `Model` | 当前模型，包括 API、端点和模型名。 |
| `QueuedInputs` | 排队等待处理的输入数量。 |
| `RunningTasks` | 尚未结束的任务数量，前台任务和后台任务都计算在内。 |
| `Usage` | 累计用量，类型为 `UsageReport`，见 [17.1](17-usage-cost.md#171-用量)。 |
| `Context` | 上下文占用情况，包括已用量、窗口大小和压缩阈值。 |
| `Workers` | 仍在运行的 goroutine 的名字和存活时间。排查 session 为什么无法关闭时，可以查看这个字段。 |

`sess.Info()` 返回的 `SessionInfo` 包含 `SessionID`、`ParentSessionID`、`Agent`（所属 agent 的昵称）、`Labels` 和 `Epoch`。

## 3.7 并发与生命周期

- Session 的所有公开方法都可以在任意 goroutine 中调用。`Send`、`Command`、`Interrupt`、`Snapshot`、`Subscribe` 这类方法从不阻塞。如果需要等待结果，请使用 `Receipt.Wait`、`Run` 或 `WaitQuiet`。
- 等待型方法包括 `Wait`、`Run`、`Compact`、`Clear`、`Rewind`、`Close` 和 `WaitQuiet`。这些方法不能在该 session 正在执行的回调中调用，否则会返回 `ErrReentrantWait`，详见第 [19](19-callback-rules.md) 章。
- 使用完毕后，请调用 `sess.Close(ctx)` 和 `ag.Close(ctx)`。`Close` 会打断正在运行的 turn 并等待 worker 退出，等待时间受 `Limits.CloseTimeout` 限制，默认为 10s。如果超时，`Close` 返回 `*kernel.LeakError`，其中列出了没有退出的 worker。
- `ag.Close` 会先关闭它所有打开的 session，因此程序退出时只调用 `ag.Close` 也可以。

### 建议做法

- 一份配置对应一个 Agent，一段对话对应一个 Session。不要为每个用户创建一个 Agent，而是用 `WithLabels` 区分用户，用 `SessionInstructions` 为个别会话定制指令。
- 建议把 Provider 和 Agent 作为长生命周期对象，在进程启动时创建并在之后复用。
- 先挂订阅，再发送输入。在创建会话时就使用 `WithSessionSink` 或 `WithSessionFunc`，否则会错过 `session.started` 事件。
- 在 Web 服务中，建议每个请求只调用一次 `Send`，拿到 `Receipt` 后交给负责推流的 goroutine 去等待。除非你的 handler 本身就是 SSE，否则不要在 HTTP handler 中直接用 `Run` 运行到结束。

---

[← 上一章：快速开始](02-quickstart.md) · [目录](README.md) · [下一章：接入模型 →](04-models.md)
