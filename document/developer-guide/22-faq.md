[← 上一章：测试](21-testing.md) · [目录](README.md)

# 22. 常见问题

本章汇总了接入 potatOS 时最常遇到的问题及其解决办法。遇到问题时，可以先在这里查找，再根据给出的链接阅读相关章节。

**为什么收不到 `session.started` 事件？**

请在调用 `NewSession` 时通过 `kernel.WithSessionSink` 或 `WithSessionFunc` 挂上订阅。如果在会话创建之后再调用 `Subscribe`，就会错过已经发出的事件。如果需要在 agent 级别订阅，请使用 `kernel.WithEventSink`。

**为什么模型一直停着不动？**

常见原因有两个。一是某个订阅者没有消费事件，背压会让模型的输出流等待它。二是审查者没有返回结果，而审查默认没有超时。你可以通过 `sess.Snapshot()` 查看 `Phase`、正在运行的 `Workers` 和队列长度。如果出现了 `hook.started` 却没有对应的 `hook.completed`，说明执行卡在了某个 hook 中。

**如何只获取最终答案？**

使用 `rcpt.Wait(ctx)` 返回的 `TurnResult.FinalText`。

**如何只显示主对话的流式文本？**

订阅 `message.delta` 事件，并只保留 `len(ev.Path) == 0` 且 `Channel == "text"` 的事件。

**为什么修改了工具定义后，模型没有看到变化？**

工具定义在一轮开始时冻结，在这一轮之内不会改变。对于声明了 `ThinkingPrefixBound` 的模型，工具定义在整个 epoch 内都不会改变。因此，改动会从下一轮开始生效，对于上述模型则从下一个 epoch 开始生效。如果工具的 `Spec()` 是动态变化的，请在变化时调用 `kernel.ToolChanged`。

**能否在系统提示词中放入当前时间？**

不建议这样做，请改用 `ContextProvider`，见 [11.4](11-prompt.md#114-每轮动态内容)。系统提示词在一个 epoch 内保持不变，放入动态内容会破坏缓存。

**模型报告 thinking 签名错误，应该怎么处理？**

请使用 `WithCapabilities` 声明 `ThinkingPrefixBound: true`。如果没有声明，kernel 会在第一次报错后降级重试，并发出 `model.notice` 事件提示你。

**切换到其他厂商的模型后请求被拒绝，应该怎么处理？**

有些接口对历史格式要求更严格，例如带工具的 DeepSeek 要求每条助手消息都带有推理字段，因此切换过去后可能被拒绝。遇到这种情况，请在切换后执行一次 `/compact` 或 `/clear`。

**推理模型思考时间较长时被重试了，应该怎么处理？**

请调大 `RetryPolicy.FirstEventTimeout`。它的默认值为 2 分钟，从发出请求时开始计时。

**如何让某个工具只能由主 agent 执行？**

在 `pre_tool_use` hook 中检查 `in.Path`。如果 `in.Path` 非空，说明这次调用来自 subagent 或后台任务。具体写法见 [9.1](09-hooks.md#91-注册) 中的例子。

**为什么在工具中调用 `Compact` 会返回 `ErrReentrantWait`？**

工具运行在 session 自身的执行路径上，因此不能在工具中等待这个 session。请改为调用 `tc.Session().Command("compact", "")`，详见第 [19](19-callback-rules.md) 章。

**如何关闭某个内置能力？**

使用 `std.Without("<name>")`，其中的名字就是扩展的 `Name()`，例如 `subagent`、`history`、`tasks`、`compact`、`cmd.fork`。如果只是不想让模型看到某个工具，可以使用 `Registrar.Hide(kernel.KindTool, name)`。

**为什么 `Close` 返回 `LeakError`？**

这说明有工具或任务没有在 `CloseTimeout` 内退出。你可以通过 `LeakError.Workers` 中的名字找到对应的 worker。工具和任务需要响应 `ctx.Done()`，你也可以调大 `CloseTimeout`。

**为什么 `Send` 返回 `ErrQueueFull`？**

这表示有 256 条输入尚未被处理。通常原因是 session 卡住了，而不是队列太小。排查方法见上文“为什么模型一直停着不动？”。

**能否手动修改 JSONL 文件？**

不建议这样做。kernel 把历史视为只追加的记录，如果修改了内容再调用 `Resume`，可能导致工具调用与其结果无法对应。如果需要修改历史，请使用回退、分叉或清空。

**如何在多进程或多副本环境中使用？**

同一时刻，一个 session 只能被一个进程打开。`ErrSessionOpen` 只在同一个 agent 内进行检查，跨进程的互斥需要你自己保证。你可以按 session id 把请求路由到固定的副本，或者在你的存储实现中实现互斥。

**模型的回复被截断了，应该怎么处理？**

这种情况下 `TurnResult.Outcome` 的值为 `max_tokens`。你可以调大 `Params.MaxOutputTokens`，同时声明模型的 `MaxOutputTokens` 能力。也可以开启 `WithAutoContinueTruncatedText(true)`，让纯文本回复自动续写一次。

---

[← 上一章：测试](21-testing.md) · [目录](README.md)
