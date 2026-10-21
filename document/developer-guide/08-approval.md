[← 上一章：注册工具](07-tools.md) · [目录](README.md) · [下一章：Hook →](09-hooks.md)

# 8. 审查（human in the loop）

审查机制让人或外部系统在工具真正执行之前进行把关。本章介绍如何用规则决定哪些调用需要审查，如何注册审查者给出结论，结论如何生效，以及如何在终端和 Web 两种界面中接入审查。

本章内容：[8.1 规则](#81-规则) · [8.2 规则的求值方式](#82-规则的求值方式) · [8.3 审查者](#83-审查者) · [8.4 事件](#84-事件) · [8.5 接入示例](#85-接入示例) · [8.6 注意事项](#86-注意事项) · [8.7 建议做法](#87-建议做法)

审查由两部分组成。**规则**决定哪些调用需要审查，**审查者**负责给出结论。两者都可以在 agent 级和 session 级设置，session 级的设置追加在 agent 级之后。

## 8.1 规则

审查规则是纯数据，可以从配置文件加载，并且在各语言版本的 SDK 之间通用。规则的 schema 见 `spec/schema/approval_rule.schema.json`。下面是一组规则的示例：

```json
[
  {"id": "shell-rm", "tool": "bash", "when": {"command": {"glob": "rm *"}}, "action": "ask"},
  {"id": "prod-db", "tool": "sql_exec", "when": {"db": {"eq": "prod"}}, "action": "deny", "reason": "禁止直接改生产库"},
  {"id": "home-only", "tool": "write_file", "when": {"path": {"prefix": "/home/", "normalize": "path"}}, "action": "allow"},
  {"id": "read-ok", "tool": "read_*", "action": "allow"},
  {"id": "everything-else", "tool": "*", "action": "ask"}
]
```

解析规则后，可以在 agent 级注册，也可以在新建 session 时追加：

```go
rules, err := kernel.ParseApprovalRules(data)
kernel.WithApprovalRules(rules...)                       // 在 agent 级注册
ag.NewSession(ctx, kernel.SessionApprovalRules(more...)) // 在 session 级追加
```

每条规则包含以下字段：

| 字段 | 说明 |
|---|---|
| `id` | 规则 id，在同一组规则内必须唯一。它会出现在拒绝理由和事件中。 |
| `tool` | 工具名，可以精确匹配，也可以用整串 glob（支持 `*` 和 `?`）匹配。`*` 匹配全部工具。 |
| `when` | 按输入字段进行匹配。键是用点分隔的字段路径，其中数字段用于索引数组，`*` 匹配任意元素，例如 `files.*.path`。所有条件都成立时规则才算命中。 |
| `action` | 命中后的动作，取值为 `allow`、`ask` 或 `deny`。 |
| `reason` | 规则的理由，拒绝时模型会看到它。 |

`when` 中每个条件的值只能设置一个运算符。可用的运算符如下：

| 运算符 | 含义 | 例子 |
|---|---|---|
| `eq` | 按 JSON 判断相等。数字按值比较，对象不考虑键的顺序。 | `{"eq": "prod"}`、`{"eq": 3}` |
| `glob` | 对整个字符串进行 glob 匹配。 | `{"glob": "rm *"}` |
| `regex` | 对整个字符串进行正则匹配，只支持可移植子集。子集包括 ASCII 范围的 `\d` `\w` `\s` `\b`、量词、`(?:)` 和 `\|`，不支持行内 flag、`^`、`$`、反向引用和环视。 | `{"regex": "git (push\|reset) .*"}` |
| `prefix` | 按前缀匹配。加上 `"normalize": "path"` 后，会先按 POSIX 路径规则清理字段值（去掉 `..` 和重复斜杠），再进行比较。 | `{"prefix": "/home/", "normalize": "path"}` |
| `in` | 字段值等于列表中的任意一项。 | `{"in": ["dev", "staging"]}` |
| `exists` | 判断字段存在或不存在。 | `{"exists": true}` |

求值时有两点需要注意。字段不存在时，只有 `{"exists": false}` 成立，其他运算符都不成立。如果字段值的类型与运算符不匹配，例如对数字使用 `glob`，条件同样不成立。

在 Go 中，你还可以在 `kernel.ApprovalRule` 上设置 `Match func(ToolCallView) bool`，用任意逻辑判断是否命中。但这样的规则无法写入文件，也无法在不同语言之间共享。

> 注意：当规则匹配的是 SQL、shell 命令这类自由文本字段时，规则并不构成安全边界，因为同一个意图总能换一种写法表达。对于真正危险的工具，应使用 `ask` 交给人查看，或者干脆不注册它。

## 8.2 规则的求值方式

一次工具调用可能同时命中多条规则，也可能同时受到 hook 的影响。SDK 按以下方式得出最终结论：

- 多条规则同时命中时，`deny` 的优先级最高。显式的 `allow` 可以豁免 `ask`，但不能豁免 `deny`。如果没有任何规则命中，调用会被放行。
- `pre_tool_use` hook 的结论与规则的结论之间取更严格的一方。只要任何一方给出 `ask`，另一方的 allow 都无法豁免它。只要任何一方给出 `deny`，调用就会被拒绝。
- 规则在 `pre_tool_use` 改写之后的输入上求值。输入会先经过规范化，重复键会被直接拒绝，键名的大小写必须精确匹配。因此，规则、审查者和工具看到的是同一份输入。
- 如果审查者改写了输入，SDK 会在新输入上重新求值规则，并从头再审一遍。
- subagent 和后台任务中的调用，也交给同一套规则和审查者处理。派生出的 agent 一定会继承父 agent 的规则和审查者，无法绕开它们。

## 8.3 审查者

审查者是一个函数，接收一次审查请求并返回结论。用 `kernel.WithApprover` 注册审查者：

```go
kernel.WithApprover(func(ctx context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
	return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
})
```

审查者收到的 `ApprovalRequest` 包含以下字段：

| 字段 | 说明 |
|---|---|
| `ApprovalID` | 这次审查的 id，与 `approval.requested` 事件中的 id 对应。 |
| `Call` | 一个 `ToolCallView`，包含 `CallID`、`Name` 和 `Input`。`Input` 是规范化之后的输入，与工具收到的输入逐字节相同。 |
| `Rules` | 命中的规则列表，每项包含 `ID`、`Action` 和 `Reason`。 |
| `HookReasons` | `pre_tool_use` hook 给出 `ask` 时附带的理由。 |
| `BatchID`、`BatchSize` | 同一批工具调用的 id 和调用个数，UI 可以据此把一批调用集中展示。 |
| `Round` | 当前是第几轮审查。审查者改写输入后会重新审查，轮次随之加一。 |
| `ApprovedInput` | 前面的审查者批准过的输入。只有后面的审查者改写了输入时才有值。 |
| `SessionID`、`Agent`、`TurnID`、`Step`、`Path` | 发起审查的 session 的信息。`Path` 非空表示调用来自 subagent 或后台任务。 |

审查者返回的 `ApprovalDecision` 中，`Verdict` 字段决定结论的效果：

| `Verdict` | 效果 |
|---|---|
| `VerdictApprove` | 批准调用。可以同时设置 `UpdatedInput` 改写输入，改写后会重新审查。 |
| `VerdictDeny` | 拒绝调用，模型会收到 “Denied by reviewer: <Reason>”。 |
| `VerdictDenyAndStop` | 拒绝调用，并在这一批调用执行完之后结束本轮。 |

## 8.4 事件

审查过程会发出以下两个事件：

| 事件 | 字段 |
|---|---|
| `approval.requested` | `ApprovalID`、`CallID`、`Name`、`Input`、`Rules`、`BatchID`、`BatchSize` |
| `approval.resolved` | `ApprovalID`、`CallID`、`Verdict`、`Reason`、`By`（表示由哪个审查者给出结论）、`InputUpdated`、`DurationMs` |

`approval.requested` 事件在调用审查者之前就会发出。因此，UI 可以根据这个事件显示“等待审批”的状态，再由审查者去等待 UI 返回用户的回答。

## 8.5 接入示例

本节给出几种常见的接入方式，包括终端交互、Web 界面、为审查设置时间上限，以及自动审查与人工审查的组合。

### 在终端中直接询问用户

在命令行程序中，最简单的做法是在审查者函数里直接打印请求并读取用户的输入：

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

### 在 Web 界面中接入

在 Web 应用中，审查者无法直接与用户交互。常见的做法是让审查者等待一个 channel，再由 HTTP 接口在用户做出选择后向这个 channel 写入结论：

```go
type pending struct {
	mu   sync.Mutex
	wait map[string]chan kernel.ApprovalDecision // 以 ApprovalID 作为 key
}

func (p *pending) approve(ctx context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
	ch := make(chan kernel.ApprovalDecision, 1)
	p.mu.Lock()
	p.wait[req.ApprovalID] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.wait, req.ApprovalID); p.mu.Unlock() }()

	// 前端通过 approval.requested 事件得知这条请求，用户点击后调用 POST /approvals/{id}
	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done(): // 轮次被打断或 session 已关闭
		return kernel.ApprovalDecision{Verdict: kernel.VerdictDeny, Reason: "canceled"}, ctx.Err()
	}
}

// 处理 POST /approvals/{id}
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

### 为审查设置时间上限

`WithApprover` 等价于在 `kernel.Approval` 这个 hook 点上注册一个名为 `approver` 的同步 hook，默认没有超时。如果你需要为审查设置时间上限，可以自己注册这个 hook 并指定超时：

```go
kernel.WithHook(kernel.Approval, "web-approver", p.approve, kernel.Timeout(10*time.Minute))
```

审查超时会按拒绝处理。

### 先自动审查，再交给人工

多个审查者会按 `Priority` 和注册顺序依次审查。任何一个审查者拒绝，结论就是拒绝。前一个审查者批准后，后一个审查者仍然会看到这次请求。因此，自动审查者应当只负责批准它能确定安全的调用，对于拿不准的调用也返回批准，把最终决定权留给后面的人工审查者：

```go
kernel.WithHook(kernel.Approval, "auto", func(ctx context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
	if isKnownSafe(req.Call) {
		return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
	}
	return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove, Reason: "needs a person"}, nil // 交给下一个审查者决定
}, kernel.Priority(-1))
kernel.WithHook(kernel.Approval, "human", p.approve)
```

审查者之间没有“跳过后面的审查者”这样的机制。如果你希望人工审查者只处理自动审查没有把握的那部分调用，需要在人工审查者中查看 `req.Rules` 和 `req.Call`，自行判断。

## 8.6 注意事项

- **审查默认没有超时**，SDK 一定会等待审查者返回。等待期间只有这一批调用被阻塞，事件和其他任务照常进行。
- 当轮次被打断或 session 关闭时，`ctx` 会被取消，审查者应当响应 `ctx.Done()`。
- 一批调用中命中 `ask` 的调用会**并发**请求审查，整批调用都有结论之后才开始执行。
- **没有注册审查者时，`ask` 一律按拒绝处理。**
- 审查者返回 Go `error` 时也按拒绝处理。模型看到的是“审查者出错”这类固定文字，而不是错误的原文。

## 8.7 建议做法

- 建议把规则分为三层。明确安全的调用（例如只读工具）用 `allow`，明确危险的调用（例如修改生产库、删除文件）用 `deny`，其余用 `ask`。最后再加一条 `{"tool": "*", "action": "ask"}` 覆盖所有剩余情况，这样新增的工具默认就需要审查。
- 规则文件应按环境区分。开发环境可以宽松一些，生产环境则应严格，并用 `SessionApprovalRules` 按用户角色追加规则。
- 在 UI 中展示审查请求时，应按 `BatchID` 分组。这样用户可以一次看完整批调用，而不是被逐个弹窗打断。
- 审查者只需把请求交给人或策略引擎，然后等待结论。改写输入需要谨慎，因为改写后的输入会重新经过规则求值。
- 建议记录 `approval.resolved` 事件，它是审计日志中最有价值的一条记录。

---

[← 上一章：注册工具](07-tools.md) · [目录](README.md) · [下一章：Hook →](09-hooks.md)
