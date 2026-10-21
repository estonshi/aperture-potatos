[← Previous: Testing](21-testing.md) · [Contents](README.md)

# 22. Frequently asked questions

This chapter collects the problems you are most likely to run into when you integrate potatOS, along with how to solve them. When you hit a problem, look for it here first, then follow the links to the relevant chapters.

**Why don't I receive the `session.started` event?**

Attach your subscription when you call `NewSession`, with `kernel.WithSessionSink` or `WithSessionFunc`. If you call `Subscribe` after the session has been created, you miss the events that were already emitted. To subscribe at the agent level, use `kernel.WithEventSink`.

**Why does the model seem to hang?**

There are two common causes. One is a subscriber that is not consuming events: backpressure makes the model's output stream wait for it. The other is an approver that has not returned a decision, because approval has no timeout by default. You can call `sess.Snapshot()` to look at the `Phase`, the running `Workers` and the queue length. If you see a `hook.started` event without a matching `hook.completed`, execution is stuck in that hook.

**How do I get only the final answer?**

Use the `TurnResult.FinalText` that `rcpt.Wait(ctx)` returns.

**How do I display only the streamed text of the main conversation?**

Subscribe to `message.delta` events and keep only the ones where `len(ev.Path) == 0` and `Channel == "text"`.

**Why doesn't the model see my change to a tool definition?**

Tool definitions are frozen at the start of a turn and do not change during that turn. For models that declare `ThinkingPrefixBound`, tool definitions stay the same for the whole epoch. A change therefore takes effect from the next turn, or, for those models, from the next epoch. If a tool's `Spec()` changes dynamically, call `kernel.ToolChanged` whenever it changes.

**Can I put the current time in the system prompt?**

This is not recommended. Use a `ContextProvider` instead; see [11.4](11-prompt.md#114-per-turn-dynamic-content). The system prompt stays the same within an epoch, and putting dynamic content in it breaks the prompt cache.

**The model reports a thinking signature error. What should I do?**

Declare `ThinkingPrefixBound: true` with `WithCapabilities`. If you do not declare it, the kernel downgrades and retries after the first error, and emits a `model.notice` event to let you know.

**Requests are rejected after I switch to another vendor's model. What should I do?**

Some APIs are stricter about the format of the history. For example, DeepSeek with tools requires every assistant message to carry a reasoning field, so requests can be rejected after you switch to it. If this happens, run `/compact` or `/clear` once after switching.

**A reasoning model was retried while it was thinking for a long time. What should I do?**

Increase `RetryPolicy.FirstEventTimeout`. Its default is 2 minutes, counted from when the request is sent.

**How do I make a tool usable only by the main agent?**

Check `in.Path` in a `pre_tool_use` hook. If `in.Path` is not empty, the call came from a subagent or a background task. See the example in [9.1](09-hooks.md#91-registering-hooks) for how to write this.

**Why does calling `Compact` from a tool return `ErrReentrantWait`?**

A tool runs on the session's own execution path, so it cannot wait on that session. Call `tc.Session().Command("compact", "")` instead. See chapter [19](19-callback-rules.md) for details.

**How do I turn off a built-in feature?**

Use `std.Without("<name>")`, where the name is the extension's `Name()`, such as `subagent`, `history`, `tasks`, `compact` or `cmd.fork`. If you only want to keep the model from seeing a tool, use `Registrar.Hide(kernel.KindTool, name)`.

**Why does `Close` return `LeakError`?**

It means that a tool or task did not exit within `CloseTimeout`. Use the names in `LeakError.Workers` to find the workers involved. Tools and tasks need to respond to `ctx.Done()`; you can also increase `CloseTimeout`.

**Why does `Send` return `ErrQueueFull`?**

It means that 256 inputs are still waiting to be processed. The usual cause is a stuck session, not a queue that is too small. To troubleshoot, see "Why does the model seem to hang?" above.

**Can I edit the JSONL file by hand?**

This is not recommended. The kernel treats the history as an append-only record, and if you edit its content and then call `Resume`, tool calls may no longer match their results. If you need to change the history, use rewind, fork or clear.

**How do I run potatOS with multiple processes or replicas?**

A session can be open in only one process at a time. `ErrSessionOpen` is checked only within a single agent, so you must enforce mutual exclusion across processes yourself. You can route requests to a fixed replica by session id, or implement mutual exclusion in your store implementation.

**The model's reply was cut off. What should I do?**

In this case, `TurnResult.Outcome` is `max_tokens`. You can increase `Params.MaxOutputTokens` and also declare the model's `MaxOutputTokens` capability. Alternatively, turn on `WithAutoContinueTruncatedText(true)` so that a plain-text reply is automatically continued once.

---

[← Previous: Testing](21-testing.md) · [Contents](README.md)
