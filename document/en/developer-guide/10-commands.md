[← Previous: Hooks](09-hooks.md) · [Contents](README.md) · [Next: System prompt and dynamic context →](11-prompt.md)

# 10. Commands

Commands give users shortcuts of the form `/xxx`. You can turn prompts that users send often into template commands, or register a custom command that runs your own host logic. This chapter shows how to write both kinds of command, how a command coordinates with a turn that is already running, and which commands the SDK ships with. After reading it, you can add commands to your application and choose the right busy policy for each one.

In this chapter: [10.1 Template commands](#101-template-commands) · [10.2 Custom commands](#102-custom-commands) · [10.3 Busy policies](#103-busy-policies) · [10.4 Invocation, interception and events](#104-invocation-interception-and-events) · [10.5 Built-in commands](#105-built-in-commands) · [10.6 Best practices](#106-best-practices)

## 10.1 Template commands

A template command combines a fixed prompt with the arguments the user supplies to form a single input. Use it to give users a shortcut for requests they make often. Create a template command with `kernel.PromptCommand` and register it with `kernel.WithCommand`:

```go
kernel.WithCommand(kernel.PromptCommand("plan", "Plan a trip", "Plan a trip: $ARGUMENTS. Check the visa rules first."))
```

When the user sends `/plan a three-day trip to Tokyo`, the template expands into an ordinary input whose source is `command`, and the model answers it as usual.

You can use the following placeholders in a template:

| Placeholder | Meaning |
|---|---|
| `$ARGUMENTS` | Replaced with the full argument text exactly as the user typed it. |
| `$ARGUMENTS[N]`, `$N` | Replaced with the Nth argument, counting from 0. |
| `$name` | Replaced with a positional argument named through `kernel.WithArgs("dest", "days")`. |
| `$$` | A literal `$`. |
| No placeholder at all | The arguments are appended to the end of the template. |

Arguments are split the way a shell splits them. Whitespace separates arguments, text inside single quotes is kept exactly as written, text inside double quotes supports two escapes, `\"` and `\\`, and outside quotes a backslash escapes the single character that follows it. `kernel.SplitArgs(s)` is this splitting function, and custom commands can call it directly.

`PromptCommand` accepts two options. `kernel.WithArgs(names...)` names the positional arguments, and `kernel.PromptBusy(policy)` sets the busy policy, which defaults to `WaitIdle`.

## 10.2 Custom commands

When a command needs to run your host's own logic rather than just send a prompt to the model, construct a `kernel.Command` directly. The following example registers a command that shows the session status:

```go
kernel.WithCommand(kernel.Command{
	Name:        "status",
	Aliases:     []string{"st"},
	Description: "Show the session status",
	Busy:        kernel.RunNow,
	Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
		snap := cc.Session().Snapshot()
		return kernel.CommandResult{
			Text: fmt.Sprintf("phase=%s epoch=%d", snap.Phase, snap.Epoch),
			Data: snap,
		}, nil
	},
})
```

`Command` has the following fields:

| Field | Description |
|---|---|
| `Name`, `Aliases` | The command's name and its aliases. |
| `Description`, `ArgHint` | A human-readable description and an argument hint, such as `/compact [instructions]`. `/help` lists both. |
| `Busy` | Determines how the command is handled while a turn is running. See [10.3](#103-busy-policies). |
| `Hidden` | When set, the command does not appear in `Commands()` or in `session.started`. |
| `Run` | The logic the command runs. |

The `CommandContext` passed to `Run` provides the following methods. `Name()` returns the command name, `Args()` returns the raw argument string, `ID()` returns the id of this invocation, and `Session()` returns the session the command belongs to. `Output(text)` emits a `command.output` event while the command is running, so a long-running command can use it to report progress.

The `CommandResult` that `Run` returns has the following fields:

| Field | Description |
|---|---|
| `Text` | Human-readable text, written to `command.completed.Text`. |
| `Data` | A structured result, written to `command.completed.Data` and also available in `TurnResult.Command.Data`. |
| `Prompt *Input` | When non-nil, this input is handed to the model with the source `command`. Template commands are built on this field. |
| `Pin` | Added to the Pin set and injected again after compaction. |

## 10.3 Busy policies

A user may send a command while the model is still working. The command's `Busy` field determines how the command runs in that case, and you should choose the policy based on whether the command modifies history.

| `Busy` | Behavior while a turn is running |
|---|---|
| `WaitIdle` (default) | The command is queued and runs after the preceding turn ends. It acts as a barrier for inputs that arrive after it, and it may modify history. `/compact` and `/clear` use this policy. |
| `RunNow` | The command runs concurrently with the turn and **must not modify history**. It suits read-only queries. `/usage` and `/context` use this policy. |
| `InterruptFirst` | The current turn is interrupted first, and then the command runs. |
| `RejectBusy` | The command does not run. It ends immediately with `command.completed{error:"busy"}`, and `TurnResult.Err` on the receipt is `ErrBusy`. |

## 10.4 Invocation, interception and events

There are two ways to invoke a command. A `/name args` line typed by the user is sent with `sess.Send(kernel.Text("/name args"))`, and the kernel parses it and runs the command. When your program runs a command directly, it calls `sess.Command("name", "args")`, which does no text parsing. Both return a receipt. After you call `Wait`, `TurnResult.Outcome` is `command` and the command's result is in `TurnResult.Command`.

The two differ when the command name does not exist. `sess.Command()` returns `ErrUnknownCommand` for a command name that is not registered. `Send` treats the text as a command only when the `/` is followed by a registered command name or alias; otherwise it sends the text to the model as ordinary text.

Before a command runs, the `command_invoke` hook fires. The hook can block the invocation or rewrite its arguments. See [9.4](09-hooks.md#94-inputs-and-outputs-of-each-point).

While a command runs, it emits the following events in order. `command.started` carries `Name`, `Args` and `Busy`; `command.output` carries the text passed to `Output(text)`; and `command.completed` carries `Text`, `Data` and `Error`. The input that a template command expands into goes through `input.accepted` and `turn.started` as usual.

If your UI needs command autocompletion, call `sess.Commands()` to list the available commands. It returns a list of `CommandInfo`, and each entry contains `Name`, `Aliases`, `Description`, `ArgHint` and `Busy`. A command registered at session level shadows an agent-level command with the same name.

## 10.5 Built-in commands

`std.Defaults()` installs seven commands. Each command is a separate extension, so you can remove any of them with `std.Without("cmd.<name>")`.

| Command | Extension name | What it does | Busy policy |
|---|---|---|---|
| `/help` | `cmd.help` | Lists the available commands. | RunNow |
| `/usage` | `cmd.usage` | Shows token usage and context size. `Data` is a `usagecmd.Report` that contains usage, context and the cache hit rate. | RunNow |
| `/context` | `cmd.context` | Shows what the context is made of. | RunNow |
| `/compact [instructions]` | `cmd.compact` | Compacts the context. You can add instructions for the summary. | WaitIdle |
| `/clear` | `cmd.clear` | Clears the context. | WaitIdle. Use `clearcmd.New(clearcmd.Options{Interrupt: true})` to interrupt first instead. |
| `/rewind [n]` | `cmd.rewind` | Undoes the last n turns; the default is 1. | WaitIdle. Use `rewindcmd.Options{Interrupt: true}` to interrupt first instead. |
| `/fork [n]` | `cmd.fork` | Copies the context into a new session, optionally dropping the last n turns. `Data` is `forkcmd.Forked{SessionID}`, and the host opens the new session with `Resume`. | RunNow |

To replace one of the default instances, pass the new instance through `std.With`, for example `std.Defaults(std.With(clearcmd.New(clearcmd.Options{Interrupt: true})))`.

Besides these seven commands, other extensions register commands too. The skills extension registers each skill as `/<skill-name>`, and the mcp extension can register the prompts a server provides as `/mcp__<server>__<prompt>`.

## 10.6 Best practices

- Use `RunNow` for read-only query commands, so that users can see the result right away even while the model is working.
- Use `WaitIdle` for commands that modify history, and `InterruptFirst` when the command clearly needs to take effect immediately. Do not call `Compact`, `Clear` or `Rewind` from a `RunNow` command.
- Watch out for reentrancy when you call a waiting method from a command. A `WaitIdle` command runs on the session's own execution path, so it can call waiting methods such as `Compact` directly. A `RunNow` command runs concurrently, so any waiting method it calls is queued. See [19](19-callback-rules.md).
- Turn questions that users ask often into template commands. This uses fewer tokens than keeping that content in the system prompt all the time.
- Set `Hidden` on commands meant for internal use. For example, a `/_sync` command that only your UI triggers does not need to be shown to users.

---

[← Previous: Hooks](09-hooks.md) · [Contents](README.md) · [Next: System prompt and dynamic context →](11-prompt.md)
