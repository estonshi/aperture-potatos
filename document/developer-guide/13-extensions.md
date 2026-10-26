[← 上一章：任务](12-tasks.md) · [目录](README.md) · [下一章：内置扩展 →](14-builtin-extensions.md)

# 13. 扩展机制

扩展用于把你自己的一组能力打包在一起，例如工具、命令、提示词分段和 hook，然后按 agent 或 session 安装。本章介绍扩展的写法、作用域、运行中的注册与撤销，以及内置扩展的安装和关闭方式。读完本章，你可以把一个功能完整地封装成扩展，并决定把它装在哪一级。

本章内容：[13.1 Extension 与 Registrar](#131-extension-与-registrar) · [13.2 作用域](#132-作用域) · [13.3 运行中注册与撤销](#133-运行中注册与撤销) · [13.4 使用 std 安装默认扩展](#134-使用-std-安装默认扩展) · [13.5 扩展的私有状态、事件与 hook 点](#135-扩展的私有状态事件与-hook-点) · [13.6 派生 agent](#136-派生-agent) · [13.7 建议做法](#137-建议做法)

## 13.1 Extension 与 Registrar

SDK 的内置能力和你编写的扩展属于同一个类型，都实现了 `Extension` 接口：

```go
type Extension interface {
	Name() string              // 名字必须唯一，std.Without 按它过滤，"app" 保留给 WithTool 等直接注册的内容
	Install(r Registrar) error // 每个作用域调用一次
}
```

如果不想为扩展单独定义类型，可以用 `kernel.ExtensionFunc(name, install)` 创建扩展。下面的例子在一个扩展中注册了一个工具、一个 hook 和一个 ContextProvider：

```go
repo := kernel.ExtensionFunc("repo", func(r kernel.Registrar) error {
	if _, err := r.Tool(searchTool); err != nil {
		return err
	}
	if _, err := kernel.On(r, kernel.PreToolUse, "repo-guard", guard, kernel.Match(kernel.MatchNames("repo_write"))); err != nil {
		return err
	}
	_, err := r.ContextProvider(kernel.ContextProvider{Name: "repo", When: kernel.AtTurnStart, Provide: provide})
	return err
})
```

`Install` 收到的 `Registrar` 提供以下方法：

| 方法 | 注册的内容 |
|---|---|
| `Tool(t)` | 注册工具，详见 [7](07-tools.md)。 |
| `HostedTool(t)` | 注册服务端工具，详见 [4.7](04-models.md#47-服务端工具)。 |
| `Command(c)` | 注册命令，详见 [10](10-commands.md)。 |
| `PromptSection(ps)` | 注册系统提示词分段，详见 [11.3](11-prompt.md#113-扩展分段)。 |
| `ContextProvider(p)` | 注册每轮动态内容，详见 [11.4](11-prompt.md#114-每轮动态内容)。 |
| `Compactor(c)` | 注册压缩策略，每个作用域只能有一个，详见 [15.6](15-context.md#156-自定义压缩策略)。 |
| `TaskKind(k)` | 注册任务类型，详见 [12](12-tasks.md)。 |
| `ApprovalRule(r)` | 注册审批规则，从下一次 gate 起生效，详见 [8.1](08-approval.md#81-规则)。 |
| `Hide(kind, name)` | 在本作用域中遮蔽继承来的注册。`kind` 可取 `KindTool`、`KindCommand`、`KindPromptSection`、`KindContextProvider`、`KindHostedTool`。`KindCompactor` 和 `KindTaskKind` 用同名注册来遮蔽，`Hide` 对它们返回错误；`KindApprovalRule` 也不能隐藏，因为隐藏外层的 `deny` 会让这个 session 绕过 agent 的策略。 |
| `Scope()`、`Agent()`、`Session()` | 分别返回当前作用域、所属 agent 和所属 session。在 agent 级时，`Session()` 返回 nil。 |

hook 通过 `kernel.On(r, ...)` 或 `kernel.OnAsync(r, ...)` 注册，详见 [9.1](09-hooks.md#91-注册)。

每次注册都会返回一个 `*kernel.Registration`，调用它的 `Remove()` 即可撤销这次注册，`Remove()` 可以重复调用。如果扩展自己管理着某些资源，例如一个连接，并且希望它们也纳入统一的撤销流程，可以用 `kernel.NewRegistration(func() { ... })` 把释放逻辑包装成一个注册。

## 13.2 作用域

扩展可以安装在 agent 级或 session 级。两种作用域的区别如下：

| 作用域 | 安装方式 | 说明 |
|---|---|---|
| agent | `kernel.Use(exts...)`、`std.Defaults(...)` | 由这个 agent 的所有 session 共享，`Install` 在 `kernel.New` 时调用一次。 |
| session | `ag.NewSession(ctx, kernel.SessionUse(exts...))` | 只对这个 session 生效，`Install` 在创建会话时调用。同名的注册会遮蔽 agent 级的注册。 |

通过 `kernel.WithTool`、`WithCommand`、`WithHook` 等直接选项注册的内容，归属于一个名为 `app` 的 agent 级扩展。因此 `app` 是保留名字，你自己的扩展不要使用它。

## 13.3 运行中注册与撤销

除了在安装扩展时注册，你还可以在会话运行期间动态地添加或撤销能力。`sess.Registrar()` 返回 session 作用域的 Registrar，用法与 `Install` 中的 Registrar 相同：

```go
reg, err := sess.Registrar().Tool(t)
// ...
reg.Remove()
```

工具和服务端工具的变化从下一轮开始生效；对于声明了 `ThinkingPrefixBound` 的模型，从下一个 epoch 开始生效。系统提示词分段从下一个 epoch 开始生效，审批规则从下一次 gate 开始生效，命令、动态内容、压缩策略和任务类型立即生效。每次变化都会发出 `registry.changed` 事件，事件包含 `Scope`、`Kind`、`Name`、`Op` 和 `Effective` 字段，其中 `Effective` 的取值为 `now`、`turn` 或 `epoch`。在一轮之内，已经冻结的工具定义不会改变。如果工具的定义本身发生了变化，即 `Spec()` 返回了不同的内容，请调用 `kernel.ToolChanged(r, name)` 发出通知。

## 13.4 使用 std 安装默认扩展

`std` 包用一个选项安装 SDK 的默认扩展集合，并允许你在此基础上追加、替换或去掉其中的扩展：

```go
kernel.New(
	std.Defaults(
		std.With(skills.New(skills.Options{Sources: []skills.Source{skills.FS(os.DirFS("./skills"))}})),
		std.Without("subagent"),
	),
	...
)
```

`std` 包提供以下函数：

| 函数 | 作用 |
|---|---|
| `std.Defaults(opts...)` | 安装默认集合，返回一个 `kernel.Option`。 |
| `std.With(exts...)` | 追加扩展，或替换同名的默认扩展。 |
| `std.Without(names...)` | 去掉指定的默认扩展。 |
| `std.Extensions(opts...)` | 返回扩展清单，由你自己安装，例如安装到 session 级。 |

默认集合包括 `compact`、`history`、`tasks`、`subagent`，以及 `cmd.help`、`cmd.usage`、`cmd.compact`、`cmd.clear`、`cmd.rewind`、`cmd.fork`、`cmd.context` 七个命令。skills、memory、peer 和 mcp 需要配置，因此不在默认集合中，需要用 `std.With` 加入。

## 13.5 扩展的私有状态、事件与 hook 点

扩展除了注册能力，还可以保存自己的状态、定义自己的事件和 hook 点。

**私有状态**。扩展可以把私有状态挂在 agent 或 session 上。这种状态是类型安全的，但只保存在内存中，恢复的 session 中不会有这些状态。

```go
var counterKey = kernel.NewKey[int]("repo.counter")

kernel.Set(sess, counterKey, 0)
n, ok := kernel.Get(sess, counterKey)
kernel.Update(sess, counterKey, func(old int, ok bool) int { return old + 1 }) // 读取和修改在一次原子操作中完成
```

**事件**。要定义扩展自己的事件，需要定义实现了 `EventType() event.Type` 的负载类型，然后用 `tc.Emit`、`sess.Emit` 或 `rt.Emit` 发出。扩展还应提供一个 `Payloads() []event.Constructor`，让跨进程的解码器能够识别这些负载类型，详见 [6.7](06-events.md#67-跨进程传递)。

**hook 点**。扩展可以用 `kernel.NewHookPoint(kernel.HookPointDef[In, Out]{...})` 定义自己的 hook 点，并用 `kernel.Fire(ctx, sess, point, in)` 触发它，详见 [9.7](09-hooks.md#97-扩展定义的-hook-点)。

**日志**。扩展应通过 `r.Agent().Logger()` 记录日志，这样告警会和宿主的日志输出到同一个地方。

## 13.6 派生 agent

派生 agent 是在已有 agent 的基础上创建一个新 agent，它可以拥有自己的身份、指令、模型和工具范围，其余配置从父 agent 继承：

```go
child, err := ag.Derive(kernel.DeriveOptions{
	Nickname:   "Researcher",
	ToolFilter: func(s kernel.ToolSpec) bool { return s.Name != "book_hotel" },
})
```

`DeriveOptions` 的字段如下。除 `InheritHooks` 外，字段取零值时表示该项沿用父 agent 的设置。`InheritHooks` 的零值是 false，表示不继承父 agent 的 hook。

| 字段 | 说明 |
|---|---|
| `Nickname`、`Description` | 派生 agent 自己的身份。 |
| `Instructions *string` | 派生 agent 自己的指令段。 |
| `Model *provider.ModelRef` | 派生 agent 自己的模型。 |
| `ToolFilter` | 决定保留哪些工具。服务端工具也会经过这个过滤函数，但只带名字。 |
| `InheritHooks` | 决定是否继承父 agent 的 hook。审查规则和审查者**总是会被继承**，因此派生出来的 agent 无法绕开审查。 |
| `Use` | 额外安装的扩展。 |
| `Limits` | 派生 agent 自己的上限，取零值的字段使用父 agent 的设置。 |

派生的 agent 以只读层的方式共享父 agent 的注册，父 agent 之后发生的变化它也能看到。subagent 扩展就是用这种方式为每种 subagent 创建 agent 的。

`ToolFilter` 不是派生时的快照：它每次对父 agent 的实时注册视图求值，父 agent 之后新增的工具也会被它过滤。它属于派生 agent 级，构造后不能更改；判断依据是 `ToolSpec`（托管工具只有名字），也不能只按名字隐藏某一次注册。如果需要在运行期按名字遮蔽某个工具，用 session 的 `sess.Registrar().Hide(kernel.KindTool, name)`：它作用在 scope 级，可以 `Remove()` 撤销，会发出 `registry.changed`，并且只遮蔽继承来的注册。

## 13.7 建议做法

- 建议一个功能对应一个扩展。把工具、它的提示词分段和它的 hook 放在同一个 `Install` 中，关闭这个功能时只需去掉一行 `std.With`。
- `Name()` 应使用简短的小写名字，并且不与内置扩展重名。命令类扩展沿用 `cmd.<name>` 的命名习惯。
- `Install` 必须能被多次调用，因为它在 agent 级调用一次，在每个 session 级各调用一次。不要在其中放只能初始化一次的逻辑，需要共享的连接应在 `New` 时创建好，`Install` 只负责注册。
- 按用户开关的能力适合安装在 session 级。你可以使用 `kernel.SessionUse(ext)`，也可以在运行中通过 `sess.Registrar()` 注册。
- 扩展需要持久保存的状态应由扩展自己存储。`kernel.Key` 只保存在内存中，需要跨重启保留的状态，请使用 `Pin`、`SaveState` 或你自己的存储。

---

[← 上一章：任务](12-tasks.md) · [目录](README.md) · [下一章：内置扩展 →](14-builtin-extensions.md)
