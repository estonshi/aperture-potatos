[← Previous: Quick start](02-quickstart.md) · [Contents](README.md) · [Next: Connecting models →](04-models.md)

# 3. Core objects

This chapter introduces the objects and terms you will use again and again in your code. We recommend getting familiar with them before you read the later chapters. The method reference tables in this chapter also work as an index: come back to them when you need to find the chapter that covers a method.

In this chapter: [3.1 Main objects](#31-main-objects) · [3.2 Terminology](#32-terminology) · [3.3 Agent methods](#33-agent-methods) · [3.4 Session methods at a glance](#34-session-methods-at-a-glance) · [3.5 Session options](#35-session-options) · [3.6 Inspecting state](#36-inspecting-state) · [3.7 Concurrency and lifecycle](#37-concurrency-and-lifecycle)

## 3.1 Main objects

When you use potatOS, you mainly work with the following four objects.

**`*kernel.Agent`**: A configuration that holds the nickname, the prompt, the model, and the registered tools and extensions. One Agent can create many sessions. You create it with `kernel.New(opts...)`.

**`*kernel.Session`**: A conversation that holds the conversation history, the running tasks and the subscribers. A Session runs at most one turn at a time. You create a new one with `ag.NewSession(ctx, opts...)`, or restore one from storage with `ag.Resume(ctx, id)`.

**`*kernel.Receipt`**: The receipt for an input or a command, which you use to wait until it has been processed. It is the return value of `sess.Send(...)` and `sess.Command(...)`.

**`event.Event`**: An event, which carries a type, a sequence number, a time, the source session, the turn, a `Path` and a `Data` payload. You receive events by subscribing; see Chapter [6](06-events.md).

## 3.2 Terminology

**turn**: The span from the moment a batch of input arrives until the model gives its final answer. A turn can contain several model calls.

**step**: One model call, together with the batch of tool executions it leads to.

**epoch**: The span of history between two compactions, clears or rewinds. Within an epoch, the system prompt and the tool definitions stay the same, so requests can hit the model's prompt cache.

**input**: A piece of content you hand to a session, which can be something the user said, a command or a reminder. The input's origin (`Origin`) indicates who provided it.

**reminder**: Background information that the runtime gives the model. The model knows that a reminder is not something the user said and that it does not grant any authorization.

**task**: A kind of background work. When a task starts, it returns an id immediately, runs in the background, and notifies the model on its own when it finishes.

**extension**: A set of registrations packaged together, which can include tools, commands, prompt sections, hooks and more. Built-in features and the extensions you write yourself use the same mechanism.

**side chat**: A conversation attached to the main session and managed by name. Each of its turns carries the main session's current context. You open one with `SideChat`; see [15.4](15-context.md#154-forks-and-side-chats).

## 3.3 Agent methods

| Method | Purpose |
|---|---|
| `kernel.New(opts...)` | Creates an agent from the options. It returns an error if the options are invalid, for example when a tool name is duplicated or an approval rule is malformed. |
| `ag.NewSession(ctx, opts...)` | Creates a new session. By the time the method returns, the first part of the history has already been written, so you can send input right away. |
| `ag.Resume(ctx, id, opts...)` | Restores a session from storage; see [16.2](16-storage.md#162-resuming-a-session). |
| `ag.FindSessions(ctx, labels)` | Finds top-level sessions in storage by label; see [16.5](16-storage.md#165-session-labels). |
| `ag.Derive(o)` | Derives a new agent. It shares the parent agent's registrations but can have its own nickname, instructions and subset of tools; see [13.6](13-extensions.md#136-derived-agents). |
| `ag.Subscribe(ch, opts...)`, `ag.SubscribeFunc(fn, opts...)` | Subscribes to the events of every session under this agent. |
| `ag.Announce(source, text, payload)` | Sends every open session a reminder that does not wake the agent; see [5.5](05-input.md#55-sending-reminders-that-do-not-wake-the-agent). |
| `ag.Nickname()`, `ag.SetNickname(name)`, `ag.Description()` | Reads or changes the nickname, and reads the description. Changing the nickname affects only sessions created afterward. |
| `ag.Logger()` | Returns the logger configured with `WithLogger`. Extensions use it to log warnings. |
| `ag.Close(ctx)` | Closes all sessions concurrently, then ends the agent-level subscriptions. |

## 3.4 Session methods at a glance

The Session methods below are grouped by purpose, and each group names the chapter with the full details.

**Sending and waiting** ([5](05-input.md))

| Method | Description |
|---|---|
| `Send(in, opts...) (*Receipt, error)` | Sends an input and returns a receipt immediately. |
| `Run(ctx, in, opts...) iter.Seq2[event.Event, error]` | Sends an input and iterates over the turn's events until the turn ends. |
| `RunCancelable(ctx, in, opts...) iter.Seq2[event.Event, error]` | Like `Run`, except that ending iteration early stops the work this input started: a started turn is interrupted, and an input still queued is withdrawn. |
| `Command(name, args) (*Receipt, error)` | Runs a command directly, without parsing text. |
| `WaitQuiet(ctx) (TurnResult, error)` | Waits until the session has no work left that would continue on its own: the turn has ended, the queued commands have run, all tasks have finished, and the task notifications have been sent. |

**Interrupting and withdrawing** ([5.6](05-input.md#56-interrupting-a-turn), [5.7](05-input.md#57-withdrawing-a-queued-input-or-sending-it-now))

| Method | Description |
|---|---|
| `Interrupt(reason) bool` | Interrupts the running turn. |
| `InterruptAndDrop(reason) bool` | Interrupts the running turn and discards the human, command and agent inputs in the queue. |
| `InterruptTurn(turnID, reason) bool` | Interrupts only the specified turn. |
| `Withdraw(inputID) error` | Withdraws an input that is still queued. |
| `InterruptWith(inputID) error` | Interrupts the running turn and processes a queued input right away. |

**Subscribing to events** ([6](06-events.md))

| Method | Description |
|---|---|
| `Subscribe(ch, opts...)`, `SubscribeFunc(fn, opts...)` | Subscribes to this session's events. |
| `Events(ctx, opts...)` | Iterates over events on the current goroutine. |
| `Emit(payload)` | Emits an event that you define yourself. |
| `Announce(source, text, payload)` | Sends a reminder that does not wake the agent, and can emit an event along with it. |

**Inspecting state** ([3.6](#36-inspecting-state))

| Method | Description |
|---|---|
| `Info() SessionInfo` | Returns the id, the parent session, the owning agent, the labels and the current epoch. |
| `SetLabels(ctx, changes)` | Merges label changes key by key. An empty string value deletes the label. See [16.5](16-storage.md#165-session-labels). |
| `Snapshot() Snapshot` | Returns the current phase, turn, usage, context usage and running goroutines. You can call it at any time, and it never blocks. |
| `Model() provider.ModelRef` | Returns the model that the next turn will use. |
| `Messages() []message.Message` | Returns every history line in the current context. The returned slice is shared, so treat it as read-only. |
| `Context() ContextReport` | Returns a token breakdown of the context, grouped by source. |
| `Commands() []CommandInfo` | Returns the available commands. |
| `Tools() []ToolSpec` | Returns the visible function tool definitions, after hidden and filtered tools are removed. |
| `HostedTools() []message.HostedTool` | Returns the visible provider-run tools. |
| `ApprovalRules() []ApprovalRuleInfo` | Returns the effective approval rules in evaluation order, each with its scope and whether it can be written to a rule file. |
| `Parent() *Session` | Returns the main session for a side chat, and the owning session for a task's child session. It returns nil for a root session. |

**Context management** ([15](15-context.md))

| Method | Description |
|---|---|
| `Compact(ctx, instructions, opts...)` | Compacts the context manually. |
| `Clear(ctx)` | Clears the context. |
| `SetContextWindow(tokens)` | Sets this session's context window; 0 follows the model. It applies from the next model call; see [15.1](15-context.md#151-automatic-compaction). |
| `Turns()`, `Rewind(ctx, id)` | Lists the turns you can rewind to, each with its first input's id, and rewinds to just before an input or a turn. |
| `Fork(ctx, opts...)`, `ForkAt(id)` | Copies the current context into a new session; `ForkAt` names the cut by an input id or a turn id. |
| `SideChat(ctx, name, opts...)`, `SideChats(ctx)`, `DropSideChat(ctx, name)` | Opens, lists and deletes side chats. |
| `SearchHistory(ctx, q)`, `HistoryLine(ctx, seq)` | Searches the history, including history from before compaction. |
| `Pins()`, `Unpin(ctx, key)` | Lists and removes content that must be injected again after compaction. |
| `SideCall(ctx, sr)` | Sends one request with this session's prompt and model. The request is not written to the history. |

**Tasks** ([12](12-tasks.md))

| Method | Description |
|---|---|
| `Tasks() *Tasks` | Returns the task manager, which provides Start, List, Get, Inspect, Send, Stop, Restart and Wait. |
| `BackgroundCall(ctx, callID)` | Moves a tool call that is running in the foreground to the background. |
| `TaskErrorResult(err)` | Produces the result a tool should return to the model when a task fails to start. |

**Models and the ledger** ([4.6](04-models.md#46-switching-models-at-runtime), [17](17-usage-cost.md))

| Method | Description |
|---|---|
| `SwitchModel(ctx, ref)` | Switches the model, starting with the next turn. |
| `TurnCalls(turnID)`, `TurnCost(ctx, turnID)` | Reports which requests a turn sent and how much it cost. |
| `RecordCalls(bucket, turnID, calls)` | Adds requests that were sent elsewhere on this session's behalf to its ledger. |

**Registration and closing**

| Method | Description |
|---|---|
| `Registrar() Registrar` | Returns the session-scoped registrar, which you use to add tools, commands and more at runtime; see [13.1](13-extensions.md#131-extension-and-registrar). |
| `Close(ctx)` | Interrupts the running turn, waits for the workers to exit, and then emits `session.closed`. |

## 3.5 Session options

`ag.NewSession(ctx, opts...)` and `ag.Resume(ctx, id, opts...)` accept the same set of options:

| Option | Purpose |
|---|---|
| `kernel.WithSessionSink(ch, subOpts...)` | Attaches an event channel when the session is created, so you are guaranteed to receive `session.started`. |
| `kernel.WithSessionFunc(fn, subOpts...)` | Works like the previous option, but receives events through a callback function. |
| `kernel.WithLabels(map)` | Sets labels on the session. The kernel itself does not interpret labels; extensions read them by convention. For example, memory reads `user_id` and `project_id`. `WithInstructionsFunc` can also read these labels. You can change them after the session is created with `SetLabels`; see [16.5](16-storage.md#165-session-labels). |
| `kernel.SessionInstructions(text)` | Sets the user instructions section for this session, overriding the agent-level setting. |
| `kernel.SessionContextWindow(tokens)` | Sets the session's context window at creation or resume, the same as `SetContextWindow`. |
| `kernel.SessionApprovalRules(rules...)` | Adds approval rules for this session. |
| `kernel.SessionInputMode(mode)` | Sets the default input mode for this session. |
| `kernel.SessionUse(exts...)` | Specifies extensions that are installed only for this session. |
| `kernel.ChildOf(rt)` | Used when a task creates a child session. The child session's events and ledger entries are attributed to that task; see [12.2](12-tasks.md#122-taskcaps-and-taskruntime). |

## 3.6 Inspecting state

Call `sess.Snapshot()` at any time to get the session's runtime state. The `Snapshot` it returns has the following fields:

| Field | Meaning |
|---|---|
| `Phase` | The current phase: `initializing`, `idle`, `running`, `maintenance`, `closing` or `closed`. `maintenance` means the session is compacting, clearing or rewinding. |
| `TurnID`, `Step` | The running turn, and the step it has reached. |
| `Epoch` | The number of the current epoch. |
| `Model` | The current model, including the API, the endpoint and the model name. |
| `QueuedInputs` | The number of inputs waiting in the queue. |
| `RunningTasks` | The number of tasks that have not finished yet, counting both foreground and background tasks. |
| `Usage` | Cumulative usage, of type `UsageReport`; see [17.1](17-usage-cost.md#171-usage). |
| `Context` | Context usage, including the amount used, the window size and the compaction threshold. |
| `Workers` | The names of the goroutines that are still running and how long each has been alive. Check this field when you are investigating why a session cannot close. |

The `SessionInfo` returned by `sess.Info()` contains `SessionID`, `ParentSessionID`, `Agent` (the nickname of the owning agent), `Labels` and `Epoch`.

## 3.7 Concurrency and lifecycle

- You can call every public Session method from any goroutine. Methods such as `Send`, `Command`, `Interrupt`, `Snapshot` and `Subscribe` never block. If you need to wait for a result, use `Receipt.Wait`, `Run` or `WaitQuiet`.
- The waiting methods are `Wait`, `Run`, `Compact`, `Clear`, `Rewind`, `Close` and `WaitQuiet`. You cannot call them from inside a callback that the same session is running; if you do, they return `ErrReentrantWait`. See Chapter [19](19-callback-rules.md) for details.
- When you are done, call `sess.Close(ctx)` and `ag.Close(ctx)`. `Close` interrupts the running turn and waits for the workers to exit. The wait is limited by `Limits.CloseTimeout`, which defaults to 10s. If the wait times out, `Close` returns a `*kernel.LeakError` that lists the workers that did not exit.
- `ag.Close` first closes all the sessions it has open, so calling only `ag.Close` when the program exits is also fine.

### Best practices

- Use one Agent per configuration and one Session per conversation. Do not create an Agent for each user. Instead, tell users apart with `WithLabels`, and customize the instructions for individual sessions with `SessionInstructions`.
- Treat Providers and Agents as long-lived objects: create them when the process starts and reuse them from then on.
- Subscribe first, then send input. Use `WithSessionSink` or `WithSessionFunc` when you create the session; otherwise you will miss the `session.started` event.
- In a web service, call `Send` only once per request, and hand the resulting `Receipt` to the goroutine that streams the output so that it does the waiting. Unless your handler is itself an SSE handler, do not use `Run` in an HTTP handler to run the turn to completion.

---

[← Previous: Quick start](02-quickstart.md) · [Contents](README.md) · [Next: Connecting models →](04-models.md)
