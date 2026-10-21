[← 上一章：Hook](09-hooks.md) · [目录](README.md) · [下一章：系统提示词与动态上下文 →](11-prompt.md)

# 10. 命令

命令为用户提供 `/xxx` 形式的快捷操作。你可以把常用的提示做成模板命令，也可以注册一个执行宿主逻辑的自定义命令。本章介绍这两种命令的写法、命令与正在运行的 turn 之间如何协调，以及 SDK 内置的命令。读完本章，你可以为自己的应用添加命令，并为每个命令选择合适的忙时策略。

本章内容：[10.1 模板命令](#101-模板命令) · [10.2 自定义命令](#102-自定义命令) · [10.3 忙时策略](#103-忙时策略) · [10.4 调用、拦截与事件](#104-调用拦截与事件) · [10.5 内置命令](#105-内置命令) · [10.6 建议做法](#106-建议做法)

## 10.1 模板命令

模板命令把一段固定的提示词和用户给出的参数拼成一条输入，适合把用户经常提出的请求做成快捷方式。用 `kernel.PromptCommand` 创建模板命令，再用 `kernel.WithCommand` 注册：

```go
kernel.WithCommand(kernel.PromptCommand("plan", "Plan a trip", "Plan a trip: $ARGUMENTS. Check the visa rules first."))
```

用户发送 `/plan 东京三日游` 时，模板会展开成一条普通输入，这条输入的来源为 `command`，模型照常回答。

模板中可以使用以下占位符：

| 写法 | 含义 |
|---|---|
| `$ARGUMENTS` | 替换为全部参数的原文。 |
| `$ARGUMENTS[N]`、`$N` | 替换为第 N 个参数，序号从 0 开始。 |
| `$name` | 替换为通过 `kernel.WithArgs("dest", "days")` 命名的位置参数。 |
| `$$` | 表示字面的 `$`。 |
| 没有任何占位符 | 参数会接在模板末尾。 |

参数按 shell 的方式切分。参数之间以空白分隔，单引号内的内容原样保留，双引号内支持 `\"` 和 `\\` 两种转义，引号之外的反斜杠会转义紧随其后的一个字符。`kernel.SplitArgs(s)` 就是这个切分函数，自定义命令也可以直接调用它。

`PromptCommand` 支持两个选项。`kernel.WithArgs(names...)` 为位置参数命名，`kernel.PromptBusy(policy)` 设置忙时策略，默认值为 `WaitIdle`。

## 10.2 自定义命令

如果命令需要执行宿主自己的逻辑，而不只是给模型发一段提示，可以直接构造 `kernel.Command`。下面的例子注册了一个查看会话状态的命令：

```go
kernel.WithCommand(kernel.Command{
	Name:        "status",
	Aliases:     []string{"st"},
	Description: "Show the session status",
	Busy:        kernel.RunNow,
	Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
		snap := cc.Session().Snapshot()
		return kernel.CommandResult{
			Text: fmt.Sprintf("phase=%s epoch=%d", snap.Phase, snap.Epoch),
			Data: snap,
		}, nil
	},
})
```

`Command` 的字段如下：

| 字段 | 说明 |
|---|---|
| `Name`、`Aliases` | 命令的名字和别名。 |
| `Description`、`ArgHint` | 给人看的说明和参数提示，`/help` 会列出它们，例如 `/compact [instructions]`。 |
| `Busy` | 决定有 turn 在运行时如何处理这个命令，详见 [10.3](#103-忙时策略)。 |
| `Hidden` | 设置后，命令不会出现在 `Commands()` 和 `session.started` 中。 |
| `Run` | 命令的执行逻辑。 |

`Run` 收到的 `CommandContext` 提供以下方法。`Name()` 返回命令名，`Args()` 返回原始参数字符串，`ID()` 返回这次执行的 id，`Session()` 返回所属的 session。`Output(text)` 用于在执行过程中发出 `command.output` 事件，耗时较长的命令可以用它报告进度。

`Run` 返回的 `CommandResult` 包含以下字段：

| 字段 | 说明 |
|---|---|
| `Text` | 给人看的文本，写入 `command.completed.Text`。 |
| `Data` | 结构化结果，写入 `command.completed.Data`，同时出现在 `TurnResult.Command.Data` 中。 |
| `Prompt *Input` | 非空时，这条输入会交给模型处理，来源为 `command`。模板命令就是通过这个字段实现的。 |
| `Pin` | 加入 Pin 集合，压缩之后会重新注入。 |

## 10.3 忙时策略

用户可能在模型仍在工作时发送命令。命令的 `Busy` 字段决定这种情况下命令如何执行，你需要根据命令是否会修改历史来选择策略。

| `Busy` | 有 turn 在运行时的行为 |
|---|---|
| `WaitIdle`（默认） | 命令排队，等前面的 turn 结束后再执行。它对之后到达的输入构成屏障，并且可以修改历史。`/compact` 和 `/clear` 使用这个策略。 |
| `RunNow` | 命令与 turn 并发执行，**不能修改历史**，适合只读的查询。`/usage` 和 `/context` 使用这个策略。 |
| `InterruptFirst` | 先打断当前 turn，再执行命令。 |
| `RejectBusy` | 不执行命令，立即以 `command.completed{error:"busy"}` 结束，回执中 `TurnResult.Err` 为 `ErrBusy`。 |

## 10.4 调用、拦截与事件

命令有两种调用方式。用户输入的 `/name args` 通过 `sess.Send(kernel.Text("/name args"))` 发送，由 kernel 解析后执行。程序直接执行命令时使用 `sess.Command("name", "args")`，这种方式不做文本解析。两种方式都会返回回执，调用 `Wait` 之后，`TurnResult.Outcome` 为 `command`，命令的结果在 `TurnResult.Command` 中。

两种方式在命令名不存在时的处理不同。`sess.Command()` 遇到未注册的命令名会返回 `ErrUnknownCommand`。`Send` 只有在 `/` 后面是已注册的命令名或别名时才把文本当作命令，否则会把它作为普通文本发给模型。

命令执行前会触发 `command_invoke` hook，hook 可以阻断这次执行或改写参数，详见 [9.4](09-hooks.md#94-每个点的输入与输出)。

命令执行过程中会依次发出以下事件。`command.started` 带有 `Name`、`Args` 和 `Busy`，`command.output` 对应 `Output(text)` 的输出，`command.completed` 带有 `Text`、`Data` 和 `Error`。模板命令展开出的输入照常经过 `input.accepted` 和 `turn.started`。

如果 UI 需要做命令自动补全，可以调用 `sess.Commands()` 列出可用命令。它返回 `CommandInfo` 列表，每项包含 `Name`、`Aliases`、`Description`、`ArgHint` 和 `Busy`。session 级注册的命令会遮蔽 agent 级的同名命令。

## 10.5 内置命令

`std.Defaults()` 会安装七个命令。每个命令都是一个独立的扩展，你可以用 `std.Without("cmd.<name>")` 去掉其中任何一个。

| 命令 | 扩展名 | 作用 | 忙时策略 |
|---|---|---|---|
| `/help` | `cmd.help` | 列出可用命令。 | RunNow |
| `/usage` | `cmd.usage` | 显示 token 用量与上下文大小。`Data` 是 `usagecmd.Report`，包含用量、上下文和缓存命中率。 | RunNow |
| `/context` | `cmd.context` | 显示上下文的组成。 | RunNow |
| `/compact [instructions]` | `cmd.compact` | 压缩上下文，可以附带对摘要的说明。 | WaitIdle |
| `/clear` | `cmd.clear` | 清空上下文。 | WaitIdle。使用 `clearcmd.New(clearcmd.Options{Interrupt: true})` 可以改为先打断。 |
| `/rewind [n]` | `cmd.rewind` | 撤销最近 n 轮，默认撤销 1 轮。 | WaitIdle。使用 `rewindcmd.Options{Interrupt: true}` 可以改为先打断。 |
| `/fork [n]` | `cmd.fork` | 把上下文复制到一个新 session，可以去掉最后 n 轮。`Data` 是 `forkcmd.Forked{SessionID}`，宿主用 `Resume` 打开新 session。 | RunNow |

如果要替换某个默认实例，可以把新实例通过 `std.With` 传入，例如 `std.Defaults(std.With(clearcmd.New(clearcmd.Options{Interrupt: true})))`。

除了这七个命令，其他扩展也会注册命令。skills 扩展会把每个 skill 注册成 `/<skill名>`，mcp 扩展可以把服务器提供的 prompts 注册成 `/mcp__<server>__<prompt>`。

## 10.6 建议做法

- 只读的查询命令建议使用 `RunNow`。这样用户在模型工作时也能立刻看到结果。
- 会修改历史的命令应使用 `WaitIdle`，明确需要立即生效时使用 `InterruptFirst`。不要在 `RunNow` 的命令中调用 `Compact`、`Clear` 或 `Rewind`。
- 在命令中调用等待型方法时，需要注意重入问题。`WaitIdle` 的命令运行在 session 自己的执行路径上，可以直接调用 `Compact` 这类等待型方法。`RunNow` 的命令是并发执行的，其中调用的等待型方法会排队，详见 [19](19-callback-rules.md)。
- 建议把用户经常提出的问题做成模板命令。与把这些内容常驻在系统提示词里相比，这样更节省 token。
- 内部使用的命令应设置 `Hidden`。例如由 UI 自己触发的 `/_sync` 就不需要展示给用户。

---

[← 上一章：Hook](09-hooks.md) · [目录](README.md) · [下一章：系统提示词与动态上下文 →](11-prompt.md)
