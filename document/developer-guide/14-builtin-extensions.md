[← 上一章：扩展机制](13-extensions.md) · [目录](README.md) · [下一章：上下文管理 →](15-context.md)

# 14. 内置扩展

SDK 自带一组扩展，覆盖压缩、历史检索、任务管理、subagent、skills、长期记忆、调用其他 agent、连接 MCP 服务器、斜杠命令和 LiteLLM 费用查询。本章逐个介绍这些扩展向模型提供了哪些能力、如何配置，以及使用时需要注意的地方。读完本章，你可以判断哪些能力直接复用内置扩展即可，不必自己实现。

本章内容：[14.1 ext/compact](#141-extcompact) · [14.2 ext/history](#142-exthistory) · [14.3 ext/tasks](#143-exttasks) · [14.4 ext/subagent](#144-extsubagent) · [14.5 ext/skills](#145-extskills) · [14.6 ext/memory](#146-extmemory) · [14.7 ext/peer](#147-extpeer) · [14.8 ext/mcp](#148-extmcp) · [14.9 ext/cmd](#149-extcmd) · [14.10 ext/litellm](#1410-extlitellm)

下表列出各个内置扩展的包路径、扩展名、是否默认安装，以及它向模型提供的内容。“默认安装”指 `std.Defaults()` 是否已经包含该扩展。

| 包 | 扩展名 | 默认安装 | 向模型提供的内容 |
|---|---|---|---|
| `ext/compact` | `compact` | 是 | 提供压缩策略，模型不会直接看到它。 |
| `ext/history` | `history` | 是 | 提供 `search_history` 工具。 |
| `ext/tasks` | `tasks` | 是 | 提供 `task_status`、`task_send`、`task_stop` 和 `task_restart` 四个工具。 |
| `ext/subagent` | `subagent` | 是 | 提供 `subagent` 工具，并在子 agent 中提供 `ask_parent`。 |
| `ext/skills` | `skills` | 否 | 提供 `skill` 工具、`<available_skills>` 目录和 `/<skill>` 命令。 |
| `ext/memory` | `memory` | 否 | 把指令文件与记忆写进每个 epoch 的开头，并提供 `memory_save` 和 `memory_forget` 工具。 |
| `ext/peer` | `peer` | 否 | 提供 `a2a_send` 工具和 `<available_agents>` 目录。 |
| `ext/mcp` | `mcp` | 否 | 提供服务器的工具、`mcp_search_tools` 和 `mcp_call_tool`，以及由 prompts 生成的命令。 |
| `ext/cmd/*` | `cmd.<name>` | 是 | 提供七个斜杠命令。 |
| `ext/litellm` | 不是扩展 | 不适用 | 为请求打标，并作为费用来源。 |

## 14.1 ext/compact

`ext/compact` 是默认的压缩策略。压缩时，它让模型为当前上下文写一份摘要。你还可以让它在调用模型之前先做一轮成本较低的清理，即把旧的工具结果替换成占位符，如果清理后的上下文已经足够小，就不再调用模型。

```go
std.Defaults(std.With(compact.New(compact.Options{
	SummaryPrompt:   "...",                                   // 替换默认的摘要提示词
	DropCurrentTurn: false,                                   // 设为 true 时，压缩会把正在运行的这一轮也纳入摘要
	Prune:           &compact.PruneOptions{KeepRecent: 4, MinTokens: 2000}, // 先清理旧工具结果，清理后足够小就不再调用模型
})))
```

`Options` 的字段如下。

| 字段 | 说明 |
|---|---|
| `SummaryPrompt` | 摘要使用的提示词。默认提示词位于 `spec/ext/compact/summary.md`。 |
| `DropCurrentTurn` | 默认情况下，正在运行的 turn 保留原样，不参与压缩。 |
| `Prune` | 清理旧工具结果的配置。`KeepRecent` 表示最近几个工具结果不清理，设为 0 时使用默认值，设为负数时全部清理。`MinTokens` 表示小于该 token 数的工具结果不清理。 |

被清理的工具结果在事件和历史中的 `Cleared` 字段为 true。原文仍然保存在存储中，模型可以用 `search_history` 找回。

## 14.2 ext/history

`ext/history` 向模型提供 `search_history` 工具，用来检索本会话的历史，包括已经被压缩掉的部分。该工具接受 `query`、`seq`、`parent` 和 `limit` 四个参数。`query` 用于按关键词检索，`seq` 用于读取某一行的全文。在 subagent 中，传入 `parent: true` 可以检索父会话当前的上下文。旁路对话的每一轮已经带着主会话当前的上下文，在旁路对话中传入 `parent: true` 会得到一条错误。

宿主程序可以用 `sess.SearchHistory` 和 `sess.HistoryLine` 完成同样的检索，详见 [15.5](15-context.md#155-检索历史)。

`history.New()` 没有配置项。

## 14.3 ext/tasks

`ext/tasks` 向模型提供四个任务管理工具。这些工具操作的是与 `sess.Tasks()` 相同的任务管理器，因此模型和宿主程序看到的是同一组任务。

| 工具 | 参数 |
|---|---|
| `task_status` | `task_id` 为空时列出全部任务。`include_result` 控制是否返回结果，`wait_ms` 表示等待一段时间后再返回。 |
| `task_send` | 接受 `task_id`、`message` 和 `mode`，其中 `mode` 取 `next_loop`、`queue` 或 `interrupt`。 |
| `task_stop` | 接受 `task_id` 和 `reason`。 |
| `task_restart` | 接受 `task_id` 和可选的新输入 `input`。 |

除了这四个工具，该扩展还会在系统提示词中加入一段任务使用说明，并在压缩后把尚未结束的任务列给模型。`tasks.New()` 没有配置项。

## 14.4 ext/subagent

`ext/subagent` 让模型把一项工作交给一个派生出来的 agent 完成，下文称之为子 agent。子 agent 在自己的子 session 中运行，模型可以等待它完成，也可以把它放到后台。该扩展的扩展名、工具名和任务类型都是 `subagent`。

```go
std.Defaults(std.With(subagent.New(subagent.Options{
	Defs: []subagent.Def{{
		Name:         "researcher",
		Description:  "Finds facts on the web and reports with sources.",
		Instructions: "You are a research assistant. ...",
		Tools:        []string{"web_fetch", "search_history"}, // 为 nil 时继承父 session 的全部工具，为空切片时不提供任何工具
		Model:        &provider.ModelRef{Provider: cheap, Model: "claude-haiku-5-5"},
		MaxSteps:     30,
	}},
	MaxConcurrent: 4,
})))
```

`Def` 描述一种子 agent 类型，字段如下。

| 字段 | 说明 |
|---|---|
| `Name`、`Description` | 类型名和说明，模型根据它们选择使用哪种子 agent。 |
| `Instructions` | 子 agent 的指令段。 |
| `Tools`、`DenyTools` | 分别指定允许和禁止的工具。 |
| `Model` | 子 agent 使用的模型。为 nil 时使用父 session 当前的模型。 |
| `FixedModel` | 为 true 时忽略调用里的 `model` 和 `effort`，即使 `Model` 为 nil 也始终跟随父 session 的模型。 |
| `MaxSteps` | 子 session 每一轮最多执行的 step 数，默认为 50。 |
| `AskTimeout` | `ask_parent` 等待回答的时长。为 0 时一直等待。 |

`Options.MaxConcurrent` 限制一个 session 中同时运行的 subagent 数量，默认为 8。`Options.Models` 和 `Options.Efforts` 声明调用时可选的模型档和思考强度，会写进工具 schema 的枚举；为空时对应参数不出现。`Options.ResolveModel` 把 `Models` 里的名字解析成 `provider.ModelRef`；`inherit` 不需要解析，直接用父 session 的模型。如果需要在运行中新增子 agent 类型，可以调用 `subagent.Define(r, def)`。在 session 级定义的类型只对该 session 可见。

模型调用 `subagent` 工具时传入以下参数：`agent_type` 指定类型，`description` 是 3 到 8 个词的摘要，`prompt` 是完整的指令和上下文，`background` 决定是否放到后台运行；`model` 和 `effort` 可选，从宿主声明的枚举里选，`model: inherit` 表示跟随父 session。子 agent 看不到父会话，因此 `prompt` 中需要写全它所需的信息。子 agent 可以用 `search_history{parent:true}` 查询父会话，也可以用 `ask_parent{question}` 向父级提问。提问后，任务进入等待输入的状态，父级（模型或宿主程序）用 `task_send` 回答。子 agent 不能再嵌套使用 subagent。

该扩展定义了两个 hook 点。`subagent.Start` 可以阻断启动、修改 prompt、选择 model / effort，或为子 session 追加上下文。`subagent.Stop` 可以让子 session 继续运行。详见 [9.7](09-hooks.md#97-扩展定义的-hook-点)。子 session 发出的事件带有 `Path` 字段，其中第一跳的 `Kind` 为 `task`，`Name` 为 `subagent`。

## 14.5 ext/skills

`ext/skills` 支持 Agent Skills，即以 `<name>/SKILL.md` 形式组织的目录。该扩展把可用的 skill 列进系统提示词。模型通过 `skill` 工具加载其中一个，用户通过 `/<name>` 命令加载。加载过的 skill 会被 Pin 住，压缩之后仍然保留在上下文中。

```go
std.Defaults(std.With(skills.New(skills.Options{
	Sources: []skills.Source{
		skills.FS(os.DirFS("./skills")),
		skills.FS(embeddedSkills, skills.WithLogger(logger)),
	},
})))
```

`Options` 的字段如下。

| 字段 | 说明 |
|---|---|
| `Sources` | skill 来源列表，排在前面的来源优先。也可以先用 `skills.Merge(sources...)` 把多个来源合并成一个。 |
| `MaxDescriptionChars` | 目录中每个 skill 描述的字符上限，默认为 1536。 |
| `PinTokens` | 每个已加载的 skill 被 Pin 住的 token 数，默认为 5000。 |
| `Rewrite` | 可选的正文改写器。它收到 `skills.RewriteInput`（会话、skill 名、触发方、参数和展开后的正文），返回最终正文或 error；返回 error 时这次加载以 `denied` 文案失败。模型路径在判重前调用它，所以重复加载也会拿到最新正文。 |

`skills.FS(fsys, opts...)` 扫描一个 `embed.FS` 或 `os.DirFS` 作为来源。它接受两个选项。`WithFrontmatterParser(fn)` 用于换成一个完整的 YAML 解析器，默认解析器只识别 Agent Skills 用到的子集。`WithLogger(l)` 用于接收 skill 被宽松加载或被跳过时的告警。如果 skill 存放在数据库或远端服务中，你需要自己实现 `skills.Source` 接口，它包含 `List`、`Load` 和 `Open` 三个方法。宿主自己实现的来源不受 Agent Skills 命名规则约束，名字里可以有 `:`，能通过命令名规则时照常注册 `/<name>` 命令。运行中新增来源可以调用 `skills.AddSource(r, src)`。

`Rewrite` 用于在正文发给模型前做宿主侧处理，例如运行正文里的 `` !`命令` ``、按会话的权限规则检查、把输出替换进正文。用户 `/name` 和模型调 `skill` 工具都会经过它。模型路径的 `skill` 工具还接受可选的 `args`：传了 `args` 时，正文里的 `$ARGUMENTS`、`$N`、`$$` 会按提示词命令的规则展开；不传时正文原样加载。

`Frontmatter` 包含以下字段：`Name`、`Description`、`License`、`Compatibility`、`Metadata`、`AllowedTools`、`UserInvocable`、`ModelInvocable`、`Extra` 和 `Location`。其中 `AllowedTools` 会被解析，但不会被强制执行。`UserInvocable` 和 `ModelInvocable` 默认都为 true。`Extra` 原样保留其他厂商定义的字段。

该扩展会发出两个事件。`skills.Activated` 记录谁加载了哪个 skill，以及估计占用多少 token。`skills.CatalogChanged` 表示目录发生了变化。hook 点 `skills.Activate` 可以阻断加载。

## 14.6 ext/memory

`ext/memory` 为 agent 提供长期记忆。它把指令文件（例如 `AGENTS.md`）以及用户和项目的长期记忆写进每个 epoch 的开头。模型可以用 `memory_save` 和 `memory_forget` 修改记忆。在会话结束或压缩时，该扩展会让模型把值得记住的内容整理进记忆。

```go
std.Defaults(std.With(memory.New(memory.Options{
	Instructions: memory.Files(os.DirFS("."), "AGENTS.md", ".agent/instructions.md"),
	Store:        myStore, // 实现 memory.Store 接口，测试时可以使用 memory.NewInMemory()
})))

ag.NewSession(ctx, kernel.WithLabels(map[string]string{"user_id": "u_42", "project_id": "acme/web"}))
```

`Options` 的字段如下。

| 字段 | 说明 |
|---|---|
| `Instructions` | 指令来源。`memory.Files(fsys, names...)` 按顺序读取文件，跳过不存在的文件，总量上限为 25KB。如需自定义来源，实现 `InstructionSource` 接口。 |
| `Store` | 记忆存储，需要实现 `List(ctx, scope)` 和 `Apply(ctx, scope, changes)` 两个方法，其中 `Apply` 必须保证全部成功或全部失败。如果存储还实现了 `Ingester`，它可以自己从对话中提取记忆。 |
| `ScopeFunc` | 根据 session 计算 `Scope{User, Project, Agent, Session}`。默认取 `Labels["user_id"]` 和 `Labels["project_id"]`。 |
| `ConsolidateOn` | 指定何时整理记忆，默认为 `{"pre_compact"}`。如果加入 `"session_end"`，需要把 `Limits.CloseTimeout` 调到足够完成一次模型调用，或者在调用 `Close` 之前自己调用 `memory.Consolidate(ctx, sess)`。 |
| `UserTokens`、`SharedTokens` | 分别限制带用户标签的记忆和不带用户标签的记忆所占的 token 数，后者对所有用户共享。两者默认都为 10000。 |
| `NoTools` | 设为 true 时不向模型提供 `memory_save` 和 `memory_forget`，记忆对模型只读。 |
| `SharedWrites` | 设为 true 时允许模型新增、改写、删除所有用户共享的记忆（不带用户标签的记忆）。默认关闭：模型的写入只落在当前 session 的用户下，共享记忆只能由你经 `Store.Apply` 维护，避免一次 prompt injection 影响所有用户。 |

`memory_save` 接受四个参数。`content` 是一句不超过 40 个词的话。`user` 和 `project` 指定记忆的作用范围，只能为空或者等于当前 session 的标签。`project` 为空时对所有项目生效。`user` 为空时，如果 session 带有用户标签且没有开启 `SharedWrites`，会替换为当前 session 的用户，否则对所有用户生效。`replaces` 指定要改写的那条记忆；在同样的条件下，改写共享记忆会返回 `shared_read_only` 错误。

该扩展会发出两个事件。`memory.Loaded` 记录本 epoch 写入了哪些记忆，`memory.Saved` 记录改了什么以及由谁触发。

## 14.7 ext/peer

`ext/peer` 让模型给另一个 agent 发消息。对方可以是同一进程中的另一个 `*kernel.Agent`，也可以是远端的 A2A 服务。每次交流都是一个 `a2a` 类型的任务，模型可以等待它完成，也可以放到后台。

```go
remote, err := a2a.NewRemotePeer(ctx, "https://research.example.com", a2a.WithHeaders(map[string]string{"Authorization": "Bearer " + token}))
local := peer.Local(otherAgent, peer.IdleClose(10*time.Minute), peer.MaxSessions(100))

std.Defaults(std.With(peer.New(remote, peer.Named(local, "writer"))))
```

下表列出构造和注册 peer 的函数。

| 函数 | 说明 |
|---|---|
| `a2a.NewRemotePeer(ctx, baseURL, opts...)` | 获取对方的 agent card 并选定接口，支持 JSON-RPC 或 HTTP+JSON，协议版本为 v0.3 或 v1.0。可用选项有 `WithHeaders`、`WithDoer`、`WithBinding`、`WithProtocolVersion` 和 `WithAllowedHosts`。`WithDoer` 用于自定义 HTTP 客户端，鉴权和签名逻辑可以放在这里。卡片里的接口地址默认必须与 `baseURL` 同主机（`baseURL` 是 https 时接口也必须是 https），因为 `WithHeaders` 的凭据会随每个请求发出；接口在别的主机上时用 `WithAllowedHosts("api.example.com")` 放行，否则返回 `a2a.ErrInterfaceHost`。 |
| `peer.Local(agent, opts...)` | 包装进程内的 agent，不经过序列化，每个对话对应它的一个 session。`IdleClose(d)` 指定会话空闲多久后关闭，配置了存储时，下次收到消息会恢复该会话。`MaxSessions(n)` 限制最多同时打开的会话数。 |
| `peer.Named(p, name)` | 指定模型使用的名字。当 agent card 中的名字不符合 `[a-zA-Z0-9_-]` 时需要使用。 |
| `peer.Register(r, p)` | 在运行中新增 peer。 |

模型调用 `a2a_send` 时传入以下参数：`agent` 指定对方，`message` 是要发送的消息，`new_conversation` 表示另起一段对话，`background` 决定是否放到后台。对方看不到本对话，因此 `message` 中需要写全所需的信息。

如果对方要求鉴权，会触发 `peer.AuthRequired` hook，并发出 `peer.auth_required` 事件。你在 hook 中修复凭据后返回 `Retry: true`，即可重试。

如果要接入自己的传输层，实现 `peer.Peer` 接口即可，它包含 `Card`、`Send`、`Subscribe` 和 `Cancel` 四个方法。

## 14.8 ext/mcp

`ext/mcp` 用于连接 MCP 服务器，并把服务器提供的工具交给模型使用。它支持 stdio 和 Streamable HTTP 两种传输方式，支持 OAuth 2.1 和 elicitation（服务器在调用过程中向用户索取输入），还可以把服务器的 prompts 注册成斜杠命令。它不在默认扩展中，需要用 `kernel.Use` 或 `kernel.SessionUse` 自己装配。

```go
m := mcp.New(mcp.Options{
	Servers: []mcp.Server{
		{Name: "fs", Command: func() *exec.Cmd {
			return exec.Command("npx", "-y", "@modelcontextprotocol/server-filesystem", "/data")
		}},
		{Name: "jira", URL: "https://mcp.example.com/jira", OAuth: &mcp.OAuth{
			ClientMetadataURL: "https://app.example.com/oauth/client.json",
			RedirectURL:       "https://app.example.com/oauth/callback",
			Authorize:         authorizer.Authorize, // 可以使用 mcp.NewCallbackAuthorizer() 或 mcp.NewLoopbackAuthorizer(nil)
			Tokens:            tokenStore,           // 实现 mcp.TokenStore 接口，在进程重启之间保存授权
		}, Prompts: true},
	},
})
defer m.Close(ctx)

kernel.New(std.Defaults(std.With(m)), kernel.WithApprovalRules(mcp.ApprovalRules(rules...)...), ...)
```

`mcp.Server` 描述一个服务器，字段如下。

| 字段 | 说明 |
|---|---|
| `Name` | 服务器名，必须匹配 `^[A-Za-z0-9_-]{1,64}$`，区分大小写，同一个安装内不能重复。它以原样出现在工具名中，格式为 `mcp__<server>__<tool>`；它的 prompts 注册成命令时转为小写，即 `/mcp__<server>__<prompt>`。名字里带 `__` 时，`mcp__<server>__*` 这类按名字前缀写的规则也会命中别的 server 的工具，需要宿主自己避免。 |
| `Command`、`URL` | 二者选一。`Command` 表示 stdio 服务器，是一个返回 `*exec.Cmd` 的函数，每次连接（包括重连）调用一次，凭据放在返回命令的 `Env` 中；`SysProcAttr`、`Stderr` 等字段都原样生效。`URL` 表示 Streamable HTTP 服务器。 |
| `Headers`、`HTTPClient` | HTTP 服务器使用的请求头和客户端。 |
| `OAuth`、`OAuthHandler` | 二者选一。`OAuth` 使用内置的 OAuth 2.1 流程，`OAuthHandler` 使用你自己实现的 SDK `auth.OAuthHandler`。 |
| `Tools`、`DenyTools` | 按 MCP 工具名指定允许和禁止的工具。`Tools` 为空时表示允许全部工具。 |
| `Trusted` | 设为 true 时信任服务器提供的注解，被服务器标为只读的工具可以并行执行。 |
| `Timeout` | 单次调用的超时时间。为 0 时使用 `Limits.ToolTimeout`。 |
| `Background` | 允许模型把这些工具放到后台运行。 |
| `Prompts` | 把服务器的 prompts 注册成 `/mcp__<server>__<prompt>` 命令。 |
| `ProtocolVersion` | 优先提议的协议版本。为空时使用最新版本。 |

`mcp.Options` 还有以下字段。

| 字段 | 说明 |
|---|---|
| `MaxResultChars` | 工具结果交给模型时的字符上限，默认为 50000。 |
| `MaxMediaBytes` | 工具结果里单个图片或 PDF 的字节上限，默认为 5 MiB。超过上限的内容，以及 png、jpeg、gif、webp 以外类型的图片，会换成一段说明文字，避免模型服务拒绝请求。 |
| `ConnectTimeout` | `Install` 等待首次连接的时长，默认为 30s。 |
| `Discovery` | 工具检索模式，见下文。 |
| `NativeSearch` | 在 `Auto` 模式下决定是否改用 `Native`，见下文。 |
| `DiscoveryRatio` | 在 `Auto` 模式下，工具定义占上下文窗口的比例超过这个值时改用检索。默认为 0.02。 |
| `CatalogRatio` | 在 `Search` 模式下目录段可占的预算比例，超过后只列工具名，不再列摘要。默认为 0.05。 |
| `ContextWindow` | 在 agent 级安装时，`Auto` 模式计算比例所用的窗口大小。默认为 200000。 |
| `Search` | 用于替换内置的关键词检索。 |

当服务器提供的工具很多时，把所有工具定义都发给模型会占用大量上下文。`Options.Discovery` 决定如何向模型呈现这些工具，可选模式如下。

| 模式 | 行为 |
|---|---|
| `Auto`（默认） | 工具定义占窗口的比例小于 `DiscoveryRatio` 时使用 `Direct`，否则使用 `Search`。如果 `NativeSearch` 为 true，则改用 `Native`。 |
| `Direct` | 为服务器的每个工具单独提供一个定义。 |
| `Search` | 只提供 `mcp_search_tools` 和 `mcp_call_tool` 两个工具，工具名列在系统提示词的目录中。`mcp_search_tools` 接受 `names`、`query`、`servers` 和 `detail` 参数，`mcp_call_tool` 接受 `name` 和 `arguments` 参数。 |
| `Native` | 使用延迟加载的工具和厂商的服务端工具检索，详见 [7.8](07-tools.md#78-延迟加载与工具检索)。 |

MCP 工具同样要经过审查规则。使用 `Search` 模式时，模型实际调用的是 `mcp_call_tool`，因此需要用 `mcp.ApprovalRules(rules...)` 包装你的规则。它会为每条针对服务器工具的规则在 `mcp_call_tool` 上补一条镜像规则，使两种模式得出的审查结论一致。

### OAuth 登录

当服务器要求登录时，扩展会发出 `mcp.auth_required` 事件，其中 `State` 为 `waiting`，并带有 `AuthURL`。你需要把这个 URL 交给用户打开，`Authorize` 会一直等到用户完成授权返回。SDK 提供了两种 authorizer：

- 桌面程序或 CLI 使用 `mcp.NewLoopbackAuthorizer(nil)`。它会在本机启动一个回调端口并打开浏览器，你需要把它的 `RedirectURL()` 填入 `OAuth.RedirectURL`。
- 服务端程序使用 `mcp.NewCallbackAuthorizer()`。你需要把它挂到自己的 HTTP 路由上，路由路径就是 `RedirectURL`。

授权结果通过 `TokenStore` 保存，该接口包含 `Load` 和 `Save` 两个方法，按 `mcp.TokenKey` 存取。`TokenKey` 包含服务器名 `Server`、服务器地址 `Resource`，以及在 session 级安装时的 `Session`（session id）与 `Labels`（session 的标签，例如 `user_id`）。多个用户共用一个 store 时，请把 `Labels["user_id"]` 或 `Session` 纳入键里，避免用户之间互相使用令牌；`Resource` 也应纳入键里，服务器换地址后旧令牌不会被发往新地址。

### elicitation

当服务器在调用过程中需要用户输入时，会触发 `mcp.Elicitation` hook。`Mode` 取 `form` 或 `url`，为 `form` 时附带 `Schema`。你在 hook 中返回 `Action`，取值为 `accept`（同时带上 `Content`）、`decline` 或 `cancel`。如果没有注册该 hook，所有请求一律拒绝。用户提供的内容只回传给服务器，模型看不到。多个用户共用一个 MCP 服务器、又需要 elicitation 时，请用 `kernel.SessionUse` 按 session 安装：agent 级安装的连接为所有 session 共用，服务器在调用之外主动发来的请求可能被交给另一个用户正在进行的调用。

该扩展会发出三个事件。`mcp.server_status` 报告连接状态，取值为 connecting、ready、failed 或 closed，并带有工具数。另外两个是 `mcp.auth_required` 和 `mcp.elicitation`。

用 `kernel.Use` 安装的 mcp 扩展会在该 agent 的所有 session 之间共享连接，用 `kernel.SessionUse` 安装则为单个 session 建立独立的连接。进程退出前，请调用 `m.Close(ctx)` 关闭连接。

## 14.9 ext/cmd

`ext/cmd` 下包含七个斜杠命令，每个命令是一个独立的包和一个独立的扩展。命令列表与用法见 [10.5](10-commands.md#105-内置命令)。

## 14.10 ext/litellm

`ext/litellm` 不是扩展，而是两个辅助函数。`litellm.RequestHeaders(o)` 为每个请求打上根 session 的 id，`litellm.CostSource(o)` 从 LiteLLM 的 spend logs 中查询费用。具体用法见 [17.4](17-usage-cost.md#174-查询费用)。

---

[← 上一章：扩展机制](13-extensions.md) · [目录](README.md) · [下一章：上下文管理 →](15-context.md)
