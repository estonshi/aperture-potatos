[← 上一章：限制与默认值](18-limits.md) · [目录](README.md) · [下一章：错误 →](20-errors.md)

# 19. 回调中的调用限制

本章说明在工具、hook、命令、任务和事件回调中调用 Session 方法时需要遵守的规则。遵守这些规则可以避免回调等待自己所在的 session，从而造成死锁。

本章内容：[19.1 调用规则](#191-调用规则) · [19.2 限制的原因](#192-限制的原因) · [19.3 常见场景](#193-常见场景)

## 19.1 调用规则

工具、hook、命令、任务的 `Run` 以及事件回调都可能运行在 session 的执行路径上。在这些回调中调用 session 的方法时，请按下表区分处理：

| 方法 | 在回调中的行为 |
|---|---|
| `Send`、`Command`、`Interrupt`、`Withdraw`、`InterruptWith`、`Snapshot`、`Info`、`Subscribe`、`Emit`、`Announce`、`Tasks().Get`、`Tasks().List`、`Tasks().Stop`、`Tasks().Send` 以及注册与撤销 | 随时可以调用，不会阻塞。 |
| `Tasks().Start`、`Tasks().Restart` | 可以调用。它们会在你的 goroutine 上运行 `task_start` hook 和 `TaskKind.New`，需要审查的 `Restart` 还会等待审查结果。 |
| `SideChat`、`Fork` | 可以调用。它们只对当前 context 取一次快照，不等待 turn，但会在你的 goroutine 上等待新 session 完成初始化。 |
| `SwitchModel` | 可以调用。目标模型的能力已经缓存时会立即返回；尚未缓存时需要等待能力解析，如果调用的正是运行当前回调的 session 或它的祖先，会返回 `ErrReentrantWait`。 |
| `Receipt.Wait`、`Run`、`Close`、`Compact`、`Clear`、`Rewind`、`WaitQuiet` | 如果等待的对象正是运行当前回调的 session 或它的祖先，会返回 `ErrReentrantWait`。这时请改用 `Command("compact", ...)` 这类非阻塞的入口。 |
| `Tasks().Wait` | 工具和 hook 中可以等待任务，但任务不能等待自己或祖先任务。 |
| `SideCall` | 可以调用，它不需要等待 turn。 |

重入检测依赖 kernel 传给回调的 `ctx`。只有把收到的 ctx 继续向下传递，检测才会生效。如果换成 `context.Background()`，调用会绕过检测，可能陷入死等，最终只能依靠超时结束。

有一个例外：在 `WaitIdle` 命令中可以调用 `Compact`、`Clear` 和 `Rewind`。这时没有 turn 在运行，所以它们会直接执行。

## 19.2 限制的原因

一个 session 在同一时刻只运行一轮。工具是在这一轮中执行的，如果工具等待这一轮结束，而这一轮又在等待工具返回，两者就会互相等待。事件回调也是同样的道理。订阅默认不会丢弃事件，如果回调在等待 turn，而 turn 在等待回调消费事件，同样会造成死锁。

此外还需要注意以下几点：

- 并行工具组中的工具和 `post_tool_use` hook 是并发调用的，详见 [7.5](07-tools.md#75-执行顺序与并发)。
- 同步 hook 有超时，而审查（`approval`）默认没有超时。
- 任务可以放心等待自己用 `ChildOf` 创建的子 session，例如在任务的 `Run` 中调用子 session 的 `Run` 或 `Receipt.Wait`，这不算重入。反过来，子 session 中的工具或 hook 等待所属的 session 或任务时，会立即返回 `ErrReentrantWait`，因为这样等下去最终是在等它自己。

## 19.3 常见场景

下表列出回调中几种常见需求的正确写法：

| 需求 | 写法 |
|---|---|
| 在工具中让 agent 压缩上下文 | 调用 `tc.Session().Command("compact", "")`，不要调用 `Compact`。 |
| 在工具中给模型补充一句话 | 直接写在工具的返回结果里，或者调用 `tc.Session().Announce(...)`。 |
| 在 hook 中启动一个后台任务并等待它完成 | 先调用 `Tasks().Start`，再调用 `Tasks().Wait`，两者都可以使用。 |
| 在事件回调中，收到某个事件后发送输入 | 可以调用 `sess.Send`，但不要在回调中对它调用 `Wait`。 |
| 在命令中重放最后一轮 | 在 `WaitIdle` 命令中调用 `Rewind`，然后返回 `CommandResult{Prompt: &input}`。 |
| 在任务中向主会话提问 | 调用 `rt.AskInput(q)`，然后从 `rt.Inbox()` 接收回答。不要先调用 `Session().Send` 再 `Wait`。 |

---

[← 上一章：限制与默认值](18-limits.md) · [目录](README.md) · [下一章：错误 →](20-errors.md)
