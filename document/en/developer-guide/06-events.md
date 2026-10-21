[← Previous: Sending input and waiting for results](05-input.md) · [Contents](README.md) · [Next: Registering tools →](07-tools.md)

# 6. Subscribing to events

Everything that happens while an agent runs is emitted as an event, including streamed text, reasoning, tool calls, task progress and usage. This chapter covers the structure of events, the ways to subscribe to them, how backpressure behaves, and the complete list of events. After reading it, you can build a UI, write logs or set up monitoring on top of them.

In this chapter: [6.1 Event structure](#61-event-structure) · [6.2 Subscribing](#62-subscribing) · [6.3 Backpressure](#63-backpressure) · [6.4 Which session an event belongs to](#64-which-session-an-event-belongs-to) · [6.5 Event reference](#65-event-reference) · [6.6 UI rendering rules](#66-ui-rendering-rules) · [6.7 Passing events across processes](#67-passing-events-across-processes) · [6.8 Emitting custom events](#68-emitting-custom-events) · [6.9 Best practices](#69-best-practices)

## 6.1 Event structure

Every event is an `event.Event`. It is an envelope that carries common information such as the type, the sequence number and the owning session, and it holds the actual content in its `Data` field. `event.Event` has the following fields:

| Field | Meaning |
|---|---|
| `Type` | A string of the form `domain.action`, for example `message.delta`. |
| `Seq` | Increases monotonically from 1 within one root session. Sequence numbers from different sessions have no order relative to each other. |
| `Time` | When the event was emitted. |
| `SessionID`, `Agent` | The session that emitted the event, and the agent's nickname. |
| `TurnID`, `Step`, `Attempt` | The turn, step number and retry attempt the event belongs to. These fields are empty for events that do not happen inside a turn. |
| `Path` | The nesting path. An empty path means the event belongs to this session itself; see [6.4](#64-which-session-an-event-belongs-to). |
| `Data` | The event payload, a pointer to one of the structs in the `event` package. You get it with a type assertion. |

You can serialize a whole `Event` directly with `json.Marshal`, and the result matches `spec/schema/event.schema.json`.

## 6.2 Subscribing

The SDK gives you several ways to subscribe to events. You can attach a channel when you create the session, or later subscribe a channel, register a callback, or iterate over events in the current goroutine:

```go
// 1. Attach a channel when creating the session so you are sure to receive session.started
events := make(chan event.Event, 64)
sess, _ := ag.NewSession(ctx, kernel.WithSessionSink(events, kernel.WithCloseOnDone()))

// 2. Subscribe a channel after the session has been created
sub, _ := sess.Subscribe(ch, kernel.WithTypes("message.", "tool."))

// 3. The callback is called in order on a dedicated goroutine; returning an error ends the subscription
sub, _ = sess.SubscribeFunc(func(ctx context.Context, ev event.Event) error { ...; return nil })

// 4. Iterate on the current goroutine; break cancels the subscription
for ev, err := range sess.Events(ctx) { ... }
```

`ag.Subscribe` and `ag.SubscribeFunc` subscribe to every session of the agent, both the existing ones and those created later. `kernel.WithEventSink` attaches a sink when the agent is created, which likewise guarantees that you receive `session.started` for every session.

You can use the following options when subscribing:

| Option | Effect |
|---|---|
| `WithTypes(prefixes...)` | Receives only events whose type starts with one of these prefixes, for example `"message."`, `"tool."` or `"turn."`. |
| `WithDropDeltas()` | Skips delta events and receives only the final events, which carry the complete content. The delta events are `message.delta`, `reasoning.delta`, `tool_call.delta`, `tool.progress`, `task.progress`, `command.output` and `provider.raw`. |
| `WithMaxDepth(n)` | Receives only events nested at most n levels deep. With 0, you receive only the session's own events. |
| `WithBuffer(n)` | Sets the high-water mark of the subscription queue. The default is 512. |
| `WithStallTimeout(d)` | Detaches the subscriber if it consumes nothing for this long. The default is 60s; a negative value turns detaching off. |
| `WithCloseOnDone()` | Closes the channel after the last event. Use it only for a channel that has no other writers. |
| `WithNative()` | Keeps the model API's raw data in `message.completed`. |

The subscription methods return a `*kernel.Subscription`, which has the following methods:

- `Unsubscribe()` cancels the subscription. The call does not block.
- `Done()` is closed when the subscription ends. After that, the SDK no longer touches your channel or callback.
- `Err()` returns the reason the subscription was detached: `ErrSubscriberStalled`, `ErrSubscriberOverflow`, `ErrChannelClosed`, the error your callback returned, or `ErrSessionClosed`. After you call `Unsubscribe` yourself, it returns nil.
- `Dropped()` returns the number of events that were dropped.

## 6.3 Backpressure

Events are ordered and, by default, never dropped. When a subscriber consumes slowly, only the model stream waits for it; **the session itself never gets stuck**. `Send`, `Interrupt` and `Snapshot` still return immediately.

Keep the following in mind about backpressure:

- Only delta events, that is, events whose payload implements `event.Bulk`, take part in backpressure. Final events such as `turn.completed` are never dropped.
- If you do not want to slow down the model, use `WithDropDeltas` or raise `WithBuffer`.
- A subscriber that still consumes nothing after `WithStallTimeout` is detached, and its `Err()` returns `ErrSubscriberStalled`. The other subscribers receive an `error{code:subscriber_detached}` event, which a monitoring system can raise an alert on.

> Note: Do not wait synchronously for a turn of the same session inside a subscription callback. The callback would be waiting for the turn while the turn waits for the callback to consume its events, and the two would deadlock. In this situation the kernel returns `ErrReentrantWait`; see [19](19-callback-rules.md).

## 6.4 Which session an event belongs to

An empty `ev.Path` means the event belongs to the session itself. Events produced by subagents, in-process peers and background tasks carry a `Path`, in which each hop is an `event.Hop` with the following fields:

| Field | Meaning |
|---|---|
| `Kind` | `task` means the hop was written by the task manager. It can also be a label defined by the caller, such as `side`. |
| `ID`, `Name` | The id and name of this hop, for example a task id or the subagent's type name. |
| `SessionID` | The session this hop sits under. |
| `CallID`, `TurnID` | The tool call and the turn that led to this hop. |

By default, all nested events are forwarded, delta events included. To render only the text of the main conversation, check `len(ev.Path) == 0`. To show subagent output in collapsible groups, group events by `Path[0].ID`.

## 6.5 Event reference

The following table lists the events the kernel emits, by category. Some events are described in detail in other chapters, and the table links to them.

| Category | Event type | Payload type | Key fields |
|---|---|---|---|
| Session | `session.started` | `SessionStarted` | `Source` (new, resume, fork and others), `Model`, `Tools`, `Commands`, `Extensions` |
| | `session.state_changed` | `SessionStateChanged` | `From`, `To` (initializing, idle, running, maintenance, closing, closed) |
| | `session.closed` | `SessionClosed` | `Reason`, `LeakedWorkers` |
| | `system.rendered` | `SystemRendered` | `Epoch`, `Sections` (the slot, name and character count of each section), `Hash` |
| Input | `input.accepted`, `input.injected`, `input.dropped` | See [5.9](05-input.md#59-input-events) | |
| Turns and steps | `turn.started`, `turn.interrupt_requested`, `turn.completed` | See [5.9](05-input.md#59-input-events) | |
| | `step.started` | `StepStarted` | `MessageID`, `API`, `Model`, `Attempt` |
| | `step.completed` | `StepCompleted` | `StopReason`, `Usage`, `TTFTMs` (time to first token), `DurationMs` |
| Model | `model.retry` | `ModelRetry` | `Attempt`, `Error`, `DelayMs` |
| | `model.notice` | `ModelNotice` | `Kind`, `Detail`; see [4.4](04-models.md#44-model-capabilities) |
| | `provider.raw` | `ProviderRaw` | The API's raw events. Requires `kernel.WithRawProviderEvents()`. |
| Messages | `message.started` | `MessageStarted` | `MessageID`, `Role` |
| | `message.delta` | `MessageDelta` | `MessageID`, `BlockIndex`, `Channel` (text or refusal), `Text` |
| | `message.block` | `MessageBlock` | Emitted when a non-text block ends. Fields: `BlockType`, `HostedCall`, `HostedResult`, `Opaque` |
| | `message.completed` | `MessageCompleted` | `Message` is the complete message; `Partial` means the message was interrupted and contains only the blocks that had finished |
| | `message.discarded` | `MessageDiscarded` | `MessageID`, `Reason` |
| Reasoning | `reasoning.started`, `reasoning.delta`, `reasoning.completed` | `Reasoning*` | `Kind`, `Text`; `completed` also has `Replayable` |
| Tool calls (while the model generates them) | `tool_call.started`, `tool_call.delta`, `tool_call.completed` | `ToolCall*` | `CallID`, `Name`, `ArgsDelta` or `Input`, `InputError` |
| Tool execution | `tool.started` | `ToolStarted` | `CallID`, `Name`, `Input`, `Group` (parallel group number), `Movable` (can be moved to the background) |
| | `tool.progress` | `ToolProgress` | `CallID`, `Text`, `Data` |
| | `tool.completed` | `ToolCompleted` | `CallID`, `Name`, `Outcome`, `IsError`, `Content`, `Structured`, `DurationMs`, `OriginalChars` (the original length when the result text exceeded `Limits.MaxToolResultChars` and was truncated; otherwise 0) |
| | `tool.abandoned` | `ToolAbandoned` | A call that did not exit within the grace period after an interrupt |
| Approval | `approval.requested`, `approval.resolved` | See [8.4](08-approval.md#84-events) | |
| Tasks | `task.started`, `task.progress`, `task.status_changed`, `task.instruction`, `task.finished`, `tasks.snapshot` | See [12.5](12-tasks.md#125-events) | |
| Hooks | `hook.started`, `hook.completed`, `hook.failed` | See [9.5](09-hooks.md#95-events) | |
| Usage | `usage.updated` | `UsageUpdated` | `Bucket`, `StepUsage`, `SessionTotal`, `Context` (how much of the context window is used) |
| Context | `context.injected` | `ContextInjected` | Emitted when reminders are delivered. Fields: `Sources`, `Count`, `Truncated` |
| | `context.compaction_started`, `context.compacted`, `context.compaction_failed` | `ContextCompact*` | `Trigger`, `Strategy`, `PreTokens`, `PostTokens`, `Epoch` |
| | `context.cleared`, `context.rewound` | `ContextCleared`, `ContextRewound` | `Epoch` for the former; `TurnID` and `Dropped` for the latter |
| | `side_chat.dropped` | `SideChatDropped` | Emitted when a side chat is dropped because the main session was cleared or rewound. Fields: `Name`, `Reason`; see [15.4](15-context.md#154-forks-and-side-chats) |
| Commands | `command.started`, `command.output`, `command.completed` | `Command*` | `CommandID`, `Name`, `Args`, `Text`, `Data`, `Error` |
| Model switching | `provider.switched`, `provider.switch_rejected` | See [4.6](04-models.md#46-switching-models-at-runtime) | |
| Other | `registry.changed` | `RegistryChanged` | Emitted when registrations change. Fields: `Scope`, `Kind`, `Name`, `Op`, `Effective` |
| | `notice` | `Notice` | Carries a hook's `SystemMessage`. Fields: `Source`, `Text` |
| | `error` | `Error` | `Code` (for example `store_failed`), `Message`, `Fatal`, `Detail` |

Each payload is a struct in the `event` package, such as `*event.MessageDelta`, and you get it with a type assertion. The `event.Type*` constants are the Go names of these event types, for example `event.TypeMessageDelta`.

## 6.6 UI rendering rules

A UI that renders streamed output must follow two rules:

- Every run of delta events is **always followed by a final event that carries the complete content**: `message.completed`, `reasoning.completed` or `tool_call.completed`. Accumulate the deltas by `MessageID` and `BlockIndex` as you render, and when the final event arrives, overwrite what you have with its complete content. This corrects any deltas that were lost.
- When a model call is retried, the model refuses to answer, or a hook rejects the reply, **the deltas already emitted are followed by a `message.discarded`**. The UI should then clear the part it has already rendered.

The events for one assistant message and the tool executions that follow it are emitted in this order:

1. First comes `message.started`.
2. Then come any number of `reasoning.*`, `message.delta`, `tool_call.*` and `message.block` events.
3. The message ends with either `message.completed` or `message.discarded`.
4. Next, each call in the batch of tools emits its own `tool.started`, `tool.progress` and `tool.completed`.
5. Last comes the next `step.started`.

## 6.7 Passing events across processes

Once serialized with `json.Marshal`, events can be sent to another process or to a frontend. If the other end also uses Go, you can restore the events with a decoder:

```go
dec := event.NewDecoder(skills.Payloads()...) // the decoder needs to be told about extension event types
ev, err := dec.Decode(raw)
```

The decoder knows every kernel event. A type it does not know decodes to `*event.Unknown` and is written back unchanged when encoded again, so no data is lost when events pass through a relay. A frontend can dispatch events directly on the `type` field; the JSON fields of each event are in `spec/schema/event.schema.json`.

## 6.8 Emitting custom events

Extensions and tools can emit custom events for the UI or other subscribers. There are two ways to emit one:

- In a tool, call `tc.Emit(payload)`. The event is attributed to that tool call.
- Anywhere else, call `sess.Emit(payload)`.

`payload` can be any type that implements `EventType() event.Type`. We recommend type names of the form `<extension>.<action>`, which keeps them distinct from the built-in events.

The built-in extensions emit their events the same way, for example `skills.Activated`, `memory.Saved` and `mcp.ServerStatusEvent`. These events are defined in each extension's package, and their JSON format is in `spec/ext/<x>/events.schema.json`.

## 6.9 Best practices

- For a chat interface, subscribe to `message.`, `reasoning.`, `tool.`, `turn.` and `approval.`, and render by the rules in [6.6](#66-ui-rendering-rules). If you show only the main conversation, keep only the events where `len(ev.Path) == 0`.
- For logging, use `WithDropDeltas()`, which leaves only a few dozen events per turn. `message.completed` has the complete text, and `tool.completed` has the complete result.
- For monitoring, subscribe at the agent level with `ag.SubscribeFunc` to `turn.completed`, `usage.updated`, `model.retry`, `error` and `session.closed`, and aggregate by `ev.SessionID`.
- Give each consumer its own subscription instead of letting several goroutines compete for one channel.
- Do only quick work in callbacks, such as writing to a buffer or sending to a channel, and hand slow work to another goroutine. Otherwise your callback slows down the model stream.

---

[← Previous: Sending input and waiting for results](05-input.md) · [Contents](README.md) · [Next: Registering tools →](07-tools.md)
