[← Previous: Calling rules in callbacks](19-callback-rules.md) · [Contents](README.md) · [Next: Testing →](21-testing.md)

# 20. Errors

This chapter lists the errors the SDK can return and the situations in which each one occurs. Once you have read it, you can use `errors.Is` and `errors.As` to handle each error in its own way.

In this chapter: [20.1 Sentinel errors](#201-sentinel-errors) · [20.2 Structured errors](#202-structured-errors) · [20.3 Model call errors](#203-model-call-errors) · [20.4 Error events](#204-error-events)

## 20.1 Sentinel errors

The kernel exports the following sentinel errors, which you can check with `errors.Is`:

| Error | When it occurs |
|---|---|
| `ErrSessionClosed`, `ErrAgentClosed` | You call a method on a session or agent that is already closed. |
| `ErrSessionClosing` | It is the cancellation cause of the turn ctx when `Close` interrupts a running turn. |
| `ErrQueueFull` | The input queue is full, or a task's inbox is full when you send it instructions. Reminders are merged first, so only human, command and agent inputs can run into a full queue. `Withdraw` and `InterruptWith` can also return this error when the caller sends requests much faster than the session can process them. |
| `ErrNotQueued` | The input you pass to `Withdraw` or `InterruptWith` is not in the queue: for example, a turn has already taken it, it has already been dropped, or the id refers to a queued command. Calling `Withdraw` on a task notification also returns this error. The error describes only the moment of the call, and an input that has been taken but not yet delivered can return to the queue. See [5.7](05-input.md#57-withdrawing-a-queued-input-or-sending-it-now). |
| `ErrReentrantWait` | A callback waits on the session that is running it. See [19](19-callback-rules.md). |
| `ErrBusy` | A `RejectBusy` command finds a turn running. You get this error from the receipt's `TurnResult.Err`; `Send` and `Command` themselves never return it. |
| `ErrDuplicate`, `ErrInvalidName` | A registration is a duplicate, or a name is invalid. |
| `ErrInvalidOrigin`, `ErrEmptyInput`, `ErrInvalidMode` | The input passed to `Send` is invalid. |
| `ErrUnknownCommand` | The command you invoked does not exist. |
| `ErrCapability` | The model capabilities do not allow the current configuration. |
| `ErrNoMatcher`, `ErrAsyncNotAllowed` | The first means the hook point does not support matchers. The second means the hook point accepts only synchronous hooks. |
| `ErrBlocked` | A hook blocked the operation. When `pre_compact` blocks `Compact`, `Compact` returns this error directly. When `command_invoke` blocks a command, the error appears in the receipt's `TurnResult.Err`. |
| `ErrSessionOpen`, `ErrSessionNotFound`, `ErrSideChat`, `ErrChildSession` | `NewSession` or `Resume` runs into a situation it does not allow. See [16.2](16-storage.md#162-resuming-a-session). |
| `ErrNoLabelStore` | The store does not implement `store.LabelStore`, so you cannot call `SetLabels` or `FindSessions`. See [16.5](16-storage.md#165-session-labels). |
| `ErrSideChatUnsupported` | You open a side chat inside a side chat, or pass `ForkAt` to `SideChat`. See [15.4](15-context.md#154-forks-and-side-chats). |
| `ErrTurnNotFound` | `TurnCalls` cannot find the requested turn: the turn is older than the 100 most recent turns kept in memory, and no `CallStore` is configured. |
| `ErrNoCostSource` | You call `TurnCost` without configuring a cost source. |
| `ErrNotMovable` | `BackgroundCall` cannot find a call that can be moved to the background. |
| `ErrTaskNotFound`, `ErrTaskNotContinuable`, `ErrTaskNoInstructions`, `ErrTaskNotRestartable`, `ErrTaskLimit`, `ErrTaskBusy` | Task errors. `ErrTaskNotFound` means the task does not exist. `ErrTaskNotContinuable` means the task has already ended and its kind does not support continuing it, so restart it instead. `ErrTaskNoInstructions` means tasks of this kind do not accept additional instructions. `ErrTaskNotRestartable` means tasks of this kind cannot be restarted. `ErrTaskLimit` means the number of running tasks has reached the limit. `ErrTaskBusy` means the task is being restarted or continued; try again later. |
| `ErrNoCompactor` | You call `Compact` without a registered compaction strategy, for example because `std.Defaults()` is not installed. |
| `ErrNothingToCompact`, `ErrUnknownTurn`, `ErrUnknownPin`, `ErrEmptyQuery`, `ErrUnknownLine` | The arguments to a context management call, or the current state, do not allow the operation. |
| `ErrSubscriberStalled`, `ErrSubscriberOverflow`, `ErrChannelClosed` | The reason a subscription was detached. You can get it from `Subscription.Err()`. |
| `ErrSeqConflict`, `ErrIncompatibleStore` | The first means the rows in the store do not match the in-memory state, usually because there is a second writer. The second means the storage format is incompatible. Either error makes the session fail. Both are re-exports of errors from the `store` package. |

## 20.2 Structured errors

The following error types carry structured fields. Use `errors.As` to extract them:

| Type | Fields | When it occurs |
|---|---|---|
| `*kernel.InputError` | `Detail` | `NewTool` fails to decode the tool input. The kernel turns the error into an `invalid_input` result and does not call the tool. |
| `*kernel.InterruptError` | `Reason` | It is the cancellation cause of an interrupted turn's ctx. |
| `*kernel.HookError` | `Kind` (timeout, error or panic), `Err` | It is passed to the hook point's `Closed` when a hook fails. The model sees only `Kind`. |
| `*kernel.LeakError` | `Workers` | `Close` timed out while some workers had still not exited. |
| `*kernel.TaskDeniedError` | `Text` | The `task_start` hook refused to start the task, or approval denied a task restart. `Text` is an explanation written for the model. |
| `*kernel.CostError` | `Status`, `Message` | A `CostSource` uses it to report the HTTP status returned by the billing backend. |
| `*provider.Error` | See [20.3](#203-model-call-errors) | A model call failed. |

## 20.3 Model call errors

When a model call fails, both the `Error` of the `turn.completed` event and `TurnResult.Err` carry an error class. Use `provider.AsError(err)` to extract the `*provider.Error`, which has the following fields:

| Field | Description |
|---|---|
| `Class` | The error class. Its values are listed in the next table. |
| `API`, `Status`, `Type`, `Code`, `Message` | The API that failed, the HTTP status, the error type and error code returned by the API, and the error message. |
| `RetryAfter` | How long the API asks you to wait. |
| `RequestID` | The request id, which you use when you contact the vendor's support. |
| `MidStream` | True when the stream broke off partway through. |
| `NoRetry` | True when the server explicitly asked the client not to retry. |
| `Limit` | For an output limit error, the limit parsed from the error message. |
| `Raw` | The raw error response body. |

`Class` takes the following values:

| `Class` | Meaning | Retried |
|---|---|---|
| `transient` | A temporary failure, such as a 5xx status or a dropped connection. | Yes |
| `rate_limited` | The request was rate limited. | Yes |
| `quota` | The quota has run out. | No |
| `auth` | Authentication failed. | No |
| `invalid_request` | The request is invalid. | No |
| `context_overflow` | The context exceeds the context window. The kernel compacts the context first and then retries. | Once, after compaction |
| `reasoning_replay` | The API rejected the reasoning sent back to it. The kernel downgrades the way it sends reasoning back and tries again. | Once |
| `not_found` | The model does not exist. | No |
| `capability` | The current configuration was rejected locally, based on the declared capabilities. | No |
| `canceled` | The call was canceled. | No |

Call `err.Retryable()` to find out whether an error can be retried.

## 20.4 Error events

The `error` event corresponds to `event.Error`, which has the fields `Code`, `Message`, `Fatal` and `Detail`. When `Fatal` is true, the session closes because of the error. A common `Code` is `store_failed`, which means a write to the store failed; under the `StoreWarn` policy, the write is retried.

> Note: Retries of model calls are reported through the `model.retry` event. They do not emit an `error` event.

---

[← Previous: Calling rules in callbacks](19-callback-rules.md) · [Contents](README.md) · [Next: Testing →](21-testing.md)
