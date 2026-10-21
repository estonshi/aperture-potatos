[← Previous: Approval](08-approval.md) · [Contents](README.md) · [Next: Commands →](10-commands.md)

# 9. Hooks

Hooks let you insert your own logic at fixed points while an agent runs, for example to rewrite input, block a tool call, add content to the context or write an audit log. Use a hook when you need to customize the behavior of the loop without modifying the SDK. This chapter covers how to register hooks, how synchronous and asynchronous hooks differ, when each hook point fires and what its inputs and outputs are, and some common uses.

In this chapter: [9.1 Registering hooks](#91-registering-hooks) · [9.2 Synchronous and asynchronous hooks](#92-synchronous-and-asynchronous-hooks) · [9.3 Hook points](#93-hook-points) · [9.4 Inputs and outputs of each point](#94-inputs-and-outputs-of-each-point) · [9.5 Events](#95-events) · [9.6 Common uses](#96-common-uses) · [9.7 Hook points defined by extensions](#97-hook-points-defined-by-extensions) · [9.8 Best practices](#98-best-practices)

## 9.1 Registering hooks

To register a hook, you specify the hook point, a name for the hook and a handler function, and you can add options. The following example registers one synchronous hook and one asynchronous hook:

```go
// A synchronous hook can rewrite or block an operation
kernel.WithHook(kernel.PreToolUse, "main-agent-books",
	func(ctx context.Context, in kernel.PreToolUseInput) (kernel.PreToolUseOutput, error) {
		if len(in.Path) > 0 { // the call comes from a subagent or a background task
			return kernel.PreToolUseOutput{Decision: kernel.Deny, Reason: "only the main agent may book"}, nil
		}
		return kernel.PreToolUseOutput{}, nil
	}, kernel.Match(kernel.MatchNames("book_hotel")))

// An asynchronous hook does not block the loop and can only add content to later context
kernel.WithAsyncHook(kernel.PostToolUse, "audit",
	func(ctx context.Context, in kernel.PostToolUseInput) (kernel.Injection, error) {
		log.Printf("audit: %s %s", in.Call.Name, in.Result.Outcome)
		return kernel.Injection{}, nil
	})
```

The SDK offers four ways to register a hook:

| Form | Scope | Description |
|---|---|---|
| `kernel.WithHook(point, name, fn, opts...)` | agent | Registers a synchronous hook. |
| `kernel.WithAsyncHook(point, name, fn, opts...)` | agent | Registers an asynchronous hook. |
| `kernel.On(r, point, name, fn, opts...)`, `kernel.OnAsync(...)` | Determined by `r` | Used in an extension's `Install` or on `sess.Registrar()`. The returned `*Registration` can be used to revoke the registration. |
| `kernel.OnJSON(r, point, name, fn, opts...)`, `kernel.OnJSONAsync(...)` | Determined by `r` | Receives the input and returns the output as JSON, in the format given in `spec/schema/hook.schema.json`. This suits forwarding hooks to a process written in another language. |

You can pass the following options when registering:

| Option | Effect |
|---|---|
| `Match(m)` | Fires the hook only when the key matches. The key is usually a tool name or a command name; [9.3](#93-hook-points) lists the key of each hook point. The available matchers are exact matching with `MatchNames("a", "b")`, whole-string regular expression matching with `MatchRegexp(expr)`, arbitrary logic written in Go with `MatchFunc(fn)`, and the declarative `ParseMatcher(s)`. In `ParseMatcher`, `""` or `"*"` matches everything, `a\|b` matches exactly, and a leading `re:` marks a regular expression. |
| `Priority(n)` | Sets the order in which multiple hooks on the same hook point run. Lower values run first. The default is 0. |
| `Timeout(d)` | Replaces the default timeout of the hook point. 0 means no timeout. |
| `Fail(p)` | Decides what happens when the hook returns an error, times out or panics. `FailDefault` uses the hook point's default behavior, `FailOpen` treats the hook as having no opinion, and `FailClosed` treats it as a block or a denial. |
| `WakeLimit(n, per)` | Limits how often an asynchronous hook can wake an idle session. Injections over the limit wait until the next turn to be delivered. |

## 9.2 Synchronous and asynchronous hooks

Hooks come in two kinds, synchronous and asynchronous, and they can do different things.

A **synchronous hook** runs on the execution path that triggered it, and the agent waits for it to return. The output types of synchronous hooks have fields for rewriting and blocking, and all of them except the one for the approval point embed `kernel.Common`. `Common` has the following fields:

| `Common` field | Effect |
|---|---|
| `AdditionalContext` | Added to the context as a reminder, with its source marked as `hook:<name>`. |
| `SystemMessage` | Shown only to people, emitted as a `notice` event. |
| `Halt`, `HaltReason` | Ends the turn after this hook point. The history stays valid. |

An **asynchronous hook** runs outside the main flow, and the agent does not wait for it. An asynchronous hook can only return a `kernel.Injection`, which has the following fields:

| Field | Effect |
|---|---|
| `AdditionalContext` | Delivered as a reminder with the next model call or the next turn. |
| `SystemMessage` | Shown only to people. |
| `Wake` | Allows the hook to wake an idle session and start a new turn, subject to `WakeLimit`. Because the injected content is a reminder, a turn started this way also counts toward `Limits.MaxConsecutiveAutoTurns`. |

The return type itself guarantees that an asynchronous hook cannot change anything. At most `Limits.MaxAsyncHooks` asynchronous hooks run at the same time, 64 by default.

The input of every hook embeds `kernel.HookEnv`, which includes `Session`, `SessionID`, `Agent`, `TurnID`, `Step` and `Path`. For the `Session` methods you can call from a hook, see [19](19-callback-rules.md).

When a hook point has several synchronous hooks, they run one after another, ordered by `Priority`, then by scope (agent level before session level), then by registration order. A rewrite made by one hook is visible to the next. As soon as one hook blocks or denies, the hooks after it no longer run.

## 9.3 Hook points

The following table lists every hook point the kernel provides, with when it fires, the key used for matching, the default timeout, the default handling on failure, and what a synchronous hook can do there:

| Hook point | When it fires | Key | Default timeout | On failure | What a synchronous hook can do |
|---|---|---|---|---|---|
| `SessionStart` | When a session is created or resumed, and after compaction, clearing, rewind or a fork starts a new epoch. | The source: new, resume, clear, compact, rewind or fork. | 30s | Treated as no opinion. | Add context. |
| `SessionEnd` | When the session closes. | The close reason. | 5s | Treated as no opinion. | Do cleanup work. |
| `UserPromptSubmit` | Before input reaches the model. | The input source, such as human or command. | 30s | Blocks. | Block all or part of the input, or rewrite it. |
| `CommandInvoke` | Before a command runs. | The command name. | 10s | Blocks. | Block the command or rewrite its arguments. |
| `PreModelRequest` | Before each step sends its request. | The model name. | 10s | Treated as no opinion. | Provide a parameter patch for this step, set the reasoning parameters for the next turn, or supply a synthetic reply directly. |
| `PostModelResponse` | Before the model's reply is committed. | The stop reason. | 30s | Treated as no opinion. | Reject the reply. |
| `PreToolUse` | Before each tool call runs. | The tool name. | 60s | Denies. | Return allow, ask or deny, rewrite the input, or skip the tool and supply a result directly. |
| `Approval` | When a rule or a `pre_tool_use` hook requires approval. | The tool name. | None | Denies. | Give the approval verdict. `WithApprover` registers on this point. |
| `PostToolUse` | After each tool call ends. | The tool name. | 30s | Treated as no opinion. | Replace the result, or ask for the turn to stop once this batch of calls ends. |
| `PostToolBatch` | After a whole batch of tool calls has ended. | None. | 30s | Treated as no opinion. | Add context or stop the turn. |
| `Stop` | When the model gives its final answer and the turn is about to end. | None. | 30s | Treated as no opinion. | Ask for the turn to keep running. |
| `TurnEnd` | After every turn, whatever its outcome. | The turn's outcome. | 10s | Treated as no opinion. | Add context, which is delivered with the next turn. |
| `PreCompact` | Before compaction. | The trigger. | 30s | Treated as no opinion. | Block the compaction (except compaction caused by context window overflow), or rewrite the summary instructions. |
| `PostCompact` | After compaction. | The trigger. | 30s | Treated as no opinion. | Add context. |
| `ProviderSwitch` | At the start of the next turn, before a pending model switch actually takes effect. | The target model name. | 10s | Blocks. | Block the switch. |
| `TaskStart` | Before a task starts, restarts or continues. | The task kind. | 10s | Denies. | Deny the task, or rewrite its `TaskSpec`. |
| `TaskNotify` | Before a task notification reaches the model. | The task kind. | 10s | Treated as no opinion. | Rewrite the notification text, or keep the notification from waking the session. |

The "On failure" column shows the behavior under `FailDefault`, and you can change it for each hook with `Fail(p)`. After a synchronous hook times out or is canceled, the kernel waits a further `Limits.HookGrace` (1s by default) for it to return, and then stops waiting. The `Approval` point accepts only synchronous hooks.

## 9.4 Inputs and outputs of each point

The following table lists only the fields you can rewrite and the key input fields at each hook point. In addition, every input also contains `HookEnv`, and every output (except at the approval point) also contains `Common`.

| Point | Input | Output |
|---|---|---|
| `SessionStart` | `Source`, `Model` | None |
| `SessionEnd` | `Reason`, `Usage` | None |
| `UserPromptSubmit` | `Inputs []PromptInput`, each with `ID`, `Text`, `Parts`, `Origin` and `Metadata`. A single firing contains inputs from only one source. | `Block` drops all the inputs, `BlockIDs` drops the specified inputs, and `Reason` gives the reason. `UpdatedInputs` matches inputs by `ID` and rewrites their `Text`, `Parts` and `Metadata`, but cannot change their source. |
| `CommandInvoke` | `Name`, `Args`, `Busy` | `Block`, `Reason`, `UpdatedArgs *string` |
| `PreModelRequest` | `Model`, `Params`, `ToolNames`, `EstTokens` | `ParamsPatch` changes the parameters of this step only and can set `MaxOutputTokens`, `Temperature`, `TopP`, `StopSequences`, `ToolChoice`, `Headers` and `Extra`. `NextTurnReasoning` sets the reasoning parameters for the next turn. `Synthetic *message.Message` skips the model call and uses this message as the reply. |
| `PostModelResponse` | `Message` is the reply in its public form, without the API's raw data. There are also `Stop` and `Usage`. | `Reject` and `Reason`. A rejected reply does not enter the history, and the turn ends. |
| `PreToolUse` | `Call` (with `ID`, `Name` and `Input`), `Spec` | `Decision` is `Allow`, `Ask` or `Deny`, together with `Reason`. `UpdatedInput` replaces the whole input. `SyntheticResult` contains `Content` and `IsError`; when it is set, the tool is not run. |
| `Approval` | See [8.3](08-approval.md#83-approvers) | See [8.3](08-approval.md#83-approvers) |
| `PostToolUse` | `Call`, `Result`, `DurationMS` | `UpdatedResult *SyntheticResult`, `StopTurn` |
| `PostToolBatch` | `Calls`, `Results` | `StopTurn` |
| `Stop` | `LastAssistantText`, `StopHookActive` (the turn has already been continued), `Continuations` | `Continue` and `Reason`. `Reason` is sent to the model as a reminder. A turn can be continued at most `Limits.MaxStopContinuations` times, 8 by default. |
| `TurnEnd` | `Result TurnResult` | None |
| `PreCompact` | `Trigger` is manual, auto, overflow or switch. There are also `Instructions`, `PreTokens` and `Range`. | `Block`, `Reason`, `UpdatedInstructions *string` |
| `PostCompact` | `Trigger`, `Summary`, `PreTokens`, `PostTokens`, `Epoch` | None |
| `ProviderSwitch` | `From`, `To`, `DroppedReasoning`, `DroppedOpaque` | `Block`, `Reason` |
| `TaskStart` | `Spec`, `CallID`, `Attempt`, `Continuation`, `Instruction` | `Deny`, `Reason`, `UpdatedSpec`. `UpdatedSpec` cannot change `Kind`. |
| `TaskNotify` | `Info TaskInfo`, `Text` | `UpdatedText`, `NoWake` |

## 9.5 Events

Every hook run emits events. They are useful when you need to find out why an operation was blocked:

| Event | Fields |
|---|---|
| `hook.started` | `HookID`, `Point`, `Name`, `Async`, `CallID` |
| `hook.completed` | `HookID`, `Point`, `Name`, `CallID`, `Decision`, `DurationMs`, `SystemMessage` |
| `hook.failed` | `HookID`, `Point`, `Name`, `Error`, `Timeout`, `Canceled`, `PolicyApplied` (which failure policy was applied) |

## 9.6 Common uses

This section shows how to handle several common scenarios. You can take the code directly as a starting point and adapt it.

### Writing an audit log

Auditing does not need to change the flow, so it is a good fit for an asynchronous hook, which does not slow down the loop:

```go
kernel.WithAsyncHook(kernel.PostToolUse, "audit", func(ctx context.Context, in kernel.PostToolUseInput) (kernel.Injection, error) {
	audit.Write(in.SessionID, in.Call.Name, in.Call.Input, in.Result.Outcome, in.DurationMS)
	return kernel.Injection{}, nil
})
```

### Redacting secrets before a result reaches the model

The following hook checks the result text after the `bash` and `read_file` tools finish, and if the text contains secrets, replaces the original result with the cleaned text:

```go
kernel.WithHook(kernel.PostToolUse, "redact", func(ctx context.Context, in kernel.PostToolUseInput) (kernel.PostToolUseOutput, error) {
	text := message.Message{Content: in.Result.Content}.Text() // only text results are handled here
	if cleaned := redactSecrets(text); cleaned != text {
		return kernel.PostToolUseOutput{UpdatedResult: &kernel.SyntheticResult{Content: message.Content{&message.TextBlock{Text: cleaned}}}}, nil
	}
	return kernel.PostToolUseOutput{}, nil
}, kernel.Match(kernel.MatchNames("bash", "read_file")))
```

### Forcing a tool call in the first step

By setting `ToolChoice` in the `ParamsPatch` of `PreModelRequest`, you can require the model to call `make_plan` in the first step:

```go
kernel.WithHook(kernel.PreModelRequest, "plan-first", func(ctx context.Context, in kernel.PreModelRequestInput) (kernel.PreModelRequestOutput, error) {
	if in.Step == 1 {
		return kernel.PreModelRequestOutput{ParamsPatch: &kernel.ParamsPatch{ToolChoice: &provider.ToolChoice{Mode: provider.ToolChoiceNamed, Name: "make_plan"}}}, nil
	}
	return kernel.PreModelRequestOutput{}, nil
})
```

### Checking the format before the answer ends

A `Stop` hook can check the content when the model gives its final answer, and make the model continue if the answer does not meet your requirements. Checking `StopHookActive` keeps the hook from asking for a continuation again and again:

```go
kernel.WithHook(kernel.Stop, "checklist", func(ctx context.Context, in kernel.StopInput) (kernel.StopOutput, error) {
	if !in.StopHookActive && !strings.Contains(in.LastAssistantText, "## Summary") {
		return kernel.StopOutput{Continue: true, Reason: "End your answer with a '## Summary' section."}, nil
	}
	return kernel.StopOutput{}, nil
})
```

### Blocking user input with sensitive content

The following hook applies only to input whose source is human, and drops the inputs that violate the policy:

```go
kernel.WithHook(kernel.UserPromptSubmit, "policy", func(ctx context.Context, in kernel.UserPromptSubmitInput) (kernel.UserPromptSubmitOutput, error) {
	for _, p := range in.Inputs {
		if violates(p.Text) {
			return kernel.UserPromptSubmitOutput{BlockIDs: []string{p.ID}, Reason: "policy"}, nil
		}
	}
	return kernel.UserPromptSubmitOutput{}, nil
}, kernel.Match(kernel.MatchNames("human")))
```

### Adding background information after each turn

The `AdditionalContext` returned by a `TurnEnd` hook reaches the model with the next turn:

```go
kernel.WithHook(kernel.TurnEnd, "budget", func(ctx context.Context, in kernel.TurnEndInput) (kernel.TurnEndOutput, error) {
	return kernel.TurnEndOutput{Common: kernel.Common{AdditionalContext: fmt.Sprintf("Input tokens used in this turn: %d.", in.Result.Usage.InputTokens)}}, nil
})
```

## 9.7 Hook points defined by extensions

Besides the hook points the kernel provides, extensions can define their own with `kernel.NewHookPoint(def)`. You register hooks on them and fire them exactly as you do with the kernel's hook points. The built-in extensions define the following hook points:

| Point | When it fires | What it can do |
|---|---|---|
| `subagent.Start` | When a subagent task is about to open a child session. | Deny, rewrite the prompt, or add context for the first turn of the child session. |
| `subagent.Stop` | When a turn of the child session ends. | Ask the child session to continue; `Reason` becomes its next instruction. |
| `skills.Activate` | When the model or the user is about to load a skill. | Deny the load. |
| `peer.AuthRequired` | When a remote agent requires authentication. | Fix the credentials and have the call retried. |
| `mcp.Elicitation` | When an MCP server asks the user for input during a call. | Accept and attach content, or decline or cancel. |

To define a hook point in your own extension, give `Name`, `Key`, `Chain`, `Merge`, `Timeout`, `Fail` and `Closed` in a `HookPointDef`. `Key` provides the string that matchers use, `Chain` passes the rewrites of one hook on to the next hook, `Merge` combines multiple outputs, and `Closed` is the default output used on failure. To fire the hook point, call `kernel.Fire(ctx, sess, point, in)`.

## 9.8 Best practices

- Prefer asynchronous hooks whenever you can. Logic that does not change the flow, such as auditing, statistics and notifications, fits well in `WithAsyncHook`, and the model does not wait for it.
- Make synchronous hooks return quickly, and give them a timeout. A synchronous hook sits on the critical path of the loop, so if it calls an external service, set its `Timeout` yourself and think carefully about which `Fail` policy to use.
- Use `Match` to narrow where a hook fires instead of checking the tool name inside the hook. When the matcher does not match, the hook is not called at all, and no events are produced.
- Handle errors carefully at hook points that fail closed by default. `UserPromptSubmit`, `CommandInvoke`, `PreToolUse`, `Approval`, `ProviderSwitch` and `TaskStart` block or deny by default when a hook returns an error, times out or panics, so a single bug in a hook can stop the whole agent from working.
- Keep the shape of the data when you rewrite it. `UpdatedInput` replaces the original input entirely, so what you return must still be valid tool input. `UpdatedInputs` cannot change the source of an input.
- Write `AdditionalContext` for the model in English with neutral wording. Put content meant for people into `SystemMessage`.
- Shared state used by `PostToolUse` hooks in a parallel group needs a lock, because these hooks run concurrently.

---

[← Previous: Approval](08-approval.md) · [Contents](README.md) · [Next: Commands →](10-commands.md)
