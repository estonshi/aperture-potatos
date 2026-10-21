[← Previous: Usage, call records and cost](17-usage-cost.md) · [Contents](README.md) · [Next: Calling rules in callbacks →](19-callback-rules.md)

# 18. Limits and defaults

This chapter lists every limit and timeout you can adjust in the SDK, together with its default. If the agent stops unexpectedly or waits for too long, check this chapter first.

In this chapter: [18.1 Limits](#181-limits) · [18.2 Retry policy](#182-retry-policy) · [18.3 Context policy](#183-context-policy) · [18.4 Other agent-level options](#184-other-agent-level-options) · [18.5 Best practices](#185-best-practices)

All defaults come from `spec/data/limits.json`. A field left at its zero value takes the default, and a negative value means no limit. `MaxTruncationContinues` and `ReadAhead` are exceptions: for these two, a negative value turns the corresponding feature off.

## 18.1 Limits

`kernel.Limits` gathers the limits and timeouts that apply while an agent runs. You set them with `kernel.WithLimits`:

```go
kernel.WithLimits(kernel.Limits{MaxSteps: 50, ToolTimeout: 2 * time.Minute})
```

| Field | Default | Description |
|---|---|---|
| `MaxSteps` | No limit | The maximum number of model calls in one turn. When the limit is reached, the turn ends with `max_steps`. In unattended scenarios (background tasks, scheduled runs), set a limit so that a model stuck in a loop does not keep running. |
| `ToolTimeout` | 10 minutes | The timeout for a single tool call. You can override it for an individual tool with `ToolSpec.Timeout`. |
| `ToolCancelGrace` | 3s | The grace period to wait for tools to exit after an interrupt. A tool that still has not exited after this period is recorded as abandoned. |
| `MaxParallelTools` | 8 | The number of tools that run at the same time within a parallel group. |
| `MaxConsecutiveAutoTurns` | 8 | The limit on consecutive turns triggered only by task notifications and reminders. Content injected by asynchronous hooks counts as well. Once the limit is reached, the agent waits for user input. |
| `MaxStopContinuations` | 8 | The maximum number of times stop hooks can ask a turn to keep running. |
| `MaxTruncationContinues` | 1 | The number of times a truncated reply is continued automatically. `WithAutoContinueTruncatedText` must also be enabled. |
| `MaxQueuedInputs` | 256 | The capacity of the input queue. When the queue is full, `Send` returns `ErrQueueFull`. |
| `MaxRunningTasks` | 8 | The maximum number of unfinished tasks in one session, counting both foreground and background tasks. |
| `MaxToolResultChars` | 100000 | The maximum number of text characters of a single tool result that are passed to the model. When a result is longer, half of the limit is kept from its beginning and half from its end, the middle is replaced with a note, and the `OriginalChars` field of `tool.completed` gives the original length. A negative value means no limit. |
| `MaxAsyncHooks` | 64 | The maximum number of asynchronous hooks running at the same time. |
| `HookGrace` | 1s | The grace period to keep waiting for a synchronous hook to return after it times out or is canceled. After this period, the kernel stops waiting. |
| `ReadAhead` | 64 | The number of events read ahead from the model stream, so that a slow event consumer does not slow down network reads. |
| `InitTimeout` | 30s | The timeout for session initialization, which includes rendering the prompt, installing extensions and running the `session_start` hook. |
| `CloseTimeout` | 10s | How long `Close` waits for the worker to exit. |

## 18.2 Retry policy

`provider.RetryPolicy` controls how model calls are retried, and you set it with `kernel.WithRetryPolicy`:

```go
kernel.WithRetryPolicy(provider.RetryPolicy{MaxAttempts: 6, FirstEventTimeout: 10 * time.Minute})
```

| Field | Default | Description |
|---|---|---|
| `MaxAttempts` | 4 | The maximum number of attempts for one model call. |
| `BaseDelay`, `MaxDelay` | 1s, 30s | The initial and maximum backoff delay. The actual delay includes random jitter. If the API returns `Retry-After`, the wait follows that header instead. |
| `MaxElapsed` | 3 minutes | The limit on the total time of all retries combined. |
| `FirstEventTimeout` | 2 minutes | The longest wait from sending the request to receiving the first byte, including the time to connect and to wait for the response headers. Reasoning models usually start streaming their reasoning after anywhere from a few seconds to a dozen or so seconds. Set it to a negative value for no limit. |
| `IdleTimeout` | 2 minutes | The longest gap allowed between two adjacent frames in the stream. Comment lines also count as frames. Set it to a negative value for no limit. |

Only retryable errors trigger a retry. These include rate limiting, overload, transient failures and interrupted streams. Each retry emits a `model.retry` event. Errors that cannot be retried, such as authentication failures and invalid parameters, make the call fail immediately. For how errors are classified, see [20.3](20-errors.md#203-model-call-errors).

## 18.3 Context policy

You set the context policy with `kernel.WithContextPolicy`. Its fields and defaults are listed in [15.1](15-context.md#151-automatic-compaction). If no model capabilities are declared, the SDK assumes a context window of 200k and a maximum output of 64k.

## 18.4 Other agent-level options

The following options are set when you create the agent and apply to the whole agent:

| Option | Effect |
|---|---|
| `WithAutoContinueTruncatedText(true)` | When a plain-text reply is cut off by the output limit, continues it automatically once. Off by default. |
| `WithLogger(l)` | Sets a `*slog.Logger` for warnings from the kernel and extensions. |
| `WithClock(c)`, `WithIDGenerator(g)` | Fix the time and ids for tests; see [21](21-testing.md). |
| `WithTokenCounter(tc)` | Counts tokens exactly when the context nears the compaction threshold. |
| `WithRawProviderEvents()` | Emits `provider.raw` events, which contain the raw stream from the API, for debugging. |
| `WithPanicOnInternalError(true)` | Panics when the kernel hits an internal bug instead of closing the session. Enable it in tests. |
| `WithStorePolicy(p)` | Sets what happens when a store write fails; see [16.1](16-storage.md#161-choosing-a-store). |
| `WithInputMode(m)` | Sets the default input mode; see [5.4](05-input.md#54-the-three-modes-while-the-agent-is-busy). |

## 18.5 Best practices

- Set a timeout for each tool with `ToolSpec.Timeout`. Use the global `ToolTimeout` only as a fallback for cases you did not anticipate.
- Raise `FirstEventTimeout` in two situations: when the input is very long and misses the cache, so that prefill can take a minute or two, or when the model sends nothing until it has generated the answer. Otherwise the request is judged to have timed out and is retried, and every retry pays for prefill again.
- Do not set `MaxSteps` too low. Each batch of tool calls the model makes is one step, and it is normal for a complex task to take twenty or thirty steps. If what you want to limit is time and cost, use tool timeouts and usage events instead.
- In production, set `CloseTimeout` long enough for at least the slowest tool to exit. Otherwise `Close` returns `LeakError`.
- `MaxConsecutiveAutoTurns` keeps the agent from running out of control. When background tasks keep sending notifications and the model keeps responding to them, the agent stops after 8 turns and waits for user input.

---

[← Previous: Usage, call records and cost](17-usage-cost.md) · [Contents](README.md) · [Next: Calling rules in callbacks →](19-callback-rules.md)
