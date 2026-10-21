[← 上一章：审查](08-approval.md) · [目录](README.md) · [下一章：命令 →](10-commands.md)

# 9. Hook

Hook 让你在 agent 运行过程中的固定节点插入自己的逻辑，例如改写输入、阻断工具调用、向上下文追加内容或者记录审计日志。当你需要定制 loop 的行为又不想修改 SDK 时，可以使用 hook。本章介绍 hook 的注册方式、同步与异步 hook 的区别、每个 hook 点的触发时机和输入输出，以及一些常见用法。

本章内容：[9.1 注册](#91-注册) · [9.2 同步与异步](#92-同步与异步) · [9.3 hook 点一览](#93-hook-点一览) · [9.4 每个点的输入与输出](#94-每个点的输入与输出) · [9.5 事件](#95-事件) · [9.6 常见用法](#96-常见用法) · [9.7 扩展定义的 hook 点](#97-扩展定义的-hook-点) · [9.8 建议做法](#98-建议做法)

## 9.1 注册

注册 hook 时需要指定 hook 点、hook 的名称和处理函数，并可以附带选项。下面的例子分别注册了一个同步 hook 和一个异步 hook：

```go
// 同步 hook 可以改写或阻断操作
kernel.WithHook(kernel.PreToolUse, "main-agent-books",
	func(ctx context.Context, in kernel.PreToolUseInput) (kernel.PreToolUseOutput, error) {
		if len(in.Path) > 0 { // 调用来自 subagent 或后台任务
			return kernel.PreToolUseOutput{Decision: kernel.Deny, Reason: "only the main agent may book"}, nil
		}
		return kernel.PreToolUseOutput{}, nil
	}, kernel.Match(kernel.MatchNames("book_hotel")))

// 异步 hook 不阻塞 loop，只能向后续上下文追加内容
kernel.WithAsyncHook(kernel.PostToolUse, "audit",
	func(ctx context.Context, in kernel.PostToolUseInput) (kernel.Injection, error) {
		log.Printf("audit: %s %s", in.Call.Name, in.Result.Outcome)
		return kernel.Injection{}, nil
	})
```

SDK 提供四种注册方式：

| 写法 | 作用域 | 说明 |
|---|---|---|
| `kernel.WithHook(point, name, fn, opts...)` | agent | 注册同步 hook。 |
| `kernel.WithAsyncHook(point, name, fn, opts...)` | agent | 注册异步 hook。 |
| `kernel.On(r, point, name, fn, opts...)`、`kernel.OnAsync(...)` | 由 `r` 决定 | 在扩展的 `Install` 中或 `sess.Registrar()` 上使用。返回的 `*Registration` 可用于撤销注册。 |
| `kernel.OnJSON(r, point, name, fn, opts...)`、`kernel.OnJSONAsync(...)` | 由 `r` 决定 | 以 JSON 形式收发输入和输出，格式见 `spec/schema/hook.schema.json`，适用于转发给其他语言的进程处理。 |

注册时可以传入以下选项：

| 选项 | 作用 |
|---|---|
| `Match(m)` | 只在 key 匹配时触发。key 一般是工具名或命令名，各 hook 点的 key 见 [9.3](#93-hook-点一览)。可用的匹配器有：精确匹配 `MatchNames("a", "b")`，整串正则匹配 `MatchRegexp(expr)`，用 Go 编写任意逻辑的 `MatchFunc(fn)`，以及声明式的 `ParseMatcher(s)`。`ParseMatcher` 中 `""` 或 `"*"` 表示全部匹配，`a\|b` 表示精确匹配，以 `re:` 开头表示正则。 |
| `Priority(n)` | 决定同一 hook 点上多个 hook 的执行顺序，数值小的先执行，默认为 0。 |
| `Timeout(d)` | 替换这个 hook 点的默认超时。设为 0 表示不设超时。 |
| `Fail(p)` | 决定 hook 出错、超时或 panic 时如何处理。`FailDefault` 按 hook 点的默认行为处理，`FailOpen` 视为 hook 没有意见，`FailClosed` 视为阻断或拒绝。 |
| `WakeLimit(n, per)` | 限制异步 hook 唤醒空闲 session 的频率。超过上限的注入会等到下一轮再送达。 |

## 9.2 同步与异步

hook 分为同步和异步两种，它们的能力不同。

**同步 hook** 在触发它的执行路径上运行，agent 会等待它返回。同步 hook 的输出类型带有改写和阻断相关的字段，并且除审查点之外都内嵌了 `kernel.Common`。`Common` 包含以下字段：

| `Common` 字段 | 作用 |
|---|---|
| `AdditionalContext` | 作为 reminder 加入上下文，来源标记为 `hook:<name>`。 |
| `SystemMessage` | 只展示给人看，以 `notice` 事件的形式发出。 |
| `Halt`、`HaltReason` | 在这个 hook 点之后结束本轮，历史仍然保持合法。 |

**异步 hook** 在主流程之外运行，agent 不会等待它。异步 hook 只能返回 `kernel.Injection`，其字段如下：

| 字段 | 作用 |
|---|---|
| `AdditionalContext` | 在下一次模型调用或下一轮时作为 reminder 送达。 |
| `SystemMessage` | 只展示给人看。 |
| `Wake` | 允许唤醒空闲的 session 开始新的一轮，受 `WakeLimit` 约束。由于注入的内容是提醒，这样开始的一轮也计入 `Limits.MaxConsecutiveAutoTurns`。 |

返回类型本身保证了异步 hook 无法改变任何东西。同时运行的异步 hook 数量上限为 `Limits.MaxAsyncHooks`，默认为 64。

每个 hook 的输入都内嵌了 `kernel.HookEnv`，其中包括 `Session`、`SessionID`、`Agent`、`TurnID`、`Step` 和 `Path`。在 hook 中可以调用哪些 `Session` 方法，见 [19](19-callback-rules.md)。

同一个 hook 点上有多个同步 hook 时，它们依次执行，顺序依据 `Priority`、作用域（agent 级先于 session 级）和注册顺序确定。前一个 hook 的改写对后一个 hook 可见。一旦某个 hook 给出阻断或拒绝，后面的 hook 就不再执行。

## 9.3 hook 点一览

下表列出 kernel 提供的全部 hook 点，以及每个点的触发时机、匹配用的 key、默认超时、失败时的默认处理和同步 hook 能做的事情：

| hook 点 | 何时触发 | key | 默认超时 | 失败时 | 同步 hook 能做什么 |
|---|---|---|---|---|---|
| `SessionStart` | session 新建或恢复时，以及压缩、清空、回退、分叉开启新 epoch 之后。 | 来源，取值为 new、resume、clear、compact、rewind 或 fork。 | 30s | 视为没有意见。 | 追加上下文。 |
| `SessionEnd` | session 关闭时。 | 关闭原因。 | 5s | 视为没有意见。 | 执行收尾工作。 |
| `UserPromptSubmit` | 输入送达模型之前。 | 输入来源，例如 human、command 等。 | 30s | 阻断。 | 阻断全部或部分输入，或改写输入。 |
| `CommandInvoke` | 命令执行之前。 | 命令名。 | 10s | 阻断。 | 阻断命令或改写参数。 |
| `PreModelRequest` | 每个 step 发送请求之前。 | 模型名。 | 10s | 视为没有意见。 | 提供本 step 的参数补丁、设置下一轮的推理参数，或直接给出一个合成回复。 |
| `PostModelResponse` | 模型回复提交之前。 | 停止原因。 | 30s | 视为没有意见。 | 拒绝这条回复。 |
| `PreToolUse` | 每个工具调用执行之前。 | 工具名。 | 60s | 拒绝。 | 给出 allow、ask 或 deny，改写输入，或跳过工具直接给出结果。 |
| `Approval` | 规则或 `pre_tool_use` hook 要求审查时。 | 工具名。 | 无 | 拒绝。 | 给出审查结论。`WithApprover` 就注册在这个点上。 |
| `PostToolUse` | 每个工具调用结束之后。 | 工具名。 | 30s | 视为没有意见。 | 替换结果，或要求在这批调用结束后停止本轮。 |
| `PostToolBatch` | 一批工具调用全部结束之后。 | 无。 | 30s | 视为没有意见。 | 追加上下文或停止本轮。 |
| `Stop` | 模型给出最终回答、本轮即将结束时。 | 无。 | 30s | 视为没有意见。 | 要求继续运行。 |
| `TurnEnd` | 每轮结束之后，无论结局如何。 | 本轮的结局。 | 10s | 视为没有意见。 | 追加上下文，随下一轮送达。 |
| `PreCompact` | 压缩之前。 | 触发原因。 | 30s | 视为没有意见。 | 阻断压缩（窗口溢出引起的压缩除外），或改写摘要说明。 |
| `PostCompact` | 压缩之后。 | 触发原因。 | 30s | 视为没有意见。 | 追加上下文。 |
| `ProviderSwitch` | 下一轮开始、待生效的模型切换真正生效之前。 | 目标模型名。 | 10s | 阻断。 | 阻断切换。 |
| `TaskStart` | 任务启动、重启或续接之前。 | 任务类型。 | 10s | 拒绝。 | 拒绝任务，或改写 `TaskSpec`。 |
| `TaskNotify` | 任务通知送达模型之前。 | 任务类型。 | 10s | 视为没有意见。 | 改写通知文本，或设置不唤醒 session。 |

表中“失败时”一列是 `FailDefault` 下的行为，你可以用 `Fail(p)` 为每个 hook 单独修改。同步 hook 超时或被取消后，kernel 还会再等待 `Limits.HookGrace`（默认 1s）让它返回，之后便放弃等待。`Approval` 点只接受同步 hook。

## 9.4 每个点的输入与输出

下表只列出各 hook 点中可以改写的字段和关键的输入字段。除此之外，每个输入都另含 `HookEnv`，每个输出（审查点除外）都另含 `Common`。

| 点 | 输入 | 输出 |
|---|---|---|
| `SessionStart` | `Source`、`Model` | 无 |
| `SessionEnd` | `Reason`、`Usage` | 无 |
| `UserPromptSubmit` | `Inputs []PromptInput`，每项包含 `ID`、`Text`、`Parts`、`Origin` 和 `Metadata`。一次触发只包含一种来源的输入。 | `Block` 丢弃全部输入，`BlockIDs` 丢弃指定的输入，`Reason` 说明理由。`UpdatedInputs` 按 `ID` 匹配并改写 `Text`、`Parts` 和 `Metadata`，但不能改变来源。 |
| `CommandInvoke` | `Name`、`Args`、`Busy` | `Block`、`Reason`、`UpdatedArgs *string` |
| `PreModelRequest` | `Model`、`Params`、`ToolNames`、`EstTokens` | `ParamsPatch` 只修改这一步的参数，可设置 `MaxOutputTokens`、`Temperature`、`TopP`、`StopSequences`、`ToolChoice`、`Headers` 和 `Extra`。`NextTurnReasoning` 设置下一轮的推理参数。`Synthetic *message.Message` 表示不调用模型，直接用这条消息作为回复。 |
| `PostModelResponse` | `Message` 是公开形式的回复，不含接口的原始数据。另有 `Stop` 和 `Usage`。 | `Reject` 和 `Reason`。拒绝后回复不会进入历史，本轮结束。 |
| `PreToolUse` | `Call`（包含 `ID`、`Name` 和 `Input`）、`Spec` | `Decision` 取值为 `Allow`、`Ask` 或 `Deny`，另有 `Reason`。`UpdatedInput` 会整个替换输入。`SyntheticResult` 包含 `Content` 和 `IsError`，设置后跳过工具执行。 |
| `Approval` | 见 [8.3](08-approval.md#83-审查者) | 见 [8.3](08-approval.md#83-审查者) |
| `PostToolUse` | `Call`、`Result`、`DurationMS` | `UpdatedResult *SyntheticResult`、`StopTurn` |
| `PostToolBatch` | `Calls`、`Results` | `StopTurn` |
| `Stop` | `LastAssistantText`、`StopHookActive`（表示本轮已经续跑过）、`Continuations` | `Continue` 和 `Reason`。`Reason` 会作为 reminder 发给模型。一轮最多续跑 `Limits.MaxStopContinuations` 次，默认为 8。 |
| `TurnEnd` | `Result TurnResult` | 无 |
| `PreCompact` | `Trigger` 取值为 manual、auto、overflow 或 switch。另有 `Instructions`、`PreTokens`、`Range`。 | `Block`、`Reason`、`UpdatedInstructions *string` |
| `PostCompact` | `Trigger`、`Summary`、`PreTokens`、`PostTokens`、`Epoch` | 无 |
| `ProviderSwitch` | `From`、`To`、`DroppedReasoning`、`DroppedOpaque` | `Block`、`Reason` |
| `TaskStart` | `Spec`、`CallID`、`Attempt`、`Continuation`、`Instruction` | `Deny`、`Reason`、`UpdatedSpec`。`UpdatedSpec` 不能修改 `Kind`。 |
| `TaskNotify` | `Info TaskInfo`、`Text` | `UpdatedText`、`NoWake` |

## 9.5 事件

每次 hook 执行都会发出事件。当你需要排查某个操作为什么被阻断时，这些事件很有用：

| 事件 | 字段 |
|---|---|
| `hook.started` | `HookID`、`Point`、`Name`、`Async`、`CallID` |
| `hook.completed` | `HookID`、`Point`、`Name`、`CallID`、`Decision`、`DurationMs`、`SystemMessage` |
| `hook.failed` | `HookID`、`Point`、`Name`、`Error`、`Timeout`、`Canceled`、`PolicyApplied`（表示按哪种失败策略处理） |

## 9.6 常见用法

本节给出几个常见场景的写法，可以直接作为起点修改。

### 记录审计日志

审计不需要改变流程，因此适合用异步 hook 实现，不会拖慢 loop：

```go
kernel.WithAsyncHook(kernel.PostToolUse, "audit", func(ctx context.Context, in kernel.PostToolUseInput) (kernel.Injection, error) {
	audit.Write(in.SessionID, in.Call.Name, in.Call.Input, in.Result.Outcome, in.DurationMS)
	return kernel.Injection{}, nil
})
```

### 在结果交给模型之前抹掉密钥

下面的 hook 在 `bash` 和 `read_file` 工具结束后检查结果文本，如果其中有密钥，就用清理后的文本替换原结果：

```go
kernel.WithHook(kernel.PostToolUse, "redact", func(ctx context.Context, in kernel.PostToolUseInput) (kernel.PostToolUseOutput, error) {
	text := message.Message{Content: in.Result.Content}.Text() // 这里只处理文本结果
	if cleaned := redactSecrets(text); cleaned != text {
		return kernel.PostToolUseOutput{UpdatedResult: &kernel.SyntheticResult{Content: message.Content{&message.TextBlock{Text: cleaned}}}}, nil
	}
	return kernel.PostToolUseOutput{}, nil
}, kernel.Match(kernel.MatchNames("bash", "read_file")))
```

### 强制模型在第一步调用指定工具

通过 `PreModelRequest` 的 `ParamsPatch` 设置 `ToolChoice`，可以要求模型在第一个 step 先调用 `make_plan`：

```go
kernel.WithHook(kernel.PreModelRequest, "plan-first", func(ctx context.Context, in kernel.PreModelRequestInput) (kernel.PreModelRequestOutput, error) {
	if in.Step == 1 {
		return kernel.PreModelRequestOutput{ParamsPatch: &kernel.ParamsPatch{ToolChoice: &provider.ToolChoice{Mode: provider.ToolChoiceNamed, Name: "make_plan"}}}, nil
	}
	return kernel.PreModelRequestOutput{}, nil
})
```

### 在回答结束前检查格式

`Stop` hook 可以在模型给出最终回答时检查内容，不符合要求就让模型继续。检查 `StopHookActive` 可以避免反复要求续跑：

```go
kernel.WithHook(kernel.Stop, "checklist", func(ctx context.Context, in kernel.StopInput) (kernel.StopOutput, error) {
	if !in.StopHookActive && !strings.Contains(in.LastAssistantText, "## Summary") {
		return kernel.StopOutput{Continue: true, Reason: "End your answer with a '## Summary' section."}, nil
	}
	return kernel.StopOutput{}, nil
})
```

### 阻断含有敏感内容的用户输入

下面的 hook 只对来源为 human 的输入生效，丢弃其中违反策略的那几条：

```go
kernel.WithHook(kernel.UserPromptSubmit, "policy", func(ctx context.Context, in kernel.UserPromptSubmitInput) (kernel.UserPromptSubmitOutput, error) {
	for _, p := range in.Inputs {
		if violates(p.Text) {
			return kernel.UserPromptSubmitOutput{BlockIDs: []string{p.ID}, Reason: "policy"}, nil
		}
	}
	return kernel.UserPromptSubmitOutput{}, nil
}, kernel.Match(kernel.MatchNames("human")))
```

### 每轮结束后为模型补充背景信息

`TurnEnd` hook 返回的 `AdditionalContext` 会随下一轮送达模型：

```go
kernel.WithHook(kernel.TurnEnd, "budget", func(ctx context.Context, in kernel.TurnEndInput) (kernel.TurnEndOutput, error) {
	return kernel.TurnEndOutput{Common: kernel.Common{AdditionalContext: fmt.Sprintf("Input tokens used in this turn: %d.", in.Result.Usage.InputTokens)}}, nil
})
```

## 9.7 扩展定义的 hook 点

除了 kernel 提供的 hook 点，扩展也可以用 `kernel.NewHookPoint(def)` 定义自己的 hook 点，其注册和触发方式与 kernel 的 hook 点完全相同。内置扩展定义了以下 hook 点：

| 点 | 何时触发 | 能做什么 |
|---|---|---|
| `subagent.Start` | subagent 任务要开启子 session 时。 | 拒绝、改写 prompt，或为子 session 的第一轮追加上下文。 |
| `subagent.Stop` | 子 session 的一轮结束时。 | 要求子 session 继续，`Reason` 会作为下一条指令。 |
| `skills.Activate` | 模型或用户要加载一个 skill 时。 | 拒绝加载。 |
| `peer.AuthRequired` | 远端 agent 要求鉴权时。 | 修复凭据后让它重试。 |
| `mcp.Elicitation` | MCP 服务器在调用过程中向用户索要输入时。 | 接受并附带内容，或者拒绝、取消。 |

在你自己的扩展中定义 hook 点时，需要在 `HookPointDef` 中给出 `Name`、`Key`、`Chain`、`Merge`、`Timeout`、`Fail` 和 `Closed`。其中 `Key` 是匹配器使用的字符串，`Chain` 用于把前一个 hook 的改写交给下一个 hook，`Merge` 用于合并多个输出，`Closed` 是失败时使用的默认输出。触发 hook 点时调用 `kernel.Fire(ctx, sess, point, in)`。

## 9.8 建议做法

- 建议在可行时优先使用异步 hook。审计、统计、通知这类不改变流程的逻辑适合用 `WithAsyncHook` 实现，模型不会因此等待。
- 同步 hook 应尽快返回，并设置超时。同步 hook 处于 loop 的关键路径上，如果需要调用外部服务，应自己设置 `Timeout`，并考虑清楚使用哪种 `Fail` 策略。
- 用 `Match` 缩小 hook 的触发范围，不要在 hook 内部判断工具名。匹配器不命中时，hook 根本不会被调用，也不会产生事件。
- 在失败时默认不放行的 hook 点上，务必妥善处理错误。`UserPromptSubmit`、`CommandInvoke`、`PreToolUse`、`Approval`、`ProviderSwitch` 和 `TaskStart` 这几个点在 hook 出错、超时或 panic 时默认阻断或拒绝，因此 hook 中的一个 bug 就可能让整个 agent 停止工作。
- 改写时要保持数据的形状。`UpdatedInput` 会整个替换原输入，返回的内容仍须是合法的工具输入。`UpdatedInputs` 不能改变输入的来源。
- 建议用英文和中性措辞编写发给模型的 `AdditionalContext`。需要展示给人看的内容放进 `SystemMessage`。
- 并行组中 `PostToolUse` hook 的共享状态需要加锁，因为这些 hook 是并发执行的。

---

[← 上一章：审查](08-approval.md) · [目录](README.md) · [下一章：命令 →](10-commands.md)
