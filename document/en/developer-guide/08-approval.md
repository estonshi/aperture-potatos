[← Previous: Registering tools](07-tools.md) · [Contents](README.md) · [Next: Hooks →](09-hooks.md)

# 8. Approval (human in the loop)

Approval lets a person or an external system check a tool call before it actually runs. This chapter covers how rules decide which calls need approval, how to register approvers that give a verdict, how a verdict takes effect, and how to integrate approval into a terminal or a web UI.

In this chapter: [8.1 Rules](#81-rules) · [8.2 How rules are evaluated](#82-how-rules-are-evaluated) · [8.3 Approvers](#83-approvers) · [8.4 Events](#84-events) · [8.5 Integration examples](#85-integration-examples) · [8.6 Things to keep in mind](#86-things-to-keep-in-mind) · [8.7 Best practices](#87-best-practices)

Approval has two parts. **Rules** decide which calls need approval, and **approvers** give the verdict. You can set both at the agent level and at the session level; session-level settings are appended after the agent-level ones.

## 8.1 Rules

Approval rules are plain data. You can load them from a configuration file, and they work the same way in the SDKs for every language. The schema for rules is `spec/schema/approval_rule.schema.json`. Here is an example set of rules:

```json
[
  {"id": "shell-rm", "tool": "bash", "when": {"command": {"glob": "rm *"}}, "action": "ask"},
  {"id": "prod-db", "tool": "sql_exec", "when": {"db": {"eq": "prod"}}, "action": "deny", "reason": "Direct changes to the production database are not allowed"},
  {"id": "home-only", "tool": "write_file", "when": {"path": {"prefix": "/home/", "normalize": "path"}}, "action": "allow"},
  {"id": "read-ok", "tool": "read_*", "action": "allow"},
  {"id": "everything-else", "tool": "*", "action": "ask"}
]
```

After parsing the rules, you can register them at the agent level or append more when you create a session:

```go
rules, err := kernel.ParseApprovalRules(data)
kernel.WithApprovalRules(rules...)                       // register at the agent level
ag.NewSession(ctx, kernel.SessionApprovalRules(more...)) // append at the session level
```

Each rule has the following fields:

| Field | Description |
|---|---|
| `id` | The rule id, which must be unique within a rule set. It appears in denial reasons and in events. |
| `tool` | The tool name, matched exactly or with a whole-string glob (`*` and `?` are supported). `*` matches every tool. |
| `when` | Matches on input fields. Each key is a dot-separated field path, in which numeric segments index into arrays and `*` matches any element, for example `files.*.path`. The rule matches only when every condition holds. |
| `action` | What to do when the rule matches: `allow`, `ask` or `deny`. |
| `reason` | The reason for the rule. The model sees it when a call is denied. |

Each condition in `when` can set only one operator. The available operators are:

| Operator | Meaning | Example |
|---|---|---|
| `eq` | JSON equality. Numbers are compared by value, and key order in objects does not matter. | `{"eq": "prod"}`, `{"eq": 3}` |
| `glob` | Glob match against the whole string. | `{"glob": "rm *"}` |
| `regex` | Regular expression match against the whole string, limited to a portable subset. The subset includes ASCII-range `\d` `\w` `\s` `\b`, quantifiers, `(?:)` and `\|`. Inline flags, `^`, `$`, backreferences and lookaround are not supported. | `{"regex": "git (push\|reset) .*"}` |
| `prefix` | Prefix match. With `"normalize": "path"`, the field value is first cleaned by POSIX path rules (removing `..` and duplicate slashes) before it is compared. | `{"prefix": "/home/", "normalize": "path"}` |
| `in` | The field value equals any item in the list. | `{"in": ["dev", "staging"]}` |
| `exists` | Tests whether the field exists or not. | `{"exists": true}` |

Two details matter during evaluation. When a field does not exist, only `{"exists": false}` holds, and every other operator fails. When the type of the field value does not fit the operator, for example `glob` on a number, the condition also fails.

In Go, you can also set `Match func(ToolCallView) bool` on a `kernel.ApprovalRule` to decide with arbitrary logic whether the rule matches. Such a rule, however, cannot be written to a file or shared across languages.

> Note: When a rule matches a free-text field such as SQL or a shell command, the rule is not a security boundary, because the same intent can always be expressed in another way. For truly dangerous tools, use `ask` so that a person looks at each call, or do not register the tool at all.

## 8.2 How rules are evaluated

A single tool call can match several rules at once and can also be affected by hooks. The SDK reaches the final verdict as follows:

- When several rules match, `deny` takes precedence. An explicit `allow` can override `ask` but not `deny`. If no rule matches, the call is allowed.
- Between the verdict of the `pre_tool_use` hooks and the verdict of the rules, the stricter one wins. If either side says `ask`, an allow from the other side cannot override it. If either side says `deny`, the call is denied.
- Rules are evaluated on the input as rewritten by `pre_tool_use`. The input is normalized first: duplicate keys are rejected outright, and key names must match exactly, including case. Rules, approvers and the tool therefore all see the same input.
- If an approver rewrites the input, the SDK evaluates the rules again on the new input and runs the whole approval again from the start.
- Calls from subagents and background tasks go through the same rules and approvers. A derived agent always inherits its parent agent's rules and approvers and cannot get around them.

## 8.3 Approvers

An approver is a function that receives an approval request and returns a verdict. Register one with `kernel.WithApprover`:

```go
kernel.WithApprover(func(ctx context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
	return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
})
```

The `ApprovalRequest` an approver receives has the following fields:

| Field | Description |
|---|---|
| `ApprovalID` | The id of this approval, which matches the id in the `approval.requested` event. |
| `Call` | A `ToolCallView` with `CallID`, `Name` and `Input`. `Input` is the normalized input, byte for byte the same as the input the tool receives. |
| `Rules` | The list of rules that matched, each with `ID`, `Action` and `Reason`. |
| `HookReasons` | The reasons attached when a `pre_tool_use` hook returned `ask`. |
| `BatchID`, `BatchSize` | The id of the batch of tool calls this call belongs to and the number of calls in it. The UI can use them to show a batch together. |
| `Round` | Which round of approval this is. When an approver rewrites the input, approval runs again and the round goes up by one. |
| `ApprovedInput` | The input that earlier approvers approved. It is set only when a later approver has rewritten the input. |
| `SessionID`, `Agent`, `TurnID`, `Step`, `Path` | Information about the session that requested approval. A non-empty `Path` means the call comes from a subagent or a background task. |

In the `ApprovalDecision` an approver returns, the `Verdict` field determines the effect:

| `Verdict` | Effect |
|---|---|
| `VerdictApprove` | Approves the call. You can also set `UpdatedInput` to rewrite the input, in which case approval runs again. |
| `VerdictDeny` | Denies the call. The model receives "Denied by reviewer: <Reason>". |
| `VerdictDenyAndStop` | Denies the call and ends the turn once this batch of calls has run. |

## 8.4 Events

Approval emits the following two events:

| Event | Fields |
|---|---|
| `approval.requested` | `ApprovalID`, `CallID`, `Name`, `Input`, `Rules`, `BatchID`, `BatchSize` |
| `approval.resolved` | `ApprovalID`, `CallID`, `Verdict`, `Reason`, `By` (which approver gave the verdict), `InputUpdated`, `DurationMs` |

The `approval.requested` event is emitted before any approver is called. The UI can therefore show a "waiting for approval" state based on this event, while the approver waits for the UI to return the user's answer.

## 8.5 Integration examples

This section shows several common ways to integrate approval: interacting in a terminal, a web UI, setting a time limit for approval, and combining automatic review with human review.

### Asking the user in a terminal

In a command-line program, the simplest approach is to print the request in the approver function and read the user's answer directly:

```go
kernel.WithApprover(func(ctx context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
	fmt.Printf("\n%s %s\n[y/n/e(dit)] ", req.Call.Name, req.Call.Input)
	switch readLine() {
	case "y":
		return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
	case "e":
		return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove, UpdatedInput: json.RawMessage(readLine())}, nil
	default:
		return kernel.ApprovalDecision{Verdict: kernel.VerdictDeny, Reason: "user declined"}, nil
	}
})
```

### Integrating with a web UI

In a web application, the approver cannot interact with the user directly. A common approach is to have the approver wait on a channel, and have an HTTP endpoint write the verdict to that channel once the user has made a choice:

```go
type pending struct {
	mu   sync.Mutex
	wait map[string]chan kernel.ApprovalDecision // keyed by ApprovalID
}

func (p *pending) approve(ctx context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
	ch := make(chan kernel.ApprovalDecision, 1)
	p.mu.Lock()
	p.wait[req.ApprovalID] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.wait, req.ApprovalID); p.mu.Unlock() }()

	// The frontend learns about this request from the approval.requested event and calls POST /approvals/{id} when the user clicks
	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done(): // the turn was interrupted or the session was closed
		return kernel.ApprovalDecision{Verdict: kernel.VerdictDeny, Reason: "canceled"}, ctx.Err()
	}
}

// Handles POST /approvals/{id}
func (p *pending) resolve(id string, d kernel.ApprovalDecision) bool {
	p.mu.Lock()
	ch, ok := p.wait[id]
	p.mu.Unlock()
	if ok {
		ch <- d
	}
	return ok
}
```

### Setting a time limit for approval

`WithApprover` is equivalent to registering a synchronous hook named `approver` on the `kernel.Approval` hook point, and by default it has no timeout. If you need a time limit for approval, register the hook yourself and give it a timeout:

```go
kernel.WithHook(kernel.Approval, "web-approver", p.approve, kernel.Timeout(10*time.Minute))
```

An approval that times out is treated as a denial.

### Automatic review before a human

Multiple approvers review a request one after another, in order of `Priority` and then registration order. If any approver denies, the verdict is a denial. After one approver approves, the next approver still sees the request. An automatic approver should therefore limit itself to approving the calls it can be sure are safe. For calls it is unsure about, it should also return approve and leave the final decision to the human approver that comes after it:

```go
kernel.WithHook(kernel.Approval, "auto", func(ctx context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
	if isKnownSafe(req.Call) {
		return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
	}
	return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove, Reason: "needs a person"}, nil // let the next approver decide
}, kernel.Priority(-1))
kernel.WithHook(kernel.Approval, "human", p.approve)
```

Approvers have no mechanism for skipping the approvers after them. If you want the human approver to handle only the calls the automatic approver was unsure about, inspect `req.Rules` and `req.Call` in the human approver and decide there.

## 8.6 Things to keep in mind

- **Approval has no timeout by default**, and the SDK always waits for the approver to return. While it waits, only this batch of calls is blocked; events and other tasks continue as usual.
- When the turn is interrupted or the session closes, `ctx` is canceled, and the approver should respond to `ctx.Done()`.
- The calls in a batch that hit `ask` request approval **concurrently**, and the batch starts running only after every call in it has a verdict.
- **When no approver is registered, `ask` is always treated as a denial.**
- When an approver returns a Go `error`, that is also treated as a denial. The model sees a fixed text saying that the approver failed, not the original error.

## 8.7 Best practices

- Split your rules into three tiers. Use `allow` for calls that are clearly safe (such as read-only tools), `deny` for calls that are clearly dangerous (such as changing the production database or deleting files), and `ask` for everything else. Finish with a `{"tool": "*", "action": "ask"}` rule that covers whatever is left, so that newly added tools require approval by default.
- Keep separate rule files for each environment. Development can be more permissive, while production should be strict, with `SessionApprovalRules` appending rules according to the user's role.
- When you show approval requests in the UI, group them by `BatchID`. The user can then review a whole batch at once instead of being interrupted by one pop-up after another.
- An approver only needs to hand the request to a person or a policy engine and wait for the verdict. Be careful about rewriting input, because rewritten input goes through rule evaluation again.
- Record the `approval.resolved` event. It is the most valuable entry in an audit log.

---

[← Previous: Registering tools](07-tools.md) · [Contents](README.md) · [Next: Hooks →](09-hooks.md)
