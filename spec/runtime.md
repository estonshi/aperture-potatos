# Runtime semantics

This document is normative for every binding. It describes behavior, not APIs: bindings name their methods in their own idiom, but what they do must match. A *panic* is a failure that escapes the code that raised it, whatever the language calls it (a Go panic, an uncaught exception).

## 1 Invariants

**I1 History is always valid for the API.** After any exit path (normal end, interrupt, error, panic, hook halt, truncation), every `tool_call` has exactly one `tool_result`, in the tool message right after it. The turn repairs its tail on exit. Before rendering every request, the adapter's projection heals any dangling call once more (result `not_executed`, notice `dangling_tool_call_healed`), so resumed sessions, external stores and future exit paths are covered too.

**I2 A tool runs at most once.** Only tool calls in a fully committed assistant message are executed (§4.2). Retries replay only the model call. An execution ledger keyed by call id removes duplicates. On resume, the torn batch at the end of the file is dropped first (§3). Every unpaired call then gets a `lost` result and every unfinished task is marked `lost`. Nothing is re-run automatically.

**I3 The session's state owner runs no user code.** Tools, hooks (reviewers included), event handlers, provider HTTP, stores, memory and skill sources, prompt sections, context providers, and a task's run and constructor all run on worker or caller threads of execution, bounded by cancellation.

**I4 Event order and completeness.** Within each root session, `seq` starts at 1 and strictly increases. Each subscriber sees an ordered subsequence of that sequence; there is no order across root sessions. A lossless (Block) subscription sees consecutive seqs. DropDeltas drops only bulk events and never a final event. State changes are committed before their events are published. Every line committed to history gets a `message.completed`. When a new epoch opens, each of its lines (the system line and any copies included) gets its `message.completed` in order, and only then does the summary event follow (`context.compacted`, `context.cleared`, `context.rewound` or `provider.switched`). The line sequence rebuilt from events alone equals the session file, except for native raw data, storage batch markers, and a side chat's dropped mark, which is written after the side chat closed (§12.8).

**I5 Public methods are never blocked by slow consumers.** Only the explicit waiting methods block: waiting on a receipt, iterating a run, close, compact, clear, wait-quiet, waiting on a task, turn cost (it waits for the cost source and for retries of missing entries), and switching models (only while the target's capabilities are not yet cached; bounded by cancellation and the init timeout). All other public methods are non-blocking. Only these run user code synchronously on the caller's thread:
- starting a task, restarting it, or sending it an instruction that triggers a continuation runs the `task_start` hook, within its timeout. A restart that must pass the approval gate first waits for the verdict, bounded by the caller's cancellation.
- inspecting a task calls the task's inspector.
- switching models calls the capability resolver, and turn cost calls the cost source.
- constructing a task runs the kind's constructor.

A restart never waits for the attempt it replaces. A waiting method called re-entrantly from a callback of the same session chain fails with a re-entrant-wait error. Capability resolvers and cost sources MUST NOT wait on the session chain either.

**I6 History only grows within an epoch.** Committed lines are never modified. Compaction, clear, and a provider switch that must refreeze tools each open a new epoch: a system line carrying a `boundary` comes first, then the new epoch's lines. Carried-over lines are copies with `copy_of`. Old lines stay in the file for audit. The system prompt is frozen within an epoch.

**I7 Every concurrent worker has an owner.** Each is registered with a name, an owner and an exit condition. Task workers belong to the task manager. After close, the worker count is zero, or close returns a leak report; it never hangs silently.

**I8 Sinks belong to the caller.** The runtime never closes a channel or other event sink the caller passed in, unless the caller asked for close-on-done; then the subscription closes it when it ends for any reason. A caller closing its sink before the subscription ends is misuse; the runtime only guarantees it does not crash.

**I9 A request never ends with an assistant message.** The only exception is replaying Anthropic `pause_turn` as is. Any continuation first commits a user-side continue reminder.

**I10 Parameters are frozen for a turn.** Thinking configuration, effort, the tool set and the provider and model do not change within a turn; changes by hooks or the API take effect from the next turn. For models whose thinking signatures bind the prompt prefix (declared by the caller as a capability), the tool definitions sent to the model are frozen per epoch together with the system prompt. The freeze points are epoch start and the moment a provider switch takes effect. Tool registry and enum changes within an epoch wait for the next epoch. A switch that must refreeze tools opens a new epoch with boundary reason `switch`.

## 2 Events

### 2.1 Ordering

**O1 Monotonic.** Seqs on one root bus strictly increase. Within each (subscriber, root session) pair, events arrive in ascending seq with no reordering or duplicates. A Block subscriber sees consecutive seqs. There is no order across root sessions.

**O2 Causal.** Two events published one after the other by the same thread of execution keep that order. Hence:
- For one block, `*.delta` comes before `*.completed`.
- `message.completed` comes before its `step.completed`.
- `tool_call.completed` comes before its `tool.started`.
- For one call, `approval.requested` and then `approval.resolved` come before its `tool.started`. The first `tool.started` of a batch waits until every approval in the batch is resolved.
- Every `tool.completed` of a batch comes before the next `step.started`.
- Every event under a task hop comes after that attempt's `task.started`.

**O3 State-consistent.** State-changing events (`message.completed`, `turn.*`, `session.state_changed`, `context.*`, and `task.*` except `task.progress`) happen in the same step as the state change, in a fixed order: publish the snapshot, then publish the event, then resolve waiters. So after `turn.completed`, or once a receipt wait returns, a snapshot already reflects the change. Likewise, after `task.finished`, or once a task wait returns, the task's info already shows its new state.

**O4 No order without causality.** Side calls, tasks and child sessions interleave with the main loop, with only O1 between them. Consumers split events by `path` and group task events by `task_id`.

**O5 Paired.** Each `*.started` has exactly one matching `*.completed`, or the whole step is voided by one `message.discarded` (retry, refusal, hook reject, empty message). Specifically:
- `tool.started` pairs with `tool.completed` or with `tool.abandoned`, never both.
- `task.started` pairs with that attempt's `task.finished`. The exception is the `task.finished{status:lost}` a resume emits, which has no `task.started`.
- Each `approval.requested` has exactly one `approval.resolved`.
- Each `command.started` has exactly one `command.completed`. A command that `command_invoke` blocked has only `command.completed{error:"blocked"}`.
- Each `hook.started` has exactly one `hook.completed` or `hook.failed`.
- Each turn has exactly one `turn.completed`.
- Each accepted queue item either enters history in a user message or gets exactly one `input.dropped`; items still queued when the session closes get `session_closing`.
- `session.closed` is the last event.

### 2.2 Delivery

**G1** For each root session, a subscriber's events are an ordered subsequence of that session's seq sequence.

**G2 (Block, the default)** Lossless. A slow consumer slows only bulk producers (model streams, progress, relays), never the state owner, public methods or non-bulk events.

**G3 (DropDeltas)** Only bulk events are dropped; final and lifecycle events never are. The loss shows as seq gaps and in the subscription's dropped count. Final events carry the full content, so dropped deltas do not affect correctness.

**G4 Failures are explicit.** A detached subscriber's subscription reports done with a reason, and the other subscribers receive `error{code:subscriber_detached}`. Events are never dropped silently and delivery never stalls silently. Block's losslessness assumes the subscriber stays within the hard limit and the stall timeout.

**G5 Bounded memory.** Each subscriber holds at most its hard limit of events.

**G6 Close.** When a session closes, the pump delivers what is still queued (the last event is `session.closed`), then reports done. With close-on-done, the caller's channel is closed however the subscription ends, unless the caller closed it already. Publishing on a closed bus is a no-op, so late publishes from abandoned workers never touch a closed channel.

### 2.3 Advisory events

`model.anomaly` reports a repetition a binding noticed while a block was still streaming: `message_id`, `block_index`, `channel` (`text`, `refusal` or `reasoning`), `kind` (`repetition`), `offset` (the characters the block had emitted) and `detail` (`mode` `suffix`, `line` or `block`, `unit_chars`, `repetitions`, and a `sample` of at most 256 trailing characters). It comes right after the delta that triggered it, and a binding emits at most one per block and mode.

Detection is binding-defined: a binding MAY implement none of it. What a binding MUST NOT do is let a report change anything. The message, the turn's outcome and the history stay exactly as they would have been without it. A host may act on the event, for example by interrupting the turn or rejecting the reply when it completes; that is the host's decision, not the runtime's.

## 3 Messages and the session file

A session persists one thing: every committed message, one JSON line each, append only. `schema/message.schema.json` is the line format, so any binding can read and resume a session another binding wrote.

**RULE-MSG-001 Empty content.** Adapters and partial commits drop text blocks whose text is empty. An assistant message with no block left is never committed; a `message.discarded{reason:empty}` is emitted instead. Reasoning blocks are never dropped for having empty text; a redacted block has none.

**RULE-MSG-002 Native data.** `native.raw` is kept semantically verbatim, and only an adapter of the same API reads it back. The only allowed transformation is a listed stripping rule: currently only that OpenAI Responses drops item `id`s before replay when `store` is false.

**RULE-MSG-003 Unknown content.** Unknown fields on a message or a block, and blocks of unknown type, are kept and written back verbatim whenever the line is copied or rewritten. Nested objects (`origin`, `boundary`, task references) carry no unknown fields.

### 3.1 File rules

**RULE-STORE-001** One session is one file, `<session_id>.jsonl`. Each line is one complete message. Lines are only appended. `seq` starts at 1 and strictly increases. Every line carries `v` (currently 1).

**RULE-STORE-002** Every epoch starts with a `role=system` line carrying `boundary`. The first line of the file has boundary `{reason:"new", session:{…}}` and epoch 1; each later boundary line adds 1.

**RULE-STORE-003** The current context is the last line carrying a boundary plus every line after it. Earlier lines stay for audit.

**RULE-STORE-004** Clear, compaction, rewind, a fork, opening a side chat, and a provider switch that must refreeze tools each open a new epoch with a new system line, as in the table below. A copy equals its original except for `id`, `seq`, `epoch`, `ts`, `copy_of` (the original's id) and batch markers (a fork's copies are in another file, and its boundary's `from` names the source session), and it keeps the original's unknown fields. The one exception is a compaction that clears old tool results: such a result's content becomes the `tool_result_cleared` text, the block carries `cleared: true`, and its `pin` and `structured` are dropped. Compaction copies only the lines it keeps. Only a provider switch that refreezes tools copies all remaining lines of the previous epoch. A system line's `tools` are the tool definitions frozen at epoch start. Models whose thinking signatures bind the prefix use them unchanged for the whole epoch; other models freeze from the current registry at the start of each turn.

| Case | Boundary | System line | Lines after it |
|---|---|---|---|
| New session | `{reason:"new", session}` | Freshly rendered system and tool definitions | The epoch head, when it has content |
| Clear | `{reason:"clear"}` | Re-rendered | The epoch head, when it has content |
| Compaction | `{reason:"compaction", trigger, strategy, pre_tokens}` | Re-rendered | The epoch head the runtime rebuilds (summary, pins, instruction memory, `post_compact` context), copies of the lines the compactor keeps, and a continue reminder when the turn is still running |
| Compaction up to a turn | As compaction, plus `range:{kind:"up_to", turn_id}`, and `replay_fallback:true` on a model whose thinking signatures bind the prefix | Re-rendered | The rebuilt epoch head, then copies of the chosen turn's lines and every later line |
| Compaction from a turn | As compaction, plus `range:{kind:"from", turn_id}` | The current system line, copied | Copies of the lines before the chosen turn (the old epoch head among them), then one line with origin `epoch_head`: the summary, the pins the summarized part declared, and the `post_compact` context |
| Provider switch that refreezes tools | `{reason:"switch"}` | System kept, tools refrozen for the new target | Copies of the previous epoch's remaining lines |
| Rewind | `{reason:"rewind", turn_id}`, plus `replay_fallback:true` when the current epoch has it | The current system line, copied | Copies of the lines before the chosen turn, the epoch head among them |
| Fork | `{reason:"fork", from:{session_id, seq}}`, plus `session` on the file's first line, and `replay_fallback:true` when the source epoch has it or, on a model whose thinking signatures bind the prefix, when the source is a side chat | The source epoch's system line, copied | Copies of the source lines up to `seq`; for a side chat, copies of its main session's stable prefix first (§12.8) |
| A side chat opened for the first time or after a drop | `{reason:"side_chat", from:{session_id, seq}}`, `seq` being the main session's last line, plus `session` on the file's first line | The main session's current system line, copied | The side chat's epoch head, starting with the `side_chat_head` reminder; no main-session line is copied |
| Dropped side chat | `{reason:"dropped"}` | No content; a mark only | None |

**RULE-STORE-005** Provider-private data (signatures, `encrypted_content`, `reasoning_details` …) lives in the `native` of the block or the message, stored as a JSON value verbatim.

**RULE-STORE-006** Not stored: events, side-call questions and answers, the input queue, task runtime state, and extension runtime state. A task is recorded only through lines that carry `tasks` references:
- tool results that start, continue, restart or stop a task;
- task notifications.

A status query's result line carries no reference. A reference holds the spec, the state from the task's latest save at commit time, and the status at commit time; an attempt that was asked to stop and has not returned is referenced as `canceled`, the only end it can reach, since a silent stop leaves no later line to record it. Saving state writes nothing by itself; the state lands with the next line that references the task.

**RULE-STORE-007** Readers keep the unknown fields of messages and blocks and write them back on copy and rewrite. Unknown block types are kept as opaque blocks. A line whose `v` is higher than the reader supports is an incompatible-store error.

**RULE-STORE-008 Batches.** When one write carries several lines, every line carries the same `batch` (`b_…`) and the last one also carries `batch_end: true`. Single-line writes carry neither. Examples are a new epoch's system line with the lines after it, or a backlog written in one go. Readers drop a batch at the end of the file that has no `batch_end`, and a half line at the end. So a batch is either wholly visible or absent, and a crash never leaves half an epoch.

**RULE-STORE-009 Single writer.** A session has one writer at a time. Writing a `seq` that already holds a different line fails with a seq-conflict error; the line is neither overwritten nor forked. A retry that repeats a stored line, possibly in another batch, is skipped. A runtime treats a seq conflict as fatal to the session, whatever its store policy.

**RULE-STORE-010 Calls file.** A store may also keep a session's model call ledger: one record per line, each a `model_call` (`schema/common.schema.json`) with a `seq` that strictly increases. Records are only appended. A retry that repeats a stored seq is skipped. Readers drop a half line at the end and skip a line without a seq. The reference store writes it as `<session_id>.calls.jsonl` next to the session file, so no session id may end in `.calls`, and listing sessions leaves calls files out.

**RULE-STORE-011 Label record.** A store may also keep each session's labels in a record of its own: a `label_record` (`schema/common.schema.json`) holding the session id, the parent session id for a side chat or a child session, and the labels. A save replaces the record whole. The reference store writes it as `<session_id>.labels.json`, first to a temporary file in the same directory that is then renamed over the old one, and listing sessions leaves it out. The first line's `session.labels` keep the labels at creation; the record holds the current ones (§4.7).

### 3.2 Resume

Resuming a session that the same agent already has open fails, and so does resuming a file whose current epoch is a dropped mark, or a side chat's file: a side chat is reopened only through its main session (§12.8), which gives it its parent and drops it when it is stale. Resuming a child session's file fails too unless the resume makes it a child of a running attempt (§13.10), since without an owner its events, calls and waits would belong to no task. Otherwise the runtime does the following:

1. Load the file (with the torn batch already dropped) and take the current context by RULE-STORE-003.
2. The system line supplies the system prompt and the tool snapshot. The system prompt is reused as is. For models whose thinking signatures bind the prefix, the frozen tool definitions are reused as is too, because re-rendering would change the prefix and invalidate existing signatures. The model comes from the agent's current configuration.
3. Rebuild the pin set from the epoch head (reminder blocks with source `pin:<key>`) and then from the `pin` of each later tool result. For the same key, the later pin wins. A reminder block with `unpin` retires the pin of that key declared before it.
4. Scan the whole file for `tasks` references, skipping copies: originals are still in the file, and a copy's old status must not override a newer one. The last reference to each task wins. Unfinished tasks are marked `lost`. For each, a task-origin queue item that does not wake the session is queued; it is not written, and it enters history with the next user message. The spec and the state are in the file, so restart and continuation still work after the process restarts. A running foreground task has no reference yet and is not recovered; its dangling call is healed by step 5.
5. Repair the tail: each dangling call gets a `lost` result (the `resume_lost` text, §8). By I2, the call is never re-run. The repaired tool line is committed and persisted like any other.
6. When the store keeps calls, restore the ledger and the usage totals from the calls file (§4.5). When it keeps label records, the labels are the record's, or the first line's when there is none, in which case a record is written; labels given to the resume merge into them as in §4.7 and are written.
7. Run the `session_start` hook with source `resume` (its context arrives as a reminder with the next turn, since the epoch head is already in the file), emit `session.started{source:"resume"}`, and go idle.

## 4 Sessions and turns

### 4.1 Session phases

| Phase | Inputs | History token |
|---|---|---|
| `initializing` | Queued; `run_now` commands wait for the end of initialization | The initializer, which writes the first epoch |
| `idle` | Start work at once when an item can start a turn or a command is queued | Nobody |
| `running` | Queued; an interrupt item or an `interrupt` command also cancels the turn | The turn |
| `maintenance` | Queued; interrupt items and `interrupt` commands wait for the command to end | The running `wait_idle`, `interrupt` or idle `reject` command |
| `closing` | Dropped (`input.dropped{session_closing}`), queued items too | Nobody new |
| `closed` | Rejected with a session-closed error | — |

Only the history-token holder appends lines and calls the store, so store calls never overlap. Every phase change emits `session.state_changed` after the snapshot shows the new phase. A new session ends initialization after its first epoch is written; `session.started` precedes the change to `idle`. `session.started.system_hash` is the lowercase hex SHA-256 of the system section texts joined by one NUL byte.

### 4.2 The turn loop

A turn first applies a pending model switch (§9.4), then runs `user_prompt_submit` over its inputs (§10.1), then commits the user line of its delivery (§5.6), then runs steps until an exit. When the hooks blocked every input that could start the turn, it ends `hook_stopped` before committing anything and its passengers go back to the queue. A step is one model call and the tools it asks for. Tool definitions are frozen when the turn starts (I10), in byte order of their names. On a model with `thinking_prefix_bound`, the definitions sent are the ones on the epoch's system line for the whole epoch, while calls still run against the tools registered now: a call to a tool removed since gets `unknown_tool`. A tool registration change announces `registry.changed{effective:"turn"}`, or `effective:"epoch"` on such a model; so does a change to a registered tool's definition, such as its enum, with `op:"update"`.

After each successful attempt the reply is classified:

| Stop reason | Calls | Next |
|---|---|---|
| `pause` | any | Another step whose request ends with the assistant line (I9's only exception); no user line |
| `max_tokens`, `context_window_exceeded`, reasoning exhausted, or a call with `input_error: incomplete` | yes | Every call gets `not_executed` (`truncated_call_not_executed`); the turn continues while truncation continues remain, else ends `max_tokens` |
| same | no | Ends `max_tokens`, unless automatic text continuation is on and a continue remains: then a continuation (`truncated_text_continue`) |
| `end_turn`, `stop_sequence`, `other` | no | Ends `completed`, unless a `stop` hook continues it (§10.1); an empty reply right after tool results is discarded and nudged once per turn (`empty_reply_continue`, `model.notice{empty_reply_continue}`) |
| `tool_use` or any other | yes | Run the calls (§4.4), commit their tool line, next step |
| `refusal`, `content_filter` | any | The reply is discarded (`message.discarded{reason}`), nothing enters history, the turn ends `refused` |

A reply with no block left after dropping empty text is never committed (`message.discarded{empty}`, RULE-MSG-001). A reply whose output was all reasoning also emits `model.notice{reasoning_exhausted}`. A continuation is a user line with origin `continuation` holding one reminder block with source `continue`. The turn runs at most `max_steps` steps and then ends `max_steps`. Truncation continues and automatic text continuation share one count per turn.

| Outcome | When |
|---|---|
| `completed` | The model answered without calls |
| `hook_stopped` | A hook blocked every input, halted, rejected the reply or stopped the turn after a batch, or a reviewer answered `deny_and_stop` (§10, §11) |
| `max_steps`, `max_tokens`, `refused` | As above |
| `interrupted` | An interrupt or a preempting input or command canceled the turn |
| `canceled` | The session closed |
| `error` | A non-retryable error, exhausted retries, a failed strict store write, or a runtime panic |

Every exit path pairs the last assistant line's unanswered calls with `not_executed` (`turn_ended_not_executed`, with the outcome) and persists, whatever canceled the turn (I1). `turn.completed` follows the change to `idle`.

### 4.3 Attempts and retries

Each attempt emits `step.started` and ends either with its reply's `message.completed` then `step.completed`, or with one `message.discarded`. A failed attempt emits `message.discarded{retry}` first, whether or not its response had begun, and `model.retry` after it when the policy retries. Every request to the model settles exactly once as `committed` (its reply entered history, partially or not), `discarded` (its response began but was dropped) or `failed` (it failed before any response), and each settlement emits one `usage.updated` with bucket `main`. The request of a `pause` continuation keeps ending with the assistant line on every retry of its step. Some errors change the request instead of waiting (§9.3); such a retry emits the same `message.discarded{retry}` and `model.retry`, with `delay_ms` 0, and does not count against the retry policy's attempts.

### 4.4 Tools and interruption

A batch first passes the gate, which settles every call that must not run before any call runs, so no verdict depends on another call's result. For each call, in order: a call id that already has a result reuses it and emits `error{duplicate_call}` (I2); an unregistered name gets `unknown_tool`; arguments that are not a JSON object get `invalid_input`; then the input is normalized, `pre_tool_use` runs and the approval rules decide (§11). The calls that need review are reviewed together, and the batch runs only once every review has an answer. A call the gate denied still runs `post_tool_use`.

A tool declared `background` has the `background` property of §13.6 appended to its input schema whenever tools are frozen; a schema that already has one fails its registration. After the gate, and before any group runs, each approved call of such a tool whose final input has `"background": true` is diverted, in call order: its `tool.started` and `tool.completed` are published at once around a `tool_exec` start made with the call's context (§13.9), its result is `task_started`, or the start's failure text (§13.9), and it runs no `post_tool_use`. A `background` value that is not a boolean gets `invalid_input_detail`. Calls with `background` false or absent run as below. A tool declared `background` never sees the property: the runtime removes it before every call.

The other calls run in groups, in call order: consecutive calls of tools declared `parallel` form one group, and every other call is a group of its own. A group starts only after the one before it has settled, so the order of side effects the model asked for holds. Within a group, calls start in call order and at most `max_parallel_tools` run at once (`data/limits.json`; a negative value lifts the bound). A group is named by the batch position of its first call, which `tool.started{group}` carries; `tool.started{movable: true}` marks a call the host may move to the background while it runs (§13.9). Each call emits `tool.started` and then exactly one of `tool.completed` and `tool.abandoned`; within a group they come in the order things happen. The results enter one tool line in call order.

The text of a result a tool returns is capped at `max_tool_result_chars` characters (Unicode code points, counted over its text blocks; `data/limits.json`, 100 000; a negative value lifts the cap), before `post_tool_use` sees it. Past the cap the result keeps its first half of the cap and its last half, with a text block `tool_result_truncated` between them where the cut is; its other blocks stay where they are. `tool.completed{original_chars}` then gives the text's length before the cut. One result larger than the context window could otherwise neither be sent nor be summarized.

An interrupt publishes `turn.interrupt_requested` once per turn, before it cancels, so the event precedes everything the cancellation causes. Then:

- A reply being streamed keeps its closed blocks, plus text and raw reasoning cut where the stream stopped, as a line with `partial: true`; when nothing is left the attempt is discarded as `empty`.
- A running call keeps the result it returns; an error returned after the cancel becomes `tool_interrupted`. A call still running after the tool cancel grace is abandoned (`tool_abandoned`, `tool.abandoned`) and its late result is dropped. The grace starts when the call's cancellation does, so the calls of one group share it and the turn waits it once. Calls that had not started get `tool_not_started`.
- A call that outlives its own timeout without an interrupt gets `tool_timed_out`, after the same grace. An abandoned call gives its place in the group back, so the calls queued behind it still run.
- When the turn's input had reached history, a note is queued as a reminder with source `interrupted`, mode `next_loop`, that does not wake (`interrupted_note`, listing the calls that did not complete).

An interrupt item or an `interrupt` command preempts: every item the canceled turn took but had not committed goes back to the queue. A plain interrupt that lands before the turn's first commit drops the human, command and agent items the turn took (`input.dropped{interrupted}`), because it ends the request it stopped; items a safe point took go back to the queue. Interrupt-and-drop drops those items as well as the queued human, command and agent ones; reminders stay.

### 4.5 Model calls and the ledger

Every HTTP request to a model API is one model call (`schema/common.schema.json#/$defs/model_call`), retried attempts included, so a gateway's logs match one to one. A call's `outcome` says where its response went: `committed` when it became a committed assistant line, a partial one included, whose id is `message_id`; `discarded` when the response began but never reached history, for the reasons `message.discarded` gives; `failed` when the request failed before any response, whether or not a retry followed. A synthetic reply from `pre_model_request` sends nothing and is not a call.

- `response_id`, `request_id` and `headers` come from the start of the response; a request that failed first takes `request_id` from the error. `headers` holds only the response headers the caller named, keys lowercased. A committed call writes the same three onto the message's `producer`.
- Before each request the runtime asks the caller's request-header function, when one is set, with the root session id, the session, the turn, the bucket and the model; the headers it returns are added to that request and win over the request's own headers of the same name.
- The main loop's calls have bucket `main` and belong to the running turn. A call's usage adds to the session totals, its bucket and its model the moment it is booked, and each booking emits one `usage.updated`, so the buckets are sums over the ledger.
- Each booked call gets the next `seq` of the session's ledger, and a main-loop call its `step` and `attempt`. Memory keeps the calls of the latest 100 turns, and of an older turn while a task attempt started by one of its calls still runs; calls outside any turn count toward the totals only. A turn's calls are `pending` while the turn runs or such an attempt does.
- When the store keeps calls (RULE-STORE-010), every booked call is also written to the session's calls file, off the actor and without waiting: a failed write only emits `error{store_failed}` and is retried with the next booked call, whatever the store policy, since the call already happened; closing waits for the last write. A turn memory no longer keeps is read from the file. A resumed session loads the file: memory gets the latest 100 turns, the next seq follows the file's last, and the usage totals, buckets, per-model sums, last step, steps and discards are rebuilt from the calls, retries from the main-loop calls of this session with an attempt above 1, and turns from the distinct turn ids of the file's original lines; abandoned tools count from the resume on. Without such a store the ledger is in memory only and a resumed session starts with an empty one.
- Pricing a turn asks the cost source for every call that has a `response_id`. Calls it has not logged yet are asked for again after 1 s, then after doubling waits of at most 5 s, until all are priced or the caller's deadline passes; the result then lists what is still missing. A network error or an HTTP 429 or 5xx from the source is retried the same way; any other error ends pricing at once and is returned with what was priced so far.

### 4.6 Hosted tools

A hosted tool (`message.schema.json#/$defs/hosted_tool`) is one the provider runs, such as web search. The kernel declares it, freezes it and sends it; it never runs it.

- **Registration:** kind `hosted_tool`, at agent or session scope. A session's shadows the agent's of the same name, and a session may hide an inherited one. Its name follows the tool name rule and is unique among the tools and hosted tools of its scope. Exactly one of `web_search` (named `web_search`, at most one domain list, `context_size` one of `low`, `medium`, `high`), `tool_search` (named `tool_search`) and `raw` (each value a JSON object) is set. A derived agent's tool filter is asked about each inherited hosted tool with a spec that carries only its name.
- **Freezing:** hosted tools freeze with the tool definitions (§4.2, I10), in byte order of their names, and go to the system line's `hosted_tools` wherever its `tools` go, so Resume restores them with the epoch. A hosted tool whose name a frozen tool already takes is left out of that freeze. A registration change announces `registry.changed` as a tool's does. Every request of the session, the main loop's and side calls', carries them as `hosted_tools`; each API mapping says what it sends.
- **No gate:** a hosted call never reaches the kernel's gate. `pre_tool_use`, `post_tool_use`, the approval rules and the reviewers never see it, and it has no `tool.*` events; `post_model_response` sees the whole reply. A host that must control one does not register it, or narrows it with domain filters.
- **Blocks and events:** a reply holds `hosted_call` and `hosted_result` blocks. `message.block` carries each one when it closes, as `hosted_call` or `hosted_result` without native data, and `message.completed` projects them the same way. A result may arrive in a later assistant line than its call.
- **Mixed calls:** when the latest assistant line has a hosted call no hosted result of that line answers, the safe point after its tool line delivers nothing (§5.4): the API runs that call only if the next user message holds tool results alone. The queue items, the `every_step` providers and the hook context wait for the next delivery point.
- **Usage:** `usage.hosted_calls` counts the calls per tool name, and every sum of usage (totals, buckets, models, turns) adds it per name.
- **Deferred tools:** a tool may be declared `deferred`, which its frozen definition records. When a request carries the neutral tool search, each API mapping sends such a definition so that the model loads it only when a search finds it; otherwise it is an ordinary definition. Calls to a deferred tool are gated and run like any other. While the frozen hosted tools include `tool_search`, the context estimate leaves deferred definitions out (§12.1).

### 4.7 Labels

Labels are string pairs the runtime does not interpret; extensions read them by convention. They are set when a session is created, and a fork and a side chat start with their source's. When the store keeps label records (RULE-STORE-011), a session writes its record once its first epoch is written, and a change merges into its labels: a key with a non-empty value is set, a key with an empty value is removed, and other keys stay. An empty key is rejected, and an empty change writes nothing. Changes to one session apply one at a time. The record is saved before the change is visible, so a failed save changes nothing, and a closed session rejects changes. A label query returns the records whose labels hold every given pair, an empty value matching a missing key, and that have no parent session, in session id order: side chats and child sessions are never in it. A label store's calls may overlap a session's other store calls, since they write another record. Without label records, changes and queries fail with a no-label-store error, and labels given to a resume replace the first line's for that opening only.

## 5 The input queue

### 5.1 Items

Every input is an item with an arrival seq, a mode (`queue`, `next_loop`, `interrupt`) and a wake flag. The mode is the input's own, else the session's default, else the agent's, else `next_loop`. Send accepts the origins `human` (the default), `command`, `agent` and `reminder`, and rejects an input with neither text nor parts. A human, command or agent input reserves a queue slot when it is sent and fails with a queue-full error when `max_queued_inputs` are reserved; the slot returns when the item is delivered or dropped. A reminder never fails for capacity (§5.5). Task notifications are items of origin `task` that only the task manager queues (§13.7). An announcement is a reminder item that does not wake, with the source and text its caller gives, queued in one session or in every open session of an agent (not in those of agents derived from it); a closing session gets none, and the caller's event, when given, is published just before the item is queued. Each admitted item emits `input.accepted`.

An item can start a turn when it wakes and is not a command, and, for task and reminder items, while fewer than `max_consecutive_auto_turns` turns in a row had only task and reminder inputs. Other items are passengers: they never start or interrupt a turn and ride with whatever turn delivers next.

### 5.2 Barriers

A queued `wait_idle` or `interrupt` command is a barrier. No item with a larger seq is delivered, by a new turn or at a safe point, before that command has run. Every command gets a seq when it arrives, and the prompt it returns is queued with that seq.

### 5.3 Who starts work when idle

1. The earliest queued `interrupt` command runs.
2. Otherwise, looking only at items ahead of the first barrier, the lead is the first that can start a turn in this order: a human, command or agent item in `next_loop` or `interrupt` mode; a human, command or agent item in `queue` mode; a task or reminder item in `next_loop` or `interrupt` mode; a task or reminder item in `queue` mode.
3. A lead in `next_loop` or `interrupt` mode takes every waking `next_loop` and `interrupt` item ahead of the barrier. A lead in `queue` mode goes alone, except that a human, command or agent lead also takes the waking task and reminder items. Every passenger ahead of the barrier rides along.
4. With no lead, the command at the barrier runs.

The turn's trigger and its user line's origin come from the first human, command or agent item it delivers, else from the first task item, else it is `reminder`. A turn that delivered only task and reminder items counts toward the consecutive automatic turns; any human, command or agent item resets the count.

### 5.4 Safe points

A safe point follows a committed tool line or continuation, when another model call follows in the turn. A tool line whose assistant line holds an open hosted call is not one (§4.6). It takes, in seq order, the `next_loop` items and the passengers ahead of the first barrier. Waking `queue` and `interrupt` items wait for the turn to end. The taken items become a user line committed in the same batch as the tool line or continuation, and emit `input.injected`. Items a turn takes but never commits return to the queue at their old seq.

### 5.5 Capacity and merging

While the queue holds `max_queued_inputs` items and a reminder arrives, the runtime first merges the two oldest reminder items of one source, repeatedly, until there is room or no pair is left. If there is still no room, the new reminder merges into the oldest reminder item with its source, else into the oldest reminder item, else it is queued anyway. Merges never cross a queued command. A merged item wakes if either part woke and keeps the stricter mode (`interrupt` > `next_loop` > `queue`); the runtime then checks again whether it preempts the running turn. A merged item keeps at most 32 parts and 64 KiB of text: its oldest parts are dropped (`input.dropped{merged_overflow}`) and its delivery starts with `merged_omitted`. Delivery and drop events list every part's id. Task notifications never merge and are never dropped for capacity; there is at most one undelivered notification per task (§13.7), so they are queued above the limit when it is reached.

### 5.6 Delivery

A delivery point (a turn's start, a safe point) commits at most one user line. Its blocks, each group in seq order:

1. One reminder block per reminder, text cut at 10,000 code points with `reminder_truncated` on a new line: first the `turn_start` context providers (at a turn's start only), then the `every_step` ones, each with its name as source (§10.7); then one per reminder part, source from its origin; then the context hooks returned since the last delivery point, source `hook:<name>`, in the order they ran (the calls of a parallel group in call order).
2. For each task notification, a text block `<task-notification task_id=ID kind=KIND status=STATUS>`, a newline, the notification's text with runtime tags escaped, a newline, `</task-notification>`, the three values written as attribute values (§5.7). The line's `tasks` references every task it notifies, with its status and state at commit time.
3. For each agent part, a text block `<agent-message from=VALUE>`, a newline, the `agent_message` prefix, a newline, the body with runtime tags escaped, a newline, `</agent-message>`, followed by its attachments, whose text blocks have runtime tags escaped. VALUE is the part's source written as an attribute value (§5.7).
4. For each command and human part, its text with runtime tags escaped, followed by its attachments, whose text blocks have runtime tags escaped too.

A line with reminder blocks emits `context.injected` after its `message.completed`. The receipt of every delivered part resolves when the delivering turn ends. A safe point whose turn ends after it (interrupted, stopped by a hook, or at `max_steps`) takes no queue items and runs no providers, but still delivers hook context.

### 5.7 Runtime tags

Adapters render a reminder block as `<system-reminder source=VALUE>`, a newline, the text with runtime tags escaped, a newline, `</system-reminder>`, where VALUE is the block's source written as an attribute value. Escaping runtime tags replaces the `<` of every opening `<tag` and closing `</tag` of a tag in `data/runtime_tags.json` with `&lt;`. The tag name matches in any letter case, and blanks (space, tab, CR, LF, form feed) may stand between `<`, `/` and the name, since a model does not read tags strictly; the rest of the match is kept as written, so `<SYSTEM-REMINDER` becomes `&lt;SYSTEM-REMINDER` and `< /system-reminder` becomes `&lt; /system-reminder`. Text blocks among an input's parts (§5.6 items 3 and 4) are escaped like its text.

An attribute value is the string with runtime tags escaped, then written as a JSON string by the rules of RFC 8785 §3.2.2.2:

- `"` and `\` get a backslash in front.
- U+0008, U+0009, U+000A, U+000C and U+000D become `\b`, `\t`, `\n`, `\f` and `\r`; every other character below U+0020 becomes `\u` and four lowercase hex digits.
- Every other character, U+007F, U+2028 and non-ASCII ones included, is written as itself in UTF-8.
- Invalid UTF-8 and unpaired surrogates are replaced with U+FFFD first.

The quotes of the value therefore cannot be closed early, and no value can forge a tag. For well-formed input, JavaScript's `JSON.stringify` and Python's `json.dumps(v, ensure_ascii=False)` produce exactly these bytes.

### 5.8 Withdrawing a queued input and interrupting with it

The host may act on one input that is still queued, named by its id.

- Withdrawing removes the item and drops it with `input.dropped{withdrawn}`. Its receipt resolves as dropped and its queue slot returns. In a merged reminder item only the named part is dropped: the item keeps its other parts, its mode and its wake flag, and takes the id of the first part left.
- Interrupting with an item gives it mode `interrupt` and makes it wake, then checks whether it preempts the running turn, as for a new `interrupt` item (§4.4); from then on it is delivered as such an item is. Barriers still hold (§5.2), a session in `maintenance` keeps it until the command ends, and a task or reminder item does not preempt once `max_consecutive_auto_turns` is reached, nor in a child session whose task does not run (§13.10).
- Both act only on a queued item that is not a command. An item a turn has taken, an item already dropped, a queued command and an unknown id fail with a not-queued error and change nothing. Withdrawing also fails this way for a task notification, which only its task withdraws, and for the reminder that retires a pin, which must reach history so that a resume does not restore the pin; interrupting with either works.
- A not-queued error describes the moment of the request only: an item a turn took but never committed may go back to the queue (§4.4, §5.4) and can then be acted on again.
- The runtime takes items and serves these requests one at a time, so an item is either acted on or already taken. A request reaches the runtime after every input the host sent before it, so it never overtakes that input; a turn may still have taken the input by then.

## 6 Commands

Text sent with origin `human` is a command when it starts with `/` followed by a registered name or alias and then whitespace or the end; the rest, trimmed, is the arguments. `//` escapes a literal leading slash. Parsing can be turned off per input.

| Busy policy | While a turn or a history command runs | When idle |
|---|---|---|
| `run_now` | Runs at once, beside it (`command.started{busy:true}`) | Runs at once |
| `wait_idle` | Queued as a barrier in `queue` mode | Runs in `maintenance` |
| `interrupt` | Queued as a barrier in `interrupt` mode and cancels the turn | Runs in `maintenance`, before every other queued item |
| `reject` | `command.started{busy:true}` and `command.completed{error:"busy"}` at once | As `wait_idle` |

Before a command starts, `command_invoke` runs (§10.1). A block ends it with `command.completed{error:"blocked"}` and no `command.started`; rewritten arguments are the ones `command.started` carries. A command ends with `command.completed`; a prompt it returns is then queued as one input with origin `command` and the command's name as source, at the command's seq.

A prompt command expands its template with the arguments. The arguments split into words like a shell: whitespace separates them, single quotes keep everything literally, double quotes group words, and a backslash outside single quotes escapes the next character.

| Placeholder | Becomes |
|---|---|
| `$ARGUMENTS` | The whole argument string |
| `$ARGUMENTS[N]`, `$N` | Word N, counting from 0; empty when there is none |
| `$name` | The word at the name's position, for the names the command declares |
| `$$` | A literal `$` |

Any other `$` stays as it is. When the template has no placeholder of the first three rows and the arguments are not empty, `\n\nARGUMENTS: ` and the arguments are appended.

## 7 System prompt skeleton

The system of a new epoch is its sections in slot order, each one text block: `core` (`prompts/system.core.md`), `identity` (`prompts/identity.md` with `nickname` and `description`), extension sections of the `capabilities` slot, `user` (`prompts/user.md` with `instructions`, left out when empty), then extension sections of the `catalog` and `environment` slots. A prompt file is its content without the final newline. When `catalog` or `environment` sections follow, the last block before them is marked `cache: true` (the agent/session cut, B4 in `mappings/anthropic_messages.md`) if those earlier blocks render alike for every session of the agent: the session sets no instructions of its own, the agent has no instructions function, and the session neither adds nor hides a `capabilities` section. A system rebuilt from a stored line has no mark.

Extension sections sort by order, then by name, within their slot; a session's section shadows the agent's of the same name. A section that renders empty is left out, and one that fails to render is left out with `error{section_render_failed}`. A section added or removed inside an epoch changes nothing until the next epoch, which `registry.changed{effective:"epoch"}` says. When the section asks for it, the change is announced at once as a reminder with source `registry` that does not wake: `section_updated` with the new text, or `section_removed`.

Templates use a subset of mustache: `{{name}}` inserts the value verbatim, `{{#name}}…{{/name}}` keeps its body when the value is not empty, `{{^name}}…{{/name}}` when it is empty, and `{{! … }}` is dropped. There is no escaping, no partial, and no trimming of lines that hold only a tag. Any other tag is an error.

## 8 Fixed texts

`prompts/reminders.md` holds every fixed text the runtime sends to the model; a text is a template with the variables below.

| Key | Where it goes | Variables |
|---|---|---|
| `dangling_tool_call` | Result an adapter gives a call without one when rendering (I1) | — |
| `resume_lost` | Result a resume gives each call the file left without one | — |
| `turn_ended_not_executed` | Result of a call left unanswered when a turn ends | `outcome` |
| `truncated_call_not_executed` | Result of every call of a reply cut off by the output limit | — |
| `tool_not_started` | Result of a call the interrupted batch never reached | — |
| `tool_interrupted` | Result of a call that returned an error after an interrupt | — |
| `tool_abandoned` | Result of a call still running after the grace period | — |
| `tool_timed_out` | Result of a call that outlived its timeout | `seconds`, the limit in seconds, shortest decimal form |
| `tool_result_truncated` | Between the head and the tail of a result past `max_tool_result_chars` | `omitted` and `total`, character counts |
| `unknown_tool` | Result of a call to an unregistered tool | `name` |
| `invalid_input` | Result of a call whose arguments are not a JSON object | `raw`, the arguments as received |
| `invalid_input_detail` | Result of a call a strictly decoding tool rejected | `detail` |
| `empty_reply_continue` | Continuation after an empty reply that followed tool results | — |
| `truncated_text_continue` | Continuation after a text reply cut off by the output limit | — |
| `interrupted_note` | The note queued after an interrupted turn | `partial` (non-empty when the last reply was cut off), `calls` (`name (id)` of each call that did not complete, joined by `, `) |
| `merged_omitted` | First line of a merged item that lost parts | `count` |
| `reminder_truncated` | Appended to a reminder cut at 10,000 code points | `count`, the code points cut |
| `tool_result_media` | Text an adapter puts before the images and documents it moves out of a tool result into the next user message, where the API cannot carry them in the result | `call_id` |
| `document_link` | Text part an adapter sends for a document that has only a URL, where the API has no URL form for files | `url` |
| `tool_removed` | Description of the stand-in definition an adapter sends for a tool a replayed tool search loaded that is no longer among the request's tools | — |
| `denied_by_hook` | Result of a call a `pre_tool_use` hook denied | `reason` |
| `denied_by_rule` | Result of a call an approval rule denied | `id`, `reason` |
| `denied_by_reviewer` | Result of a call a reviewer denied, or that needed review and had no reviewer | `reason` |
| `approval_interrupted` | Result of a call whose review the interrupt or close ended | — |
| `approval_stopped` | Result of a call whose review another reviewer's `deny_and_stop` ended | — |
| `stop_continue` | Reminder of a stop-hook continuation whose hook gave no reason | — |
| `section_updated` | Announcement of a section added inside an epoch | `name`, `text` |
| `section_removed` | Announcement of a section removed inside an epoch | `name` |
| `compact_continue` | Text of the continue reminder a compaction adds while the turn is still running | — |
| `pin_truncated` | Line after a re-injected pin cut to its budget | — |
| `tool_result_cleared` | Content of a tool result a compaction cleared | — |
| `pin_removed` | Reminder that tells the model a pin was retired | `key` |
| `summary_too_long` | Appended to a summary request's instructions when its reply was cut off by the output cap, for the one retry | `target` (the summary's token target) |
| `tools_allowed` | Reminder on a fork's first turn that names the tools the fork's tool filter allows | `names` |
| `tool_not_allowed` | Result of a call to a tool outside the fork's allow-list | `name` |
| `side_chat_head` | First reminder of every epoch head of a side chat | — |
| `side_chat_main_changed` | Reminder before a side chat's input when the main session changed since its previous request | `first` (§12.8) |
| `main_call_pending` | Result a side chat's request gives a main-session call that has no result yet | — |
| `side_chat_compact_scope` | Reminder before the instructions of a side chat's summary request | — |
| `task_limit` | Result of a start tool call refused by `max_running_tasks` | `limit` |
| `task_not_started` | Why `task_start` refused an attempt | `reason` |
| `task_start_failed` | Result of a start tool call whose task could not be made | `error`, the error without the binding's prefix |
| `task_ended` | Content of a foreground call whose task ended without content | `task_id`, `status`, `error` |
| `foreign_truncated` | Appended on a new line to text from another agent or task cut to its limit | `count`, the code points cut |

Two templates are files of their own: `prompts/task_started.md`, the result of a start that leaves the task running (`task_id`, and `moved` when a foreground call timed out), and `prompts/task_notification.md`, a notification's text (§13.7).

Every text block of a tool result, and every name or argument the runtime quotes in one, has its runtime tags escaped.

## 9 Models and capabilities

### 9.1 Where capabilities come from

A model's capabilities (`provider.schema.json#/$defs/capabilities`) are merged field by field, the first non-zero value winning; nested objects merge field by field too, and an empty list counts as zero:

1. The agent's declarations for the model's API whose glob matches the model name, later declarations first. In a glob, `*` matches any run of characters, slashes included, `?` matches one character, and the whole name must match.
2. The resolver's answer: the provider's own, or one the agent sets instead; an agent may also turn resolution off.
3. The default: `context_window` from `data/limits.json`; every other field zero.

A zero field means unknown, and an unknown capability never changes a request. The resolver is asked when a session is created and when a switch is asked for (§9.4), bounded by the init timeout. Its answer is kept for the agent's lifetime, per API, endpoint and model; a model it cannot name is kept as all zero. A failed lookup is not kept, raises `model.notice{capability_lookup_failed, detail{model, error}}`, and the model runs on the other sources. Resolvers must not wait on the session chain.

### 9.2 The output cap and unusable models

The effective output cap is the configured one, or `data/limits.json`'s default when it is zero, clamped to a known `max_output_tokens`; when only the window is known (declared or resolved, not the default), it is clamped to a quarter of the window instead. A clamp raises one `capability_adjusted` notice, and every request carries the effective cap. The context gauge (`usage.updated.context`) uses the model's window and this cap.

A session is not created, and a switch is refused when it is asked for, with a capability error when the model declares `no_tools` and the session has tools, or when the effective cap leaves no usable window (§12.1). The error message states the numbers and the fix.

A notice of kind `capability_adjusted`, `capability_hint`, `hosted_tool_unsupported` or `hosted_tool_adjusted` is announced once per session for each distinct detail, however many requests repeat the adjustment.

### 9.3 Recoveries

Each of these errors allows one retry per step that changes the request (§4.3); a second one ends the turn with the error. A side call (a summary request, a side chat's request outside a turn) recovers the same way, once per call, and the retry spends none of its transient retry budget either.

| Error | Retry | Also |
|---|---|---|
| class `reasoning_replay` | The request is sent with `replay: fallback`, whose form each API mapping defines | `model.notice{reasoning_replay_fallback, detail{message}}`. When the rule is `thinking_signature_bound` and the model does not declare `thinking_prefix_bound`, also `capability_hint{field:"thinking_prefix_bound"}`. That rule also teaches the session that the model binds thinking signatures to the prefix: the retry and every later request to that model in the session go as if it declared `thinking_prefix_bound`, so the retry already takes that model's fallback form, and tool freezing, compaction and side calls follow it too. |
| rule `max_output_too_large` with a parsed `limit` | `caps.max_output_tokens` is the limit | `capability_hint{field:"max_output_tokens", value}` |
| rule `max_tokens_field_unsupported`, on `openai.chat` | `caps.max_tokens_field` switches between `max_tokens` and `max_completion_tokens` | `capability_hint{field:"max_tokens_field", value}` |
| rule `reasoning_summary_unavailable` | `params.reasoning.hide` is set | `model.notice{reasoning_summary_disabled, detail{message}}` |

The last three are learned: every later request of the session to the same model starts from what the error taught. A `reasoning_replay` recovery is kept for the epoch: the refused reasoning stays in its lines, so every later request of that epoch, side calls included, is sent with `replay: fallback` at once; the next epoch starts with full replay again.

### 9.4 Switching models

A switch names the model to use from now on. It resolves the target's capabilities when they are not known yet (waiting for the resolver, bounded by the caller's cancellation and the init timeout; a re-entrant call from the session chain fails with a re-entrant-wait error instead of waiting) and checks it (§9.2). It then takes effect at the start of the next turn, never within one (I10); a later switch replaces one that has not taken effect.

At that turn's start:

1. `provider_switch` runs (§10.1). A block refuses the switch with `provider.switch_rejected{reason:"hook_blocked"}` and the turn runs on the current model.
2. When the context in use (the last reported input plus output tokens) exceeds the target's usable window (§12.1), the context is compacted once with trigger `switch` on the current model (§12.3), when a compactor is registered. When it still exceeds that room, the switch is refused with `provider.switch_rejected{reason:"capacity"}` and the turn runs on the current model.
3. Otherwise the target becomes the model. The history is not converted; each request renders it for its target by the replay matrix (`mappings/projection.md`).
4. When the target has `thinking_prefix_bound`, a new epoch opens (I6): a system line with boundary reason `switch` that keeps the system prompt and carries the tool definitions frozen for the target, then a copy of every other line of the old epoch, in order, each with `copy_of`. The new lines are persisted as one batch, and each gets its `message.completed`, published under the switching turn.
5. `provider.switched{from, to, dropped_reasoning, dropped_opaque, cache_reset:true}` follows. The counts are the reasoning and opaque blocks of the history that the target cannot receive.

The estimate's anchor and calibration ratio reset, since they describe the old model's tokenizer, and the context in use becomes the estimate of the whole request; until the next report, the context gauge's basis is `estimated`.

### 9.5 Model listings

An endpoint that answers `GET {base}/models` with `{data:[…]}` is read once, and every model is answered from it. An entry is named by `id`, else by `model_name`. Its capabilities are read from the entry itself and then from its `model_info` object, taking the first positive integer:

| Capability | Fields, in order |
|---|---|
| `context_window` | `max_input_tokens`, `context_window`, `context_length`, `max_model_len` |
| `max_output_tokens` | `max_output_tokens`, `max_tokens`, `max_completion_tokens` |

Every other field stays unknown.

## 10 Hooks

A hook is a function registered on a hook point. The JSON of each kernel point's input and output is `schema/hook.schema.json#/$defs/<point>_input` and `<point>_output`; bindings with typed hooks use the same fields.

### 10.1 Kernel points

| Point | Fires | Key | Timeout | Failure |
|---|---|---|---|---|
| `session_start` | When an epoch starts: source `new` before `session.started`, `resume` (§3.2), `clear` and `compact` (§12); context becomes the epoch head line, or a reminder with the next turn when the head is already written | source | 30 s | open |
| `session_end` | When a session that started closes, before `session.closed`; only `system_message` acts | reason | 5 s, shared by all its hooks | open |
| `user_prompt_submit` | Before a delivery point commits queued inputs, once per origin kind, in order of first appearance | origin kind | 30 s | closed: blocks every input of the firing |
| `command_invoke` | Before a command starts | command name | 10 s | closed: blocks the command |
| `pre_model_request` | Once per step, before its request; retries reuse it | model | 10 s | open |
| `post_model_response` | Once per step, after the reply and before it is committed; not for a step a `pre_model_request` hook answered, whose reply already came from a hook | stop reason | 30 s | open |
| `pre_tool_use` | At the gate, once per call, on the normalized input | tool name | 60 s | closed: denies the call |
| `approval` | For each call the gate sends to review (§11) | tool name | none | closed only: denies the call |
| `post_tool_use` | After each call, in the call's worker, and after each call the gate denied or answered | tool name | 30 s | open |
| `post_tool_batch` | Once a batch has every result, before it is committed | — | 30 s | open |
| `stop` | When the model answered without calls, before the turn ends | — | 30 s | open |
| `provider_switch` | When a pending switch is about to take effect | target model | 10 s | closed: blocks the switch |
| `turn_end` | On every way a turn ends, after its history is repaired; context rides the next turn and does not wake | outcome | 10 s, shared by all its hooks | open |
| `pre_compact` | Before a compaction, once it is decided (§12.3) | trigger | 30 s | open |
| `post_compact` | Once a compaction's result is valid, before its epoch is written; context joins the new epoch head | trigger | 30 s | open |
| `task_start` | Before an attempt is made, on the thread that starts, restarts or continues the task (§13.2) | kind | 10 s | closed: refuses the attempt |
| `task_notify` | Before a task notification is queued, off the state owner (§13.7) | kind | 10 s | open: the text as rendered |

What an output field does:

- `user_prompt_submit`: `block` drops every input of the firing, `block_ids` the named ones, each with `input.dropped{blocked}`; `updated_inputs` replaces the text, parts and metadata of inputs by id.
- `command_invoke`: `block` refuses the command (§6); `updated_args` replaces its arguments.
- `pre_model_request`: `params_patch` changes this step's request, the tool choice included; `next_turn_reasoning` replaces the reasoning parameters of the next turn only, because a turn never mixes them (I10); `synthetic` answers the step instead of the model: it is committed as the step's reply with producer API `synthetic`, `step.started{api:"synthetic"}` and `step.completed` around it, and no request is sent or booked.
- `post_model_response`: `reject` discards the reply (`message.discarded{hook_rejected}`) and ends the turn `hook_stopped`.
- `pre_tool_use`: see §11.3.
- `post_tool_use`: `updated_result` replaces the result's content and `is_error`; a success or error result follows the new `is_error`, other outcomes keep theirs. `tool.completed` carries the replaced result.
- `post_tool_use`, `post_tool_batch`: `stop_turn` ends the turn `hook_stopped` once the batch is committed; the safe point then takes no queue item.
- `stop`: `continue` keeps the turn going: a continuation line with one reminder per hook that asked, source `hook:<name>`, the hook's `reason` or `stop_continue`. At most `max_stop_continuations` per turn; beyond it the request is ignored and that hook's `hook.completed.decision` is `continue_ignored_cap`.
- `provider_switch`: `block` keeps the current model (§9.4).
- `pre_compact`: `block` refuses the compaction, except an overflow one, which must go on; `updated_instructions` replaces the per-call instructions.
- `task_start`: `deny` refuses the attempt with `task_not_started` and its `reason`, leaving nothing behind; `updated_spec` replaces the spec, and one that changes the kind counts as the hook failing.
- `task_notify`: `updated_text` replaces the notification's text; `no_wake` queues it without the right to start a turn.

### 10.2 Running a point

The hooks of a point run in Priority order (lower first), then agent scope before session scope, then registration order. A hook with a matcher runs only when the point's key matches.

1. Every matching async hook starts first, each with a copy of the input as it is before any sync hook, in the session's async pool (§10.6).
2. The sync hooks then run one at a time, each on its own worker, each seeing the input as the hooks before it rewrote it. A hook runs under the timeout of its point, or its own; `0` means no deadline. When a hook's context ends, it has `hook_grace` to return and is then abandoned. An answer that arrives after the context ended counts as none.
3. Each sync hook emits `hook.started`, then `hook.completed{decision?, system_message?}` or `hook.failed{error, timeout, canceled, policy_applied}`. The events of a per-call point (`pre_tool_use`, `approval`, `post_tool_use`) carry the `call_id`.
4. A hook that errs, panics, times out or answers JSON that does not decode has failed. At a point that fails open, or when the hook asks to, it has no opinion. At a point that fails closed, or when the hook asks to, its output is the point's closed output, given in §10.1; the reason the model may see names only the kind of failure (`timeout`, `error`, `panic`), never the error text. `approval` accepts no fail-open hook. A point without a closed output always fails open.
5. When the caller's context ends (an interrupt, a close), the chain stops. The hook running at that moment reports `hook.failed{canceled:true, policy_applied:"open"}`, and the call point handles the cancellation as it does without hooks.

A point fired from inside its own hooks more than `hooks.max_depth` deep (`data/limits.json`) runs nothing and emits `error{hook_recursion}`. A sync hook that cannot run, because of that depth or because the caller was canceled before or while the chain ran, is not consent: when one of them fails closed, the point's output is its closed output, with failure kind `recursion` or `canceled`.

### 10.3 Merging

| Output field | Merge | Stops the chain |
|---|---|---|
| `decision` (`pre_tool_use`) | The strictest: deny > ask > allow > none; its reason with it | deny |
| `synthetic_result`, `synthetic` | The first one | yes |
| `block`, `reject` | Any; the first reason | yes |
| `block_ids` | The union | no |
| `continue` (`stop`) | Any; the reasons joined with a blank line | no |
| `stop_turn` | Any | no |
| Rewrites (`updated_input`, `updated_inputs`, `updated_args`, `updated_result`) | A pipeline: each hook sees the previous rewrite, the last one counts | no |
| `params_patch` | Field by field, later hooks over earlier ones; `headers` and `extra` key by key | no |
| `next_turn_reasoning` | The last one | no |
| `additional_context`, `system_message`, `halt` | Per hook (§10.5) | no |

### 10.4 Matchers and expressions

A declarative matcher is `""` or `*` for every key; names made of `[A-Za-z0-9_.:-]` joined by `|`, compared exactly and case-sensitively; or `re:` followed by an expression that must match the whole key.

Expressions, in matchers and in approval rules, are a subset every regular-expression engine runs alike, and anything outside it is an error:

- literal characters, and `\` before any ASCII punctuation character;
- `.`, any character but `\n`;
- classes `[…]` and `[^…]` of literal characters, ranges and the escapes below; `]`, `[` and `\` in a class are escaped, and a class is never empty;
- `\d` `[0-9]`, `\w` `[0-9A-Za-z_]`, `\s` `[\t\n\f\r ]`, their negations `\D` `\W` `\S`, and outside classes the word boundary `\b` and its negation `\B`, all by these ASCII definitions;
- the quantifiers `*`, `+`, `?`, `{m}`, `{m,}` and `{m,n}` (at most 1000), each optionally followed by `?`, never two in a row;
- groups `(…)` and `(?:…)`, and alternation `|`.

`^`, `$`, inline flags, backreferences, lookaround, named groups, Unicode properties, POSIX classes, and escapes such as `\n` are errors, as are invalid UTF-8 and an unescaped `{`, `}` or `]` outside a quantifier or class. A binding whose engine defines these atoms differently rewrites them before compiling.

### 10.5 Where outputs go

- `system_message` is published as `notice{source:"hook:<name>", text}`, once per hook.
- `additional_context` of each hook becomes one reminder with source `hook:<name>`. Inside a turn it joins the next delivery point's user line (§5.6); at `session_start` it becomes the epoch head line, a user line with origin `epoch_head` right after the system line; anywhere else, and when a turn ends before it was delivered, it is queued as a reminder that does not wake.
- `halt` ends the turn after the point: after the gate or a batch, once the batch is committed; after `pre_model_request`, before the model call; after `post_model_response`, once the reply is committed. It acts only at points inside a turn.

### 10.6 Async hooks

An async hook runs beside the point and is never waited for; its only output is an injection (`#/$defs/injection`). At most `max_async_hooks` run at once per session; a call that finds the pool full is dropped with `error{async_hook_dropped}`. It runs on the session's context, not the turn's, with the timeout `hooks.async_timeout_ms` unless it sets its own, and emits `hook.started{async:true}` at the point and `hook.completed` or `hook.failed` when it ends, the latter without a turn. Those two end events are not ordered against the events of the turn that fired the point: an async hook that answers at once may end before or after that turn does. The async hooks `session_end` started are awaited, within the close timeout, before the session's context is canceled.

An injection's `system_message` is a notice. Its `additional_context` is queued as a reminder with source `hook:<name>` in `next_loop` mode. It wakes when the injection asks and the hook has woken the session fewer than `hooks.wake_limit` times within `hooks.wake_window_ms`, counted in real time; otherwise it waits as a passenger. An injection that arrives after a clear of the context it answered is dropped.

### 10.7 Context providers

A context provider adds one reminder, with its name as source, at delivery points: a `turn_start` provider at a turn's start, an `every_step` provider at a turn's start and at every safe point another model call follows. Providers of one kind run in name order, a session's shadowing the agent's of the same name. An empty answer adds nothing; a failure adds nothing and emits `error{context_provider_failed}`.

## 11 Approval

### 11.1 Normalization

Before anything reads a call's input, the gate normalizes it: it must be a JSON object that names no key twice at any depth, and insignificant whitespace is removed while key order and string escapes stay as sent. A call that fails gets `invalid_input_detail`, such as `field "a" appears more than once`, and is neither reviewed nor run. Rules, hooks, reviewers and the tool then read the same bytes; a rewrite by a hook or a reviewer is normalized the same way, and one that fails counts as that hook failing (`policy hook failed (invalid input)`, `reviewer failed (invalid input)`). A tool that decodes its input matches keys exactly and case-sensitively and rejects unknown keys, so a key that escaped a rule by its spelling never reaches the tool.

### 11.2 Rules

A rule (`schema/approval_rule.schema.json`) matches a call when its `tool` is the call's name, or a glob (`*`, `?`, whole name) matching it, and every condition of `when` holds; without `when` the name alone decides. A `when` key is a dot path into the input: a number segment indexes an array, `*` takes every element, and any other segment looks up an object key. The path's values are the values it reaches; none means the field does not exist.

| Operator | Holds when |
|---|---|
| `eq` | A value is JSON-equal: numbers by value, objects regardless of key order |
| `glob` | A string value matches the glob as a whole |
| `regex` | A string value matches the expression (§10.4) as a whole |
| `prefix` | A string value starts with it. With `normalize: "path"` the value and the prefix are first cleaned by POSIX path rules and compared by whole segments: the value matches when it equals the prefix or continues it with `/`, so `/workspace` matches `/workspace/a` but not `/workspace2`, and `/` matches every absolute path. A relative value never matches an absolute prefix, except that in a `deny` rule a value that still starts with `..` after cleaning, climbing out of the working directory, matches. |
| `in` | A value is JSON-equal to one of the list |
| `exists` | Values exist (`true`) or none does (`false`) |

A condition sets exactly one operator, and `normalize` qualifies `prefix` only. A missing field satisfies only `exists: false`; a value of the wrong type satisfies nothing. Rule ids are unique within a set; the agent's rules come before a session's. A set with an unknown operator, a condition with more or fewer than one operator, a misplaced `normalize`, an expression outside the subset, an empty or repeated id, or an unknown action is refused where it is configured.

A rule may also carry a **binding-native predicate**: a callback the host attaches when it builds the rule, taking a `tool_call_view` and returning a boolean. It is not part of a rule file, and a rule file cannot express one.

The predicate runs only after the rule's `tool` and every `when` condition match, and sees the same normalized `tool_call_view` the rules, hooks and the tool see. The rule matches if and only if both the declarative part and the predicate are true; a predicate can never make a rule match when its declarative part does not.

A predicate must be deterministic, free of side effects and I/O, safe for concurrent use, and fast enough for the gate and the review workers. When it panics or otherwise fails, the rule is resolved toward the stricter outcome: `deny` and `ask` count as matching, `allow` counts as not matching, and the binding SHOULD record the failure. A failing predicate therefore never loosens policy.

A rule that carries a predicate cannot be written to a rule file; a binding SHOULD return an error when asked to serialize one rather than silently dropping the predicate. A binding SHOULD offer a way for the host to attach a predicate; the shape of that API is the binding's, the semantics above are not.

The rules' own conclusion: any matching deny rule denies, the first in rule order naming the reason; otherwise any matching allow rule allows, which exempts from ask but never from deny; otherwise any matching ask rule asks; otherwise there is none.

Rules may be added and removed at runtime through the binding's registrar, and each change announces `registry.changed{kind:"approval_rule", effective:"now"}` with the scope it was made in. A change takes effect from the next gate, never inside a call whose set was already read: a gate reads the effective set once, and a reviewer's rewrite is judged by that same set. A set is ordered agent scope first, then session scope, and an id may appear only once across the scopes one session sees; adding a rule that repeats an id already in the set, or creating a session whose rules repeat an inherited id, is refused as a duplicate rather than resolved by shadowing. An inherited rule cannot be hidden or shadowed, and `Remove` revokes only a rule the registrar added at runtime; a session that could hide an outer `deny` would escape the agent's policy. A binding SHOULD offer a read of a session's effective rules, each with the scope it came from and whether it can be written to a rule file.

### 11.3 The gate's decision

For each normalized call:

0. In a fork limited to some tools, a call to any other tool is refused before anything else: the result is `tool_not_allowed` (outcome `denied`), and no hook, rule or review sees it.
1. `pre_tool_use` runs. A `synthetic_result` is the call's result (outcome `synthetic`) and nothing else runs; a deny is the result `denied_by_hook` (outcome `denied`).
2. The rules decide on the input as the hooks left it. A deny is the result `denied_by_rule`.
3. The call is reviewed when the hooks or the rules ask: an allow on one side never lifts an ask on the other. Otherwise it runs. With no rule and no hook deciding, it runs.

### 11.4 Review

Every call of a batch that needs review gets an approval id, and `approval.requested{approval_id, call_id, name, input, rules, batch_id, batch_size}` is published for each, in call order, before any reviewer runs, each rule carrying the scope it was registered in (`agent` or `session`). The reviews then run concurrently, and the batch waits for all of them. A review ends with exactly one `approval.resolved{approval_id, call_id, verdict, reason, by, input_updated, duration_ms}`; `by` names the reviewer that decided (the last one, when all approved), or `timeout`, `canceled` or `none`.

A review walks the `approval` hooks matching the tool, read when the review starts, in hook order (§10.2). The request (`#/$defs/approval_input`) carries the rules that matched, the reasons of the hooks that asked, the batch, and the round.

- A verdict other than `approve`, an empty or unknown one included, denies at once (`denied_by_reviewer`). A reviewer that fails, a timeout it sets for itself included (`by:"timeout"`), denies.
- `approve` with an `updated_input` that differs from the current input rewrites it: the rules decide again on the new input, and a deny there denies the call (`denied_by_rule`). When an earlier reviewer of the round had approved, the round restarts from the first reviewer on the new input, with `approved_input` set to what they approved. A second round that is rewritten again denies (`input was rewritten after approval more than once`).
- When every reviewer approves, the call runs with the final input, which `tool.started` carries.
- With no matching reviewer the call is denied with the reason `no reviewer registered` (`by:"none"`). A session whose rules can ask and that has no reviewer at all warns with `notice{source:"approval", kind:"no_reviewer", detail{rule}}` when it starts.
- `deny_and_stop` denies the call and ends the batch's other reviews that still wait: they get `approval_stopped` (outcome `not_executed`, `by:"canceled"`). The calls that are allowed or approved still run, the batch is committed, and the turn ends `hook_stopped`.
- An interrupt or a close ends every waiting review: the call gets `approval_interrupted` (outcome `interrupted`, `by:"canceled"`), and approvals given earlier in its chain no longer count.

## 12 Context management

### 12.1 Size and threshold

The runtime estimates the size of the next request. It anchors on the input plus output tokens the provider reported for the last committed main-loop step, which covered every line up to that reply, and adds the lines after it, counted by a heuristic: ASCII text at four bytes a token, every other code point as one, 1600 per image or document, a tool call's name and input bytes, and four per message. The heuristic is scaled by a ratio that each report calibrates, the mean of the old ratio and the new reported/heuristic, kept between 0.5 and 2. Without an anchor, after a new epoch or a resume, the whole request (system, tools and lines) is counted and scaled. When the estimate is within `context.count_band` of the threshold (`data/limits.json`) and a token counter is available, the runtime counts the request exactly instead.

The usable window is the context window, lowered by the policy's maximum context tokens when set, minus the effective output cap. Automatic compaction starts at `context.auto_compact_ratio` of it. The summary's token target is `context.target_ratio` of the usable window, clamped between `context.min_summary_tokens` and `context.max_summary_tokens`, and at most half the threshold. A summary request's output cap is 1.25 times the target, at most 1.25 times the maximum, and twice that for its one retry after a cut-off reply (§12.3).

### 12.2 When a session compacts

- **Automatically**, before each model call of a turn, when the estimate reaches the threshold, at most `context.max_auto_compacts_per_turn` times per turn. It never runs right before a `pause_turn` resend.
- **On overflow**, once per step, when the API rejects a request as too long (class `context_overflow`); the step is then sent again on the compacted history. Without a compactor, or when the compaction fails, the turn ends with the error.
- **Before a switch**, once, at the start of the turn the switch takes effect in, when the context does not fit the target (§9.4).
- **Manually**, by the caller. A manual compaction waits for the running turn, like a history command, and may be limited to one side of a turn of the current epoch (§12.5).

Automatic compaction stops for the session once `context.max_consecutive_failures` automatic compactions in a row have failed, with `context.compaction_failed{error:"thrash"}`. A compactor that finds nothing to compact is not a failure of that kind.

### 12.3 One compaction

1. `pre_compact` runs. A block refuses the compaction unless the trigger is `overflow`. Then `context.compaction_started{trigger, pre_tokens}`.
2. The compactor receives the current context without its system line, the epoch head's length, the summary the epoch head carries (the reminder with source `compaction`), where the running turn starts, the range, the estimate, the target and the threshold, and a summarizer. The summarizer sends one request with this session's system, tools and model: either the whole current context as the cached prefix (the rolling breakpoint on the prefix's last user-side block, `cache_tail`) followed by the instructions, or given lines followed by the instructions, with no claim on the cache. It drops any tool call from the reply. When the reply stops at the output cap, it sends the request once more with twice the cap and `summary_too_long` appended to the instructions, and returns that reply, cut off or not; if that request fails, it returns the first reply. Its requests are model calls in bucket `compaction`, attributed to the running turn when there is one.
3. The result is checked: every line it keeps is a line of the context after the epoch head, unchanged except for cleared tool results; it does not start with an assistant line; it keeps no call without its result; it summarizes, drops or clears something; and, for a full compaction, its estimated size is under 90% of the threshold. Otherwise the compaction fails with `invalid_result`.
4. `session_start{compact}` and then `post_compact` run; their context joins the new epoch head.
5. The new epoch is written in one batch (RULE-STORE-004): the system line, the epoch head (the `session_start` context, the summary as a reminder with source `compaction`, the pins, the `post_compact` context), copies of the kept lines, and, when the compaction ran inside a turn whose kept lines do not end with its input, a continue reminder (`compact_continue`). The executed-call ledger, the stop continuation count and the anchor of the estimate reset; the pin set becomes the pins of the new epoch.
6. Each new line gets `message.completed`, then `context.compacted{trigger, strategy, pre_tokens, post_tokens, epoch}`. A failed compaction ends with `context.compaction_failed{trigger, error, canceled}` instead, where `error` is `nothing_to_compact`, `compactor_failed` or `invalid_result`.

### 12.4 Pins

A pin is text that survives compaction: a tool result's `pin`, or a command result's pin, which joins the set without being written. The set holds one pin per key, the latest winning. A compaction re-injects it in the new epoch head as reminders with source `pin:<key>`, newest first until `context.pin_total_tokens` is spent, oldest first in the head; a pin longer than its budget (its `max_tokens`, or `context.pin_tokens`) is cut and followed by `pin_truncated`. Clear empties the set. Unpinning takes a key out of the set at once and queues a reminder that does not wake, with source `pin:<key>`, text `pin_removed` and `unpin: <key>` on its block; the text already in the context stays until the next epoch.

### 12.5 Partial compaction

A manual compaction may name a turn of the current epoch that starts after the epoch head:

- **Up to it**: the lines before the turn are summarized, as given lines; the turn and every later line are kept after the rebuilt epoch head. On a model whose thinking signatures bind the prefix, the kept lines follow changed content, so the boundary carries `replay_fallback: true` and every request of that epoch is sent with `replay: fallback`.
- **From it**: the turn and every later line are summarized with the whole context as the cached prefix. The system line and every line before the turn are copied unchanged, so the next request starts with the prefix the earlier requests cached; the summary follows as one line with origin `epoch_head`, holding the pins the summarized part declared and the `post_compact` context. `session_start{compact}` does not run, since the old epoch head is kept. The boundary keeps the current epoch's `replay_fallback`, as rewind and fork do: copied lines that already followed a changed prefix still do.

### 12.6 Clear

Clear waits for the running turn like compaction. It opens an epoch with a re-rendered system and no old line, empties the pin set, resets the executed-call ledger and the estimate's anchor, and drops the reminder items queued before it, and those a turn's end left after it (the interrupted note and `turn_end` context of the turn it waited for) (`input.dropped{cleared}`); asynchronous hook results started before it are dropped when they arrive. Task notifications and queued human, command and agent inputs stay. `session_start{clear}` gives the epoch head. The system line and the head are written in one batch; then `context.cleared{epoch}`.

### 12.7 Rewind

A rewind names a turn of the current epoch that starts after the epoch head, and drops it and every later line from the context. It waits for the running turn like compaction. It opens an epoch whose system line is the current one copied, with neither system nor tools re-rendered, followed by copies of the lines before the turn; so the next request starts with bytes the earlier requests already sent, and on a prefix-bound model the copied signatures still hold. The pin set is rebuilt from the kept lines as on resume (§3.2 step 3). The executed-call ledger and the estimate's anchor reset, asynchronous hook results started before it are dropped when they arrive, and the reminder items queued before it, or left by a turn's end after it, are dropped (`input.dropped{rewound}`); task notifications and queued human, command and agent inputs stay, and running tasks go on. Effects outside the context, such as files a tool wrote, are not undone. `session_start{rewind}` runs, and its context arrives as a reminder with the next turn. The new epoch is written in one batch, each line gets `message.completed`, then `context.rewound{turn_id, epoch, dropped}`.

### 12.8 Forks and side chats

A fork copies a session's current context into a new, independent session with its own id and file. By default it copies the source's stable prefix, the current epoch without a reply being generated or a call without its result; at a turn it copies the lines before that turn, which must be a rewind target. The new file's first epoch is the source epoch's system line copied, tools included, then copies of the lines (RULE-STORE-004), so the fork's first request hits the source's cache and copied signatures still hold. The new session keeps the source's agent name, labels, session instructions, approval rules and the session-scope registrations the source has at that moment; later registrations on either side apply to that side only. `session_start{fork}` runs, its context arriving with the first turn, and `session.started{source:"fork"}` follows.

A fork may be limited to some tools. The tool definitions sent stay the source's; the gate refuses any other tool (§11.3 step 0), and the first turn carries a reminder that does not wake, with source `tools` and the `tools_allowed` text naming the allowed tools in name order (`none` when there are none). The fork's system line records the allowed names in name order as boundary `tools_allowed`, an empty list when none is allowed and nothing when every tool is; resuming a session keeps the list of its latest fork line, unless a side chat is reopened with a list of its own, which holds for that opening only.

A side chat is a session held by name under a main session that carries the main session's current context in every request instead of a copy: its id is `<main id>.side.<name>`, with a name matching `[a-z0-9][a-z0-9_-]{0,63}`, and its file's first line records the main session as `parent_session_id` and the name as `side_chat`. Opening a name that is open returns that session. Otherwise a stored side chat whose current epoch is not a dropped mark and whose latest opening line has reason `side_chat` is resumed (§3.2); a stored one whose latest opening line has reason `fork`, from a version that copied the main session's lines, is marked dropped first, and any other name opens anew, into a new epoch of the same file after a drop. Opening writes an epoch that copies no line: the main epoch's system line copied, tools included, with boundary `{reason:"side_chat", from:{session_id, seq}}`, `seq` being the main session's last line, then an epoch head that starts with a reminder of source `side_chat` and the `side_chat_head` text. Every epoch head a side chat writes later, at a compaction or a clear, starts with the same reminder. The side chat keeps the main session's agent name, labels, session instructions, approval rules, model state and session-scope registrations as a fork does, and its allow-list rules are a fork's; a side chat cannot open side chats of its own, and opening one at a turn is refused.

When a side chat's turn starts, before its input is committed, it takes the main session's current context once: the committed lines of the main session's current epoch after its system line. When the main session's last line is an assistant line whose calls have no results yet, a tool line follows it with one result per call, outcome `not_executed`, not an error, and the `main_call_pending` text; that line is never stored. Every request of the turn, retries and the turn's side calls included, sends these lines first and the side chat's own current epoch after its system line next, with the side chat's own system and tools; a side call outside any turn takes the main session's context when it runs. When the side chat has a reply in its current epoch and the main session's last line differs from the one the turn of its latest reply carried (an unknown one counts as different), the turn's input message starts with a reminder that does not wake, of source `side_chat` and the `side_chat_main_changed` text. Its `first` names the first line of the turn's main-session lines whose seq is greater than the one that latest reply carried: a user-side line is `the message "<s>"`, `<s>` taken from its text blocks joined by a space, or from its reminder blocks the same way when that is empty; an assistant line is `the reply "<s>"` when it has text, otherwise `the call to <names>`, or `the calls to <names>` for more than one tool; a tool line is `the result of <names>`, or `the results of <names>` for more than one tool; `<names>` are the distinct tool names in content order joined by `, `, and `<s>` is the text with each run of white space made one space and trimmed, cut to its first 48 code points, trailing white space removed and `…` appended when longer. `first` is empty when the carried seq is unknown, when no line of the turn's main-session lines has a seq up to it, or when the first newer line has neither text nor calls. The request's `shared_prefix` is the number of the main session's own lines it carries, the placeholder left out, and its cache key is the main session's id. On a model whose signatures are bound to the prefix, the reasoning blocks of the side chat's assistant lines from earlier turns are left out of the request; the main session's lines keep theirs only when the side chat's model, system and tools equal the main session's, and the request renders with the fallback replay when the main session's current epoch does. Each model call of a side chat records the seq of the main session's last line it carried as `parent_seq`.

The context of a side chat is the whole request: its estimate adds the main session's estimate of the lines it carries, and a report calibrates the side chat's own part with the report less that estimate. Compaction, clear and rewind apply to the side chat's own lines only. The compactor is given the side chat's own estimate as the tokens and the threshold less the main session's estimate, not below zero, as the threshold. A summary request with the whole context sends the main session's lines and the side chat's lines as its prefix, and the `side_chat_compact_scope` reminder of source `side` before the instructions; one with the compactor's own lines has neither. A side chat does not compact on its own while its own lines are within the summary target.

Dropping a side chat closes it and appends a single line: a system line with boundary `{reason:"dropped"}`, no content, the next seq and epoch. Every model call of a side chat is also booked in its main session, in bucket `side` with no turn, published as `usage.updated{forwarded:true}` before the side chat publishes its own `usage.updated`. Closing a main session closes its side chats first; a side chat whose main session is gone without closing it fails the turn that cannot take the main session's context and closes with reason `main_closed`. A main session's rewind and clear drop every side chat opened before the epoch they open; a compaction and a model switch drop none, and the side chat's next turn carries the new context. Once the new epoch is written, and before the operation returns or its turn goes on, each such open side chat is closed, marked dropped and announced on the main session as `side_chat.dropped{name, reason}`, `reason` being `rewound` or `cleared`, in name order. A stored side chat opened before such an epoch that was not open, or whose mark was not written, is not listed; opening its name marks it dropped and opens anew.

A fork of a side chat copies, after the side chat's system line, the main session's stable prefix of its current epoch and then the side chat's stable prefix, or its lines before a turn; on a model whose signatures are bound to the prefix, its boundary carries `replay_fallback`.

### 12.9 History search

A session's history is its committed lines, stored or not yet stored, that the current context holds or that a compaction took out of it. What the user discarded is left out: the lines a rewind took out, everything before a clear, and in a side chat everything before its latest opening. A line counts as held when a copy of it is, and a compaction takes out the lines of its epoch that the new epoch does not copy. A search takes words, all of which must appear case-insensitively, and returns the newest lines first, at most 50 (10 by default). It skips a copy of a line of the same file, so a line is found once, at its original, while a fork's copies, whose originals are in the source's file, are searched as they are; system lines; and an assistant line whose calls are still running, so a tool searching the history never finds its own call. A line's searchable text is its text, refusal and reminder blocks, each tool call as its name, a space and its input JSON, and the text of tool results; reasoning is left out. A hit carries the line's seq, epoch, role and turn, whether the line or a copy of it is in the current context, and up to 200 code points on each side of the earliest matched word. A line of the history can also be read by seq, with whether the current context holds it. A search may be limited to the lines the current context holds, which is how a child session searches its parent (`ext/history`).

### 12.10 Context breakdown

The context breakdown reports the model, the epoch, the window, the usable window, the threshold, the estimate of the next request (§12.1) and its basis: `reported` or `counted` when no line was added since that report or count, `estimated` otherwise. Its parts are the system by section, each tool definition the next turn sends, the summary, each pin of the epoch head, the rest of the epoch head, the lines by kind (user, assistant, reasoning, tool call, tool result, reminder), and in a side chat the main session's lines as one part of kind `parent`; the `parent` part is the main session's estimate of those lines, and the others are counted by the heuristic and scaled so that all parts add up to the estimate.

## 13 Tasks

A task is background work a session owns: it runs off the turn, reports when it ends or needs input, and can be told more, stopped, restarted or continued. What it runs is up to its kind; how it behaves is the same for every kind.

### 13.1 Kinds and the manager

A kind has a name (the rules of a tool name), capabilities (`instructions`, `continue`, `restart`, `inspect`) and a constructor, called once for every attempt. A session's kind shadows the agent's of the same name; `tool_exec` is reserved for the runtime. Every session has one task manager, which the host, tools, hooks and tasks all use. A task has an id, a spec (`task.schema.json#/$defs/task_spec`), a status and a current attempt, numbered from 1; `succeeded`, `failed`, `canceled` and `lost` are terminal. At most `max_running_tasks` tasks of a session are unfinished, foreground and background together; a start, restart or continuation beyond that fails with a limit error. The 50 most recent finished tasks are kept; older ones are gone, and asking for them fails as for an unknown task.

Every value a task or its caller hands over as JSON (the input, instruction data, progress data, the result's data, the saved state) must be valid JSON and is stored with its whitespace removed; an invalid state is not kept and emits `error{internal}`.

### 13.2 Starting

A start takes a slot, runs `task_start` on the caller's thread, constructs the attempt, then registers the task and starts it at once: the task list's snapshot, `tasks.snapshot`, `task.started` (its detail holds the spec and, for a continuation, the state it resumes from), then the kind's run. A refusal or a failure before registering leaves nothing and gives the slot back. An attempt that was constructed runs exactly once, with a context that is already canceled when its session closed meanwhile. A start made with a tool call's context is recorded against that call and its turn.

### 13.3 Ending

The runtime's own record decides an attempt's end: asked to stop before its run returned, it is `canceled`, whatever the run returned; otherwise an error or a panic makes it `failed` and anything else `succeeded`. A stop after the run returned changes nothing. An attempt that has not returned `tool_cancel_grace_ms` after a stop is `canceled` and abandoned: `error{worker_leaked}`, its info says `abandoned: true`, and its late return is dropped. The end publishes the snapshot, `tasks.snapshot` and `task.finished`, then wakes waiters. Every attempt has exactly one `task.started` and one `task.finished`, except a lost one found by a resume, which has only `task.finished{lost}`. A result without a summary takes the text of its content. The attempt's unprocessed instructions are those it last reported holding (taken from the inbox and not yet acted on), then those left unread in the inbox.

An attempt may relay an event from elsewhere, such as another agent's session: it is published on the owner's bus with the task's hop, carrying the event's own seq, put in front of its path, and a bulk one waits for credit first. Inspecting a task whose kind can inspect and whose attempt is running asks the attempt for its live state on the caller's thread; the answer becomes its latest progress and emits `task.progress`, and a failure returns the error with the task's info. Inspecting any other task is the same as reading it.

### 13.4 Instructions, questions and continuation

An unfinished task whose kind takes instructions has an inbox of 16; a send beyond that fails as a full queue. Each send emits `task.instruction{accepted, reason}`. An attempt may ask a question: it becomes `waiting_input` with that question, and the first instruction accepted after it carries `answer: true` and moves it back to `running`; an instruction accepted before the question never does. Asking again replaces the question; an empty question withdraws it, together with its undelivered notification (`input.dropped{withdrawn}`). A question from a kind without instructions is ignored with `error{internal}`, since nothing could answer it.

A send to a finished task whose kind can continue starts the next attempt in the background: the same spec with the last saved state and that instruction, `task_start` with `continuation: true`, then `task.instruction` and `task.started{continuation: true}`. A kind that cannot continue refuses it.

### 13.5 Restart

A restart, allowed by the kind's capability in any status, first takes the task's migration lock; a second restart or continuation while it is held fails at once as busy, to be retried. When it has a new input, or the current input was not approved in this process, the input passes the gate (§11) as a background call of the kind's start tool: the name is that tool's, or `task:<kind>` when the kind has none, or for `tool_exec` the wrapped tool's with that tool's own input (§13.9), and the input gets `"background": true` as its last member. Rules and reviewers apply, `pre_tool_use` does not, and a refusal returns the gate's text and leaves the current attempt alone. An input is approved when the model's start tool call passed the gate or a gated restart approved it; this is not stored, so after a resume every restart is gated. Then `task_start` runs and the attempt is constructed. The switch makes the new attempt current and `pending`; a replaced attempt that still runs is asked to stop without a notification, its unread instructions become its unprocessed ones and its undelivered notifications are withdrawn. The new attempt starts once the replaced one has returned or been abandoned. Instructions sent while the lock is held wait: after a switch they reach the new attempt, never as an answer; after a failure they reach the current one, the first of them answering a pending question.

### 13.6 Waiting and the start tool

Waiting on a task returns once it is terminal or waits for input, following it across attempts; the question of an attempt asked to stop does not count. It is refused as re-entrant for a task waiting on itself or on a task that waits on it, and for a lossless handler waiting on a task of its own session. Waiting until quiet returns once the session is idle, runs no command, has no unfinished task and no notification still in `task_notify`.

A kind's start tool gets a `background` property as the last of its input schema's properties, `{"type":"boolean","default":false,"description":"Run in the background: return a task_id now and get a notification when it finishes."}`, and in `required` when the tool is strict; a schema that already has one is an error. The runtime removes it from the input before the kind sees it. With `background: true` the call starts the task and returns `task_started`. Otherwise the call waits for the task: its end is the call's result, an error unless it succeeded, and nothing is notified. When the task asks a question the call returns the question's notification text and the task goes on in the background. When the call's own timeout passes, or the host moves the call to the background (§13.9), it returns `task_started` with `moved` and the task goes on. When the call is interrupted it stops the task without a notification and returns interrupted, unless the task's end was already fixed, whose result it then returns. A call that started, restarted, continued or stopped a task with its context references the task in its result line, with its status and state at commit time.

### 13.7 Notifications

A task notifies when it succeeds, fails, waits for input or is lost, and when it is canceled by anyone but the model's own tool call, such as the host, a hook or a session policy. It does not notify while a foreground call waits on it, nor once the session is closing. The text is `prompts/task_notification.md`; a result's summary in it is sanitized (runtime tags escaped, `Human:` and `Assistant:` at a line start quoted) and cut to 8,000 code points with `foreign_truncated`. The hints to `task_send` and `task_restart` appear only when those tools were in the tool set last sent to the model. `task_notify` may replace the text or take away its right to wake. The notification is queued with origin `task` and the task's id as source, in the spec's `notify_mode` (`next_loop` by default), waking unless a hook said not to or it is a resume's lost notice. A task has at most one undelivered notification: a newer one replaces it (`input.dropped{superseded}`), in the order the task changed rather than the order its hooks finished. Interrupting with a drop, clear and rewind keep them.

### 13.8 Persistence and resume

A task is stored only through the `tasks` references of the lines that concern it: the result lines of calls that started, continued, restarted or stopped it, and the lines that delivered its notifications. A resume scans every original line of the file, skipping copies, and takes each task's last reference. An unfinished one is `lost`: `task.finished{lost}`, and a notification that does not wake, which arrives with the next input. Its spec and last stored state are kept, so it can be restarted or continued; its start time is that of the first line of its last attempt.



### 13.9 Background tools and tool_exec

`tool_exec` is the runtime's own kind: it runs one registered tool as a task. Its input is `{"tool": <name>, "input": <the tool's own input>}`, the input a JSON object without `background`; it can restart, and takes no instructions and no continuation; a start that does not have this shape fails in its constructor. A diverted call (§4.4) starts it titled with the tool's name, its input approved; a host may start one for any registered tool, unapproved.

Each attempt resolves the tool by name when it runs: a name that is no longer registered, or that a fork's allow-list leaves out, fails it. An unapproved attempt first passes the gate as the model's own call would: under the tool's name, with `"background": true` as the input's last member, through normalization, `pre_tool_use`, the rules and the reviewers, whose requests and events carry the task's hop; a refusal fails the attempt with the refusal's text. Passing the gate with the input unchanged approves it. The tool then runs with a new call id with the prefix `tcall_`, its `tool.started`, `tool.completed` and hook events under the task's hop, its progress reported as the task's; only a timeout the tool declares itself applies, and a stop, restart or close cancels it. `post_tool_use` runs once on the result: a rewrite counts, a request to stop or halt the turn is ignored. An error result fails the attempt with the result's text as its error and no summary; any other result succeeds with the result's content, and its structured output becomes the result's data.

A restart's input is the tool's own input: the runtime removes `background` and wraps it with the same tool, which a restart cannot change; the gate sees it under the tool's name (§13.5). A `task_start` hook that changes a `tool_exec` task's input fails closed, since the gate did not see that input.

The host may move a running foreground call of a session to the background. Two kinds of call are movable, and their `tool.started` carries `movable: true`: a call of a tool declared `background` that runs as an ordinary call, and a call of a kind's start tool that waits for its task (§13.6). Other calls, calls in the task hops of the session, and calls already settled, interrupted or past their timeout are not movable, and moving one fails without effect. A task's child session marks none of its calls movable, since the task, not the host, holds that session.

- **A call of a background tool** is adopted, not run again. While it runs, the runtime forwards the turn's cancellation and the call's timeout to it, so the tool sees the same cancellation as any call's. A move first claims the call: from then on a result the tool returns, an interrupt and the timeout wait for the move's outcome. The move then starts a `tool_exec` task with the call's context, titled with the tool's name, its input the call's final input without `background`, approved, passing `task_start` like any start. When the start fails, the claim is released, the call runs on in the foreground, and an interrupt or timeout that arrived meanwhile takes effect. When it succeeds, the runtime stops forwarding the turn's cancellation and the default tool timeout; a timeout the tool declares itself still counts from the call's start. The call settles at once with `task_started` with `moved`, emits `tool.completed`, gives its place in the group back and runs no `post_tool_use`, as a diverted call does (§4.4); its progress becomes the task's from then on. The task's first attempt waits for the running call to return instead of calling the tool, emits no `tool.*` of its own, runs `post_tool_use` once on the result and ends as any `tool_exec` attempt does; a stop, restart or close cancels the call. A restart runs the tool anew.
- **A start tool's call** detaches from its task as on its timeout: in one step its waiter is removed, the task becomes background when no other waiter remains, and the call returns `task_started` with `moved`. A task whose end was already fixed is not moved, and the call returns its result.

The call's result and the move are decided once: a call that settles before the move claims it is not moved, and a moved call's result is the task's.

### 13.10 Derived agents and child sessions

A derived agent keeps its parent agent's registrations as a layer under its own, read when used, so what the parent registers or removes later applies to it too. Its name, description, user section, model and limits are the parent's unless given; given limits replace only their non-zero fields. It may leave out some of the parent's tools by a filter on their definitions, asked again whenever a layer changes, and it keeps the parent's other hooks only when asked; approval rules and reviewers it always keeps.

A child session is a session owned by a running attempt of a task. Its first layer is the owning session's effective registrations (the owner's agent and session scopes, after the owner's hides) with its agent's filter and hook choice applied; its agent's own registrations and its own session's come over them, a name in a nearer layer shadowing the same name further out. Its approval rules are the owner's followed by its own. Its reviewers run layer by layer from the root: the root's, then each owning session's in turn, then its own, each layer in hook order (§10.2), and a registration that two layers share runs once.

- Every event of a child session is also published on the owner's bus with the owning attempt's task hop put before its path, that hop's `seq` being the event's seq in the child session, its envelope otherwise the child's; a bulk event waits for the owner's backpressure credit first. The hook environment's `path` and a tool call's path are the hops from the root session.
- Every model call the child books is booked on the owner too, in the bucket named after the task's kind, under the turn of the call that started the current attempt, keeping the child's session id, and so on up to the root; `usage.updated{forwarded:true}`.
- While the owning task has no running attempt, the child starts no turn of its own: task notifications and reminders wait in its queue, while human, command and agent input still start turns. When an attempt's run returns, the runtime stops the child's unfinished tasks (which notify the child) and waits, within `close_timeout_ms`, until the owner's bus holds every event the child published so far; only then does the attempt end (§13.3). A continuation binds the children that are still open to the new attempt: their hop and calls follow it, and what they held back may start.
- A task's children are closed when it restarts or is evicted, and when its session closes, after the session's tasks were told to stop.
- A child's file records its owning session as `parent_session_id` on its first line, and resuming it is refused unless it is made a child again (§3.2).
- A reviewer's `deny_and_stop` in a child session fails the owning attempt with the `denied_by_reviewer` text once the child's turn has ended, unless the attempt was asked to stop first; what its run returns is dropped.
- Waiting on the child or any session above it from the child's callbacks is re-entrant (§13.6), and the root session id of its requests is the root's.