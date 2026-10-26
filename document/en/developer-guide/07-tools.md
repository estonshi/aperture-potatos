[← Previous: Subscribing to events](06-events.md) · [Contents](README.md) · [Next: Approval →](08-approval.md)

# 7. Registering tools

Tools are how the model calls your code. This chapter shows how to define a tool's input and output, how to set its concurrency, timeout and background behavior, and how to write tools that are easy for the model to use and that your users can trust. After reading it, you can register your own tools with an agent and control how they run.

In this chapter: [7.1 Defining a tool](#71-defining-a-tool) · [7.2 ToolSpec](#72-toolspec) · [7.3 ToolContext](#73-toolcontext) · [7.4 ToolResult](#74-toolresult) · [7.5 Execution order and concurrency](#75-execution-order-and-concurrency) · [7.6 Moving a call to the background](#76-moving-a-call-to-the-background) · [7.7 Dynamic definitions and shadowing](#77-dynamic-definitions-and-shadowing) · [7.8 Deferred loading and tool search](#78-deferred-loading-and-tool-search) · [7.9 Best practices](#79-best-practices)

## 7.1 Defining a tool

The simplest way to define a tool is to call `kernel.NewTool` with a tool definition, `ToolSpec`, and a handler function. The handler's input parameter is a Go struct with json tags, and the SDK decodes the input the model provides into that struct. The following example defines a tool that books hotels and registers it at both the agent level and the session level:

```go
type bookIn struct {
	Hotel  string `json:"hotel"`
	Nights int    `json:"nights"`
}

book := kernel.NewTool(kernel.ToolSpec{
	Name:        "book_hotel",
	Description: "Book a hotel room.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"hotel":{"type":"string"},"nights":{"type":"integer"}},"required":["hotel","nights"],"additionalProperties":false}`),
	Concurrency: kernel.Serial,
}, func(ctx context.Context, tc *kernel.ToolContext, in bookIn) (kernel.ToolResult, error) {
	return kernel.TextResult(fmt.Sprintf("booked %s for %d nights", in.Hotel, in.Nights)), nil
})

kernel.WithTool(book)                       // register at the agent level
sess.Registrar().Tool(book)                 // register at the session level, which also works while the session runs
```

`NewTool` decodes input strictly. Key names must match the json tags exactly, and unknown keys and duplicate keys are both rejected. This guarantees that the input the tool runs with is exactly the input that the approval rules and approvers saw. If decoding fails, the SDK does not call your function, and the model receives an "invalid input" result directly, with `Outcome` set to `invalid_input`.

If you need more control, you can implement the `kernel.Tool` interface yourself:

```go
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, tc *ToolContext, input json.RawMessage) (ToolResult, error)
}
```

The SDK calls `Spec()` once at the start of every turn, when it freezes the tool definitions. You can therefore compute content such as enum values dynamically in `Spec()`; [7.7](#77-dynamic-definitions-and-shadowing) shows how.

## 7.2 ToolSpec

`ToolSpec` describes a tool's name, description, input format and execution behavior. Its fields are:

| Field | Description |
|---|---|
| `Name` | The tool name, which must be unique within a session. The model sees this name, so lowercase letters with underscores are recommended. |
| `Description` | The description shown to the model. It should make clear when the tool should be used and when it should not. |
| `InputSchema` | The JSON Schema of the input. We recommend setting `"additionalProperties": false` and listing every field in `required`. |
| `Strict` | Requests a strict schema when the API supports it. |
| `Concurrency` | With `kernel.Parallel`, the call can run in parallel with adjacent Parallel calls, which suits read-only tools. With `kernel.Serial`, the call runs on its own and keeps its place in the order, which suits tools with side effects. |
| `Timeout` | With 0, `Limits.ToolTimeout` applies, which defaults to 10 minutes. With `kernel.NoTimeout` (any negative value), the tool never times out, which suits tools that wait for a person to reply or for another agent. Even without a timeout, an interrupt, a stop or a close still cancels the tool. |
| `Tags` | Custom tags. The kernel does not interpret their content. |
| `Background` | Lets the model pass `background:true` to turn this call into a background task. When you set it, the parameter is added to the tool definition automatically, but the tool itself never sees it. See [7.6](#76-moving-a-call-to-the-background). |
| `Deferred` | Loads this tool on demand when the request includes tool search. See [7.8](#78-deferred-loading-and-tool-search). |

## 7.3 ToolContext

The handler's `tc *kernel.ToolContext` parameter provides context about the current call, along with methods for reporting progress:

| Method | Description |
|---|---|
| `CallID()` | Returns the id of this call, which matches `call_id` in events and history. |
| `Session()` | Returns the session the call runs in. For the session methods you can call from a tool, see [19](19-callback-rules.md). |
| `Path()` | Returns a non-empty value when the call comes from a subagent or a background task, and an empty value for a direct call from the root session. |
| `InputMetadata()` | Returns the `Metadata` of the input that triggered this turn, which you can use for tracing. |
| `Progress(text, data)` | Emits a `tool.progress` event so the UI can show progress. It works only while `Call` is running. |
| `Emit(payload)` | Emits a custom event that belongs to this call. |

## 7.4 ToolResult

`ToolResult` is the result a tool returns to the SDK. Part of it is sent to the model, and the rest is only for the host. Its fields are:

| Field | Description |
|---|---|
| `Content` | The content sent to the model. It can contain only text, image and document blocks. |
| `IsError` | Marks the result as an error, so the model sees it as an error result. |
| `Structured` | A machine-readable result. It goes into events (`tool.completed.Structured`) and hooks, **but is never sent to the model**. |
| `Pin` | Content to re-inject after compaction, with three fields: `Key`, `Content` and `MaxTokens`. When `MaxTokens` is 0, the default of 5k applies. New content for the same key replaces the old content. |

The SDK provides two helper functions for building results: `kernel.TextResult(s)` and `kernel.ErrorResult(format, args...)`.

What the model finally sees depends on how the tool ends. The following table lists every case:

| What the tool does | What the model sees | `Outcome` of the result |
|---|---|---|
| Returns a `ToolResult` | `Content`, marked as an error or not according to `IsError`. | `success` or `error` |
| Returns a Go `error` | The error text, marked as an error. | `error` |
| Panics | `panic: <value>` without a stack trace, marked as an error. | `error` |
| Is interrupted, including when it is abandoned after the grace period or had not started yet | A fixed "interrupted" text instead of the tool's own error. | `interrupted` |
| Times out | A fixed "timed out" text. | `error` |
| Fails to decode its input | A fixed "invalid input" text. | `invalid_input` |
| Is denied by a rule, an approver or a hook | The reason for the denial. | `denied` |
| Has its result supplied directly by a hook | The content the hook supplied. | `synthetic` |
| Had no recorded result when the process restarted | A fixed "result lost" text. The tool is not run again. | `lost` |

Because the text of a returned `error` goes straight to the model, keep secrets, internal addresses and similar details out of error messages.

## 7.5 Execution order and concurrency

The model may make several tool calls in one reply. The SDK schedules them by these rules:

- Tools run only after the model's reply has been written to history. When a model call fails, only the model call is retried, so the same tool call is never executed twice.
- Within a batch of calls, consecutive `Parallel` calls run in parallel, in groups of at most `Limits.MaxParallelTools` (8 by default). `Serial` calls run on their own. The groups run one after another in the order the model gave.
- The tools in a parallel group and their `post_tool_use` hooks are called concurrently, so they must be safe for concurrent use.
- On an interrupt, the ctx of every running tool is canceled. If a tool still has not returned after the grace period `Limits.ToolCancelGrace` (3s by default), it is recorded as abandoned, a `tool.abandoned` event is emitted, and the model receives an "interrupted" result.
- Pass the `ctx` a tool receives all the way down, because it carries the timeout, the interrupt signal, and the reentrancy detection described in [19](19-callback-rules.md).

## 7.6 Moving a call to the background

Some tools run for a long time, and you may not want them to block the current conversation. For these, declare `Background: true` in the tool definition. When the model calls such a tool, it can add `background: true`, and the call then runs in the background as a task of kind `tool_exec` (`kernel.ToolExecKind`). The tool call returns a task id immediately, and the model is notified when the tool finishes; see [12](12-tasks.md). The tool's own code needs no changes at all.

The host can also move a tool to the background itself while the tool is running:

```go
info, err := sess.BackgroundCall(ctx, callID) // callID comes from a tool.started event whose Movable is true
```

If there is no call that can be moved, for example because the tool is not a `Background` tool, the call has already finished, or it is not running in the foreground, `BackgroundCall` returns `ErrNotMovable`. A common use is to offer the user a "Move to background" button when the UI notices that a tool has been running for a long time.

## 7.7 Dynamic definitions and shadowing

A tool's definition and visibility are not fixed. The SDK supports the following three kinds of adjustment.

**Tools whose definition changes.** If a tool's enum values or similar content come from a data source that changes, such as a database, implement the `Tool` interface yourself and compute the definition in `Spec()`. When the data changes, call `kernel.ToolChanged(r, name)` to tell the session to use the new definition from the next turn on.

**Adding and removing tools at runtime.** `sess.Registrar().Tool(t)` returns a `*Registration`, and calling its `Remove()` revokes the registration. Such changes take effect from the next turn, and the `Effective` field of the `registry.changed` event is then `turn`. For models that declare `ThinkingPrefixBound`, the change takes effect from the next epoch, and `Effective` is `epoch`. Either way, the tool definitions frozen at the start of a turn never change within that turn.

**Hiding a tool in one session only.** After you call `sess.Registrar().Hide(kernel.KindTool, "book_hotel")`, that agent-level tool is no longer visible in the session.

**Reading the tools that are visible now.** `sess.Tools()` returns the function tool definitions visible in the session, sorted by name. A session-level registration shadows an agent-level tool of the same name, and a tool hidden with `Hide` is left out; a tool whose `Spec()` panics is skipped and logged. The result is a copy, so changing it does not affect the registry, which makes it suitable for telling a host which tools exist during a handshake. Provider-run tools such as `web_search` are not included; read them with `sess.HostedTools()`.

## 7.8 Deferred loading and tool search

When you have many tools, putting every tool definition into the context uses a lot of tokens. Instead, you can keep some tools out of the context by default and load them only when the model finds them through search. Enabling this takes two steps:

1. Register the provider-neutral tool search: `kernel.WithHostedTool(message.HostedTool{Name: "tool_search", ToolSearch: &message.ToolSearch{}})`.
2. Mark rarely used tools with `Deferred: true`.

Anthropic Messages and OpenAI Responses currently support this feature; Chat Completions does not. On Chat Completions, deferred tools are sent as ordinary tools, and a `model.notice{hosted_tool_unsupported}` event is emitted. If a model itself does not support search, declare that with `Capabilities.NoToolSearch`. The `Native` search mode of ext/mcp is built on this mechanism; see [14.8](14-builtin-extensions.md#148-extmcp).

## 7.9 Best practices

- Write tool descriptions in English with neutral wording. A description is written for the model: usually one sentence says what the tool does, another says when to use it and when not to, and, if needed, a third says what it returns.
- Make the input schema as tight as you can. Set `additionalProperties: false`, write a `description` for every field, and use enums wherever possible. This noticeably lowers the chance that the model fills in wrong arguments.
- Mark read-only tools `Parallel` and tools with side effects `Serial`. For example, when the model makes three calls at once, "check the weather, check the exchange rate, book a hotel", the first two run in parallel, and the third waits for them to finish and then runs on its own.
- Do not let tool results grow too large. A tool result stays in the context until compaction happens, so trim or paginate output of tens of KB inside the tool, and put most of the data into `Structured` for the UI.
- Use `Pin` to save conclusions that must be kept. For example, put "the current working directory" or "the option the user chose" into a Pin, and the model can still see them after compaction.
- Long-running tools should report progress. You can send progress with `Progress`, or mark the tool `Background` and let the model decide whether to wait.
- Tools should be idempotent. The model occasionally calls the same tool twice, so a tool with external side effects (such as placing an order or sending an email) should require an idempotency key in its input, or be marked `ask` in the approval rules.
- Error messages are written for the model. Say what is wrong and how to fix it, for example "nights must be between 1 and 30", and the model will correct its input and retry. Do not return only a stack trace.
- Before returning an image result, check that the model supports images, that is, that `NoImagesInToolResult` is false. When the model does not support them, images are removed.

---

[← Previous: Subscribing to events](06-events.md) · [Contents](README.md) · [Next: Approval →](08-approval.md)
