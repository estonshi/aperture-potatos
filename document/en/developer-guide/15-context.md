[← Previous: Built-in extensions](14-builtin-extensions.md) · [Contents](README.md) · [Next: Storage and resume →](16-storage.md)

# 15. Context management

This chapter explains how to control the way conversation history appears in the model's window. It covers automatic compaction, manual compaction, clearing, rewind, forks, side chats, searching history and inspecting the token breakdown. Read it when conversations grow long, or when you need to build features such as "undo" or "branch".

In this chapter: [15.1 Automatic compaction](#151-automatic-compaction) · [15.2 Manual compaction and clearing](#152-manual-compaction-and-clearing) · [15.3 Rewind](#153-rewind) · [15.4 Forks and side chats](#154-forks-and-side-chats) · [15.5 Searching history](#155-searching-history) · [15.6 Custom compaction strategies](#156-custom-compaction-strategies) · [15.7 Pins and SideCall](#157-pins-and-sidecall) · [15.8 Inspecting the context](#158-inspecting-the-context) · [15.9 Best practices](#159-best-practices)

## 15.1 Automatic compaction

The usable window is the session's window (the model's, or one the policy names) minus the maximum output. When the context exceeds 90% of the usable window, the kernel compacts it automatically between two model calls. To compact, the kernel asks the compaction strategy for a summary, then starts a new epoch and places the summary and the Pins at the head of the new epoch. The default compaction strategy is `ext/compact`, which has the model write the summary.

Automatic compaction requires an installed compaction strategy, and `std.Defaults()` already includes one. If no compaction strategy is installed, the kernel never compacts automatically, and the current turn ends with an error once the window is full.

When compaction starts, the kernel emits a `context.compaction_started` event. A successful compaction emits `context.compacted`, which carries the `Trigger`, `Strategy`, `PreTokens`, `PostTokens` and `Epoch` fields. A failed compaction emits `context.compaction_failed`. The `pre_compact` and `post_compact` hooks run before and after compaction. When the new epoch begins, the `session_start` hook also runs, with `Source` set to `compact`.

You can tune automatic compaction with `kernel.ContextPolicy`:

```go
kernel.WithContextPolicy(kernel.ContextPolicy{
	MaxContextTokens: 150000, // the window itself, to correct one the provider does not report
	AutoCompactRatio: 0.85,
})
```

`ContextPolicy` has the following fields.

| Field | Default | Description |
|---|---|---|
| `MaxContextTokens` | The model's window | Sets the window itself, overriding the model's reported value; use it to correct a window the provider does not report or reports wrongly. 0 uses the model's. |
| `AutoCompactRatio` | 0.9 | Automatic compaction starts when the context reaches this fraction of the usable window. |
| `TargetRatio` | 0.1 | The target length of the summary, as a fraction of the usable window. |
| `MinSummaryTokens`, `MaxSummaryTokens` | 20k, 50k | The minimum and maximum summary length. |
| `PinTokens`, `PinTotalTokens` | 5k, 25k | The limit for a single Pin and the limit for all Pins combined. |
| `MaxAutoCompactsPerTurn` | 2 | The maximum number of automatic compactions within one turn. |
| `MaxConsecutiveFailures` | 3 | After this many consecutive failures, the kernel stops attempting automatic compaction. |
| `Disabled` | false | Set to true to turn off automatic compaction. |
| `StopTasksOnClear` | false | Set to true to make `/clear` stop background tasks. |

The window can also change at run time. `sess.SetContextWindow(1000000)` sets this session's window and `sess.SetContextWindow(0)` follows the model again; `kernel.SessionContextWindow(n)` does the same at creation or resume. The new window, and the compaction threshold computed from it, apply from the session's next model call rather than the next turn: widening it in the middle of a turn keeps that turn from compacting at its next step. A fork keeps its source's window.

The window lives in the session file: a change is written with the next line the session writes, and a resume restores the last recorded one (copies record none of their own, so only lines the session wrote count). A host need not pass it again on resume; an option it passes wins over the file.

By default, the kernel estimates the context's token count. If you pass `kernel.WithTokenCounter(tc)`, the kernel switches to an exact count when the context nears the threshold. The Anthropic adapter comes with its own `CountTokens`; for other APIs you need to supply a counter yourself.

## 15.2 Manual compaction and clearing

Besides relying on automatic compaction, you can compact or clear the context yourself. By default, `Compact` compacts the whole context, but options let you compact only the part before or after a given turn. `Clear` simply starts a new epoch.

```go
report, err := sess.Compact(ctx, "Keep every decision about the budget.")        // compact the whole context
report, err = sess.Compact(ctx, "", kernel.CompactUpTo(turnID))                   // compact only the part before this turn
report, err = sess.Compact(ctx, "", kernel.CompactFrom(turnID))                   // compact only the part after this turn
err = sess.Clear(ctx)                                                              // clear the context and start a new epoch
```

The `CompactReport` returned by `Compact` contains the `Strategy`, `PreTokens`, `PostTokens` and `Epoch` fields. If there is nothing to compact, `Compact` returns `ErrNothingToCompact`. If no compaction strategy is installed, it returns `ErrNoCompactor`.

`Clear` starts a new epoch and renders the system prompt again. It also empties the Pin set and discards any reminders queued earlier. Background tasks keep running by default. When the new epoch begins, the `session_start` hook runs with `Source` set to `clear`.

Both are waiting methods. If a turn is running, the call is queued and runs after that turn ends. When you call them from a command whose busy policy is `WaitIdle`, they run immediately. If you call them from inside a tool or a hook, they return `ErrReentrantWait`; use the non-blocking `sess.Command("compact", ...)` instead.

## 15.3 Rewind

Rewind restores the context to its state just before a given turn started. First call `Turns` to get the turns you can rewind to, then pass the ID of the chosen turn to `Rewind`.

```go
turns := sess.Turns()                       // turns you can rewind to, oldest first
report, err := sess.Rewind(ctx, turns[len(turns)-1].ID) // go back to just before the last turn
```

`TurnInfo` contains the `ID`, `Seq`, `Input` and `Started` fields. `Seq` is the sequence number of the turn's first line. `Input` is that turn's input, which a UI can use to let the user edit it and send it again. You can rewind only to turns in the current epoch. History from before a compaction cannot be rewound to; you can only search it.

The `RewindReport` returned by `Rewind` contains the `TurnID`, `Epoch`, `Dropped` and `Input` fields, where `Dropped` is the number of lines discarded. After a rewind, the kernel emits a `context.rewound` event and starts a new epoch, and the `session_start` hook runs with `Source` set to `rewind`. The system prompt stays the same. If the given turn does not exist, `Rewind` returns `ErrUnknownTurn`.

> Note: Rewind changes only the context. Background tasks keep running, queued inputs and task notifications are kept, and external side effects that tools have already caused are not undone. In addition, side chats opened before the rewind point are dropped; see the next section.

## 15.4 Forks and side chats

Forks and side chats both start from the current session, but they serve different purposes. A fork copies the current context into an independent session, after which the two evolve independently. A side chat is attached to the main session and carries the main session's current context on every turn, so you can ask the main session questions at any time while it keeps working.

**Forking** copies the current context into a new, independent session. The new session has its own id and can be resumed like any other session.

```go
branch, err := sess.Fork(ctx,
	kernel.ForkAt(turnID),                   // fork from just before this turn; the default is the current stable history
	kernel.ForkTools("read_file", "search"), // only these tools may be called in the branch; definitions are still sent and other calls are blocked when made
	kernel.ForkSink(ch),                     // subscribe at creation; ForkFunc(fn) is the callback version
)
```

When the new epoch of a forked session begins, the `session_start` hook runs with `Source` set to `fork`.

A **side chat** is a conversation attached to the main session and managed by name. It is saved to the store and can be opened again and again.

```go
side, err := sess.SideChat(ctx, "notes")   // open the side chat named notes; opening it again returns the same one
names, err := sess.SideChats(ctx)          // the open side chats, plus side chats in the store that have not been dropped
err = sess.DropSideChat(ctx, "notes")      // drop the side chat; the next open starts from scratch
```

A side chat does not store the main session's content itself. At the start of each turn, the SDK takes the main session's current context once, puts it in front of the side chat's own history, adds the input for the turn and sends all of it to the model. The side chat therefore always sees the main session's latest progress, including what the main session said after the side chat was opened. If the main session is waiting for a tool result, the request includes a "no result yet" placeholder result for that call, and the placeholder is never written to any store. All steps of a turn use the same copy of the main session's context, so progress the main session makes during that turn becomes visible only on the next turn. If the main session changed between two turns, the SDK adds a reminder before the next turn's input to tell the model that the main session has changed.

The main session content in the request is identical to the prefix of the main session's own next request. As long as the side chat and the main session use the same model, the same system prompt and the same tool definitions, this part reads straight from the main session's prompt cache, and only the side chat's own content has to be written to the cache.

A side chat's context usage counts the main session content, and the breakdown from `Context()` has a `parent` entry that gives its size. Compacting, clearing or rewinding a side chat affects only the side chat's own conversation; the main session content is still included on every turn. If the main session content alone fills the window, the side chat cannot continue until the main session compacts. If the main session exits unexpectedly without closing the side chat first, the side chat's next turn fails, and the side chat then closes with the reason `main_closed`.

A side chat can restrict the tools it may call with `ForkTools`, under the same rules as a fork. Calling `search_history` with `parent: true` in a side chat returns an error, because the main session's current context is already in the request.

A side chat is a separate session. Its events go to its own event stream and are not forwarded to the main session. Subscribe to it directly, or pass `ForkSink` or `ForkFunc` when you open it so that the subscription is attached before the side chat is created. You must open a side chat from the main session with `SideChat`; calling `Resume` with its id returns `ErrSideChat`.

After the main session clears or rewinds, the side chats opened before that point are closed and marked as dropped, a `side_chat.dropped{Name, Reason}` event is emitted, and the next time you open one under the same name it starts from scratch. Compacting or switching models in the main session does not drop side chats; the side chat's next turn carries the main session's context as it stands after the compaction or switch.

You cannot open a side chat inside a side chat, and `SideChat` does not accept `ForkAt`; both return `ErrSideChatUnsupported`. The `/fork` command is rejected in a side chat. To turn a side chat into a standalone session, call `Fork` on the side chat. The resulting session contains the main session's current context plus the side chat's own content, and from then on it is tied to neither of them. If you no longer need the original side chat, call `DropSideChat`:

```go
standalone, err := side.Fork(ctx)       // copy the main session's current context and the side chat's content
err = sess.DropSideChat(ctx, "notes")   // drop the original side chat if you need to
```

## 15.5 Searching history

You can still search history after it has been compacted out of the context. `SearchHistory` finds history lines by keyword, and `HistoryLine` reads the full content of a line by its sequence number.

```go
hits, err := sess.SearchHistory(ctx, kernel.HistoryQuery{Words: []string{"budget", "hotel"}, Limit: 10})
line, inContext, err := sess.HistoryLine(ctx, hits[0].Seq)
```

`HistoryQuery` has the following fields.

| Field | Description |
|---|---|
| `Words` | The search terms. Every term must appear, and matching is case-insensitive. If it is empty, the search returns `ErrEmptyQuery`. |
| `Limit` | The number of results to return. The default is 10 and the maximum is 50. |
| `Current` | Set to true to search only lines in the current context. |

Each result is a `HistoryHit` with the `Seq`, `Epoch`, `Role`, `TurnID`, `Current` and `Snippet` fields, where `Current` tells you whether the line is still in the current context. Results include lines from before compactions and are sorted from newest to oldest. `HistoryLine` reads an entire line and returns `ErrUnknownLine` if the line is not found. The corresponding tool for the model is `search_history`; see [14.2](14-builtin-extensions.md#142-exthistory).

## 15.6 Custom compaction strategies

If you only want to change the summary prompt, `compact.Options` is enough; see [14.1](14-builtin-extensions.md#141-extcompact). If you need a completely different way of compacting, implement the `kernel.Compactor` interface and register it with `Registrar.Compactor`. Each scope can register only one compaction strategy, and a session-level strategy shadows the agent-level one.

```go
type Compactor interface {
	Compact(ctx context.Context, req CompactRequest) (CompactResult, error)
}
```

`CompactRequest` has the following fields.

| Field | Description |
|---|---|
| `Trigger` | Why compaction was triggered: `manual`, `auto`, `overflow` or `switch`. `overflow` means the window has already overflowed and compaction is mandatory. `switch` means the context does not fit in the new window during a model switch; see [4.6](04-models.md#46-switching-models-at-runtime). |
| `Instructions` | Instructions for this compaction, taken from the argument of the `/compact` command or of the `Compact` method. |
| `History`, `HeadLen` | `History` is the current context without the system lines. Its first `HeadLen` lines are the head of the epoch, which the kernel rebuilds, so do not put them in `Replace`. |
| `PrevSummary` | The summary at the start of this epoch, which you can build on. |
| `CurrentTurn` | The line at which the running turn starts. -1 means the session is idle. |
| `Range` | The range to compact when only part of the context is compacted. |
| `Tokens`, `Target`, `Threshold` | The current token count, the target token count for the summary and the automatic compaction threshold. |
| `Caps` | The capabilities of the current model. |
| `Estimate(msgs)` | Estimates the token count of a set of messages. |
| `Summarize(ctx, msgs, instructions)` | Sends one request with the current model that does not enter the history. When `msgs` is nil, it sends the entire current context, which reuses the main loop's cached prefix. |

`CompactResult` has three fields. `Summary` is the summary placed at the head of the new epoch. `Replace` holds the lines to keep verbatim after the summary, such as the turn that is running; if it is nil, no lines are kept. `Strategy` is the strategy name recorded in the epoch boundary.

## 15.7 Pins and SideCall

A **Pin** is content that has to be injected into the context again after compaction. It comes from the `Pin` field of a tool result or the `Pin` of a command result; see [7.4](07-tools.md#74-toolresult). `sess.Pins()` returns all current Pins, oldest first. `sess.Unpin(ctx, key)` revokes a Pin. After that, the text already in the current epoch stays, but the Pin is no longer injected into the next epoch. With the `ContextPolicy` defaults, each Pin can be at most 5k tokens, and all Pins combined at most 25k tokens.

**`SideCall`** sends one request using this session's system prompt, tools and model, but the result is not written to the history. Such calls are a good fit for doing your own summarizing, classification or scoring.

```go
msg, usage, err := sess.SideCall(ctx, kernel.SideRequest{
	IncludeHistory: true,                                  // include the current stable history
	Messages:       []message.Message{message.UserText("Summarize the decisions so far in 3 bullets.")},
	NoTools:        true,                                  // the model may not call tools
	MaxOutputTokens: 1000,
	Bucket:         "summary",                             // record usage in this bucket
})
```

`SideRequest` also has the following fields: `Preamble` is a reminder placed before your messages, `Hop` sets which hop the events belong to, and `Emit` controls whether streaming events are emitted.

## 15.8 Inspecting the context

`sess.Context()` returns a `ContextReport` that describes how many tokens the current context takes up. It has the following fields.

| Field | Description |
|---|---|
| `Model`, `Epoch` | The current model and epoch. |
| `Window`, `Usable`, `Threshold`, `Used` | The window size, the usable window (the window minus the maximum output), the automatic compaction threshold and the number of tokens used. |
| `Basis` | Where the used token count comes from. `reported` means the value the model reported last time, `estimated` means an estimate, and `counted` means an exact count. |
| `Parts` | A breakdown by source. Each entry has `Kind`, `Name`, `Tokens` and `Count`. `Kind` is one of system, tool, summary, pin, epoch_head, user, assistant, reasoning, tool_call, tool_result and reminder. A side chat also has a `parent` entry for the main session content it carries. |

The `/context` command shows this report. In addition, the `Context` field of the `usage.updated` event records how much of the context is used after each model call.

## 15.9 Best practices

- Do not turn off automatic compaction. In long conversations it is the only safeguard against overflowing the window. If you need to tune it, change `AutoCompactRatio` and the Pins instead.
- Make facts that must be remembered into Pins. A summary does not necessarily keep them, but a Pin is injected again after every compaction.
- Consider giving users an "undo last turn" feature. Get the last turn with `Turns()`, call `Rewind`, and put `RewindReport.Input` back into the input box.
- To ask the main session how far it has gotten while it keeps working, use a side chat, which sees the main session's latest state on every turn. To try out a different approach, use a fork.
- Use `SideCall` for auxiliary calls such as summarizing and classification. Its usage is recorded in the session's ledger, and its result does not pollute the history.
- When you call `Summarize` in a compaction strategy, pass nil. It then sends the entire context and hits the main loop's prompt cache.

---

[← Previous: Built-in extensions](14-builtin-extensions.md) · [Contents](README.md) · [Next: Storage and resume →](16-storage.md)
