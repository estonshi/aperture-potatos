[← Previous: Limits and defaults](18-limits.md) · [Contents](README.md) · [Next: Errors →](20-errors.md)

# 19. Calling rules in callbacks

This chapter describes the rules to follow when you call Session methods from tool, hook, command, task and event callbacks. Following them keeps a callback from waiting on the session it runs in, which would cause a deadlock.

In this chapter: [19.1 The rules](#191-the-rules) · [19.2 Why the rules exist](#192-why-the-rules-exist) · [19.3 Common scenarios](#193-common-scenarios)

## 19.1 The rules

Tools, hooks, commands, a task's `Run` and event callbacks can all run on the session's execution path. When you call session methods from these callbacks, handle each method as the following table describes:

| Method | Behavior in a callback |
|---|---|
| `Send`, `Command`, `Interrupt`, `Withdraw`, `InterruptWith`, `Snapshot`, `Info`, `Subscribe`, `Emit`, `Announce`, `Tasks().Get`, `Tasks().List`, `Tasks().Stop`, `Tasks().Send`, and registration and revocation | You can call these at any time. They do not block. |
| `Tasks().Start`, `Tasks().Restart` | You can call these. They run the `task_start` hook and `TaskKind.New` on your goroutine, and a `Restart` that requires approval also waits for the approval result. |
| `SideChat`, `Fork` | You can call these. They take a single snapshot of the current context and do not wait for a turn, but they wait on your goroutine for the new session to finish initializing. |
| `SwitchModel` | You can call it. It returns at once when the target model's capabilities are already cached. When they are not, it has to wait for capability resolution, and if it is called on the session running the current callback, or one of its ancestors, it returns `ErrReentrantWait`. |
| `Receipt.Wait`, `Run`, `Close`, `Compact`, `Clear`, `Rewind`, `WaitQuiet` | If the thing being waited on is the session running the current callback, or one of its ancestors, these return `ErrReentrantWait`. In that case, use a non-blocking entry point such as `Command("compact", ...)` instead. |
| `Tasks().Wait` | Tools and hooks can wait for a task, but a task cannot wait for itself or for an ancestor task. |
| `SideCall` | You can call it, because it does not need to wait for a turn. |

Reentrancy detection relies on the `ctx` that the kernel passes to the callback, and it works only if you pass that ctx on down. If you substitute `context.Background()`, the call bypasses detection and can wait forever, until a timeout finally ends it.

There is one exception: a `WaitIdle` command can call `Compact`, `Clear` and `Rewind`. No turn is running at that point, so they execute directly.

## 19.2 Why the rules exist

A session runs only one turn at a time, and tools execute inside that turn. If a tool waits for the turn to end while the turn waits for the tool to return, each one waits on the other. The same is true of event callbacks. Subscriptions do not drop events by default, so if a callback waits for the turn while the turn waits for the callback to consume an event, the result is again a deadlock.

Also keep the following points in mind:

- The tools in a parallel tool group and the `post_tool_use` hook are called concurrently. See [7.5](07-tools.md#75-execution-order-and-concurrency) for details.
- Synchronous hooks have a timeout, but approval (`approval`) has no timeout by default.
- A task can safely wait on a child session it created with `ChildOf`, for example by calling the child's `Run` or `Receipt.Wait` from the task's `Run`; that is not reentrancy. The reverse is: when a tool or hook in the child session waits on the session or task that owns it, the wait returns `ErrReentrantWait` at once, because it would end up waiting on itself.

## 19.3 Common scenarios

The following table shows the correct way to handle several common needs in callbacks:

| What you want to do | How to do it |
|---|---|
| Have the agent compact its context from a tool | Call `tc.Session().Command("compact", "")`, not `Compact`. |
| Tell the model one more thing from a tool | Put it directly in the tool's result, or call `tc.Session().Announce(...)`. |
| Start a background task from a hook and wait for it to finish | Call `Tasks().Start` and then `Tasks().Wait`. Both are allowed. |
| Send input from an event callback when a particular event arrives | You can call `sess.Send`, but do not call `Wait` on its receipt inside the callback. |
| Replay the last turn from a command | Call `Rewind` in a `WaitIdle` command, then return `CommandResult{Prompt: &input}`. |
| Ask the main session a question from a task | Call `rt.AskInput(q)`, then receive the answer from `rt.Inbox()`. Do not call `Session().Send` and then `Wait`. |

---

[← Previous: Limits and defaults](18-limits.md) · [Contents](README.md) · [Next: Errors →](20-errors.md)
