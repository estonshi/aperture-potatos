[← 上一章：命令](10-commands.md) · [目录](README.md) · [下一章：任务 →](12-tasks.md)

# 11. 系统提示词与动态上下文

本章说明模型看到的提示词由哪些部分组成，包括你的指令写在哪里、扩展的说明放在哪里，以及如何在不破坏缓存的前提下把每轮都会变化的信息送给模型。读完本章，你可以为 agent 编写指令、为扩展添加提示词分段，并检查最终渲染出的系统提示词。

本章内容：[11.1 系统提示词的结构](#111-系统提示词的结构) · [11.2 编写你的指令](#112-编写你的指令) · [11.3 扩展分段](#113-扩展分段) · [11.4 每轮动态内容](#114-每轮动态内容) · [11.5 查看渲染结果](#115-查看渲染结果) · [11.6 面向模型的文字](#116-面向模型的文字) · [11.7 覆盖固定文本](#117-覆盖固定文本) · [11.8 建议做法](#118-建议做法)

## 11.1 系统提示词的结构

系统提示词的结构由 kernel 决定，你不能整体替换它，只能向其中的部分段落填写内容。各段按以下顺序排列：

| 顺序 | 段 | 内容来源 |
|---|---|---|
| 1 | core | 由 kernel 写入，说明运行约定，即提醒、任务通知和其他 agent 的来信各代表什么。 |
| 2 | identity | 昵称与描述，通过 `WithNickname` 和 `WithDescription` 设置。 |
| 3 | capabilities | 已安装能力的使用说明，由扩展通过 `SlotCapabilities` 写入。 |
| 4 | user | 你的指令，优先级高于上面的能力说明，通过 `WithInstructions` 系列选项设置。 |
| 5 | catalog | skills、可调用的 agent 等目录，由扩展通过 `SlotCatalog` 写入。 |
| 6 | environment | 环境事实，由扩展通过 `SlotEnvironment` 写入。 |

整段系统提示词在一个 epoch 内保持不变，只有在新建会话、压缩和 `/clear` 时才会重新渲染。回退、分叉和切换模型虽然也会开启新的 epoch，但系统提示词是原样复制过去的。这样做可以命中模型的 prompt 缓存，也不会破坏思考签名。

## 11.2 编写你的指令

你的指令写在 user 段。SDK 提供三种设置方式，分别适用于固定文本、按会话动态生成的文本和单个 session 的专属文本：

| 方式 | 作用域 | 说明 |
|---|---|---|
| `kernel.WithInstructions(text)` | agent | 设置固定的指令文本。 |
| `kernel.WithInstructionsFunc(fn)` | agent | 每次重新渲染系统提示词时计算一次，即新建会话、压缩和清空时。`fn` 收到 `SessionInfo`，其中包含 `SessionID`、`Labels`、`Epoch` 等字段，因此可以按租户或项目生成不同的指令。 |
| `kernel.SessionInstructions(text)` | session | 为这个 session 指定指令，覆盖 agent 级的指令。 |

下面的例子按 session 标签中的租户读取对应的指令：

```go
kernel.WithInstructionsFunc(func(ctx context.Context, info kernel.SessionInfo) (string, error) {
	tenant := info.Labels["tenant"]
	return loadInstructions(ctx, tenant) // 指令从你的配置中心读取
})
```

## 11.3 扩展分段

扩展可以向 capabilities、catalog 和 environment 三个段写入内容，方法是在 `Install` 中注册一个提示词分段：

```go
r.PromptSection(kernel.PromptSection{
	Name:  "repo-facts",
	Slot:  kernel.SlotEnvironment,
	Order: 10,
	Render: func(ctx context.Context, rc kernel.RenderContext) (string, error) {
		return "Repository: acme/web, default branch main.", nil // 返回 "" 时省略这一段
	},
})
```

`PromptSection` 的字段如下：

| 字段 | 说明 |
|---|---|
| `Name` | 段名，会出现在 `system.rendered` 事件中。 |
| `Slot` | 取 `SlotCapabilities`、`SlotCatalog` 或 `SlotEnvironment` 之一。core、identity 和 user 三个段不对扩展开放。 |
| `Order` | 同一 slot 内的排列顺序，顺序相同时按名字排序。 |
| `AnnounceChanges` | 设为 true 时，如果内容在 epoch 内发生变化，会以 reminder 的形式告诉模型，而不是等到下一个 epoch。 |
| `Render` | 每次重新渲染系统提示词时调用一次，即新建会话、压缩和清空时。回退、分叉和切换模型时沿用原来的文本。`RenderContext` 包含 `Agent`、`Session`、`Tools` 和 `Epoch`，其中 `Tools` 是这个 epoch 中模型看到的工具定义。 |

如果不需要编写扩展，也可以用 `kernel.WithPromptSection(ps)` 在 agent 级直接注册分段。

## 11.4 每轮动态内容

当前时间、用户状态这类每轮都会变化的内容不应写进系统提示词，否则会让缓存失效。这类内容应通过 `ContextProvider` 以 reminder 的形式送达：

```go
kernel.WithContextProvider(kernel.ContextProvider{
	Name: "clock",
	When: kernel.AtTurnStart, // 也可以用 kernel.AtEveryStep
	Provide: func(ctx context.Context, pc kernel.ProvideContext) (string, error) {
		return "Today is " + time.Now().Format("2006-01-02") + ".", nil // 返回 "" 时这一次不添加
	},
})
```

`ContextProvider` 的字段如下：

| 字段 | 说明 |
|---|---|
| `Name` | 提醒的来源标签。 |
| `When` | 取 `AtTurnStart` 时，内容在每轮开始时随输入一起送达。取 `AtEveryStep` 时，每次调用模型之前都会送达。 |
| `Provide` | 生成内容的函数。`ProvideContext` 包含 `Session`、`TurnID`、`Step` 和 `Inputs`。在 `AtTurnStart` 时，`Inputs` 是这一轮的输入，因此你可以根据输入内容决定提供什么背景。 |

在扩展中，用 `r.ContextProvider(p)` 注册。内容送达时会发出 `context.injected` 事件，事件包含 `Sources`、`Count` 和 `Truncated` 字段。

除此之外，还有两种一次性的动态内容。`sess.Announce(source, text, payload)` 可以发出一条不唤醒会话的提醒。hook 的输出也可以通过 `AdditionalContext` 附加内容，详见 [9.2](09-hooks.md#92-同步与异步)。

## 11.5 查看渲染结果

调试提示词时，你可以通过以下几种方式查看实际渲染出的内容：

- 每个 epoch 渲染完成后会发出 `system.rendered` 事件。其中 `Sections` 列出每段的 slot、名字和字符数，`Hash` 可以用来判断两次渲染的结果是否相同。
- `sess.Messages()[0]` 是当前 epoch 的 system 行，它的 `Content` 中是按段划分的渲染文本块。
- `sess.Context()` 按来源列出 token 占用，其中 `Kind` 为 `system` 的部分就是提示词的各段。
- 在测试中，可以通过 `providertest.ScriptProvider` 的 `Requests()` 拿到实际发出的 `System`，详见 [21](21-testing.md)。

## 11.6 面向模型的文字

提醒、任务通知和其他 agent 的来信都会带着标签送达，core 段会告诉模型这些内容不是用户说的话，也不代表授权。来自其他 agent 或任务的文本在送达前，其中的标签字符会被转义，过长的部分会被截断，以防这些文本冒充运行时。

你自己编写的工具描述、命令模板和分段文本同样会被模型看到。建议使用英文和中性的措辞，不要写“你必须”这类带有对抗性的语气。

如果你想在提示词中引用运行时的约定，例如告诉模型提醒的格式，则不需要重复说明，因为 core 段已经写明了这些约定。

## 11.7 覆盖固定文本

运行时发给模型的固定文本都有默认英文版本。宿主需要另一种语言或措辞时，可以用 `kernel.WithTexts` 在 agent 级覆盖：

```go
ag, err := kernel.New(
	kernel.WithModel(model),
	kernel.WithTexts(map[string]string{
		"unknown_tool": "没有名为 {{name}} 的工具。",
		"identity":     "<identity>\n你叫 {{nickname}}。\n</identity>",
		"system.core":  coreText, // 整个 core 段
	}),
)
```

键有两类：

- `spec/prompts/reminders.md` 中每个二级标题就是一个 key，例如 `unknown_tool`、`denied_by_rule`、`approval_stopped`。正文使用同一套模板语法，`{{name}}` 这类变量照常展开。
- 提示词文件名，例如 `identity`、`user`、`system.core`、`agent_message`、`task_started`、`task_notification`。

规则与限制：

- 只覆盖给出的键，其余沿用内置文本；`New` 会拒绝拼错的键，返回 `ErrInvalidName`。
- `system.core` 可以整段替换，但必须仍然提到 `spec/data/runtime_tags.json` 里的每个标签，并以 `</runtime-conventions>` 闭合，否则 `New` 失败。这样模型不会失去“提醒、任务通知和其他 agent 来信各代表什么”的说明。
- 覆盖是 agent 级的，派生 agent 继承，会话内不能改。已经渲染的 epoch 保持原文本，新 epoch 才生效。
- provider 适配器写出的固定文本（`dangling_tool_call`、`tool_removed`、`document_link`、`tool_result_media`）通过 `provider.Request.Texts` 传入，kernel 会自动带上同一份覆盖表。自己实现 provider 时可以不设置，此时使用内置文本。
- 其余硬编码文案通过结构化字段翻译：内核自己发出的 `notice` 带 `Kind` 和 `Detail`，宿主可以按 `Kind` 决定显示什么，不必解析 `Text`。详见 [6](06-events.md)。

## 11.8 建议做法

- 静态内容放进系统提示词，动态内容通过 `ContextProvider` 送达。判断标准是这段文字在一天之内是否会变化，如果会变化，就不要放进系统提示词。
- 多租户场景建议组合使用 `WithLabels` 和 `WithInstructionsFunc`。这样一个 Agent 就能服务所有租户，指令按标签生成。
- 扩展分段的内容应尽量保持稳定。`Render` 返回的文本要到下一次重新渲染时才会更新，在此之前内容即使变了，模型也看不到，除非设置了 `AnnounceChanges`。确实需要频繁变化的内容应改用 `ContextProvider`。
- 建议谨慎使用 `AtEveryStep`。每个 step 都送达的提醒会一直留在历史中，在长对话里会占用不少 token，大多数场景使用 `AtTurnStart` 就足够了。
- 监控缓存命中情况时，可以查看 `system.rendered` 的 `Hash`。同一个 session 连续几个 epoch 的 hash 应该相同，如果发生变化，说明有段落的内容在变动。

---

[← 上一章：命令](10-commands.md) · [目录](README.md) · [下一章：任务 →](12-tasks.md)
