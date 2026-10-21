[← 上一章：错误](20-errors.md) · [目录](README.md) · [下一章：常见问题 →](22-faq.md)

# 21. 测试

本章介绍如何在不连接真实模型的情况下测试你的 agent。你可以用脚本模拟模型的回答，并断言 kernel 发出的请求和产生的事件。如果要测试适配器，可以回放事先录制的 HTTP 响应。

本章内容：[21.1 用脚本模拟模型](#211-用脚本模拟模型) · [21.2 脚本辅助函数](#212-脚本辅助函数) · [21.3 编写一个测试](#213-编写一个测试) · [21.4 常用断言](#214-常用断言) · [21.5 保证结果可重复](#215-保证结果可重复) · [21.6 回放真实的 HTTP 响应](#216-回放真实的-http-响应) · [21.7 建议做法](#217-建议做法)

## 21.1 用脚本模拟模型

`providertest.ScriptProvider` 实现了 `provider.Provider` 接口，它按照你提供的脚本依次回答每一次请求，适合用来代替真实模型：

```go
p := providertest.NewScriptProvider(provider.APIAnthropicMessages, script1, script2)
p.Push(script3)      // 中途追加
reqs := p.Requests()  // kernel 实际发出的每个请求
```

每一次回答由一串 `providertest.Step` 组成。每一步通常是一个 `provider.StreamEvent`，也可以是一个错误或一段停顿。设置 `Err` 时，这一步返回错误并结束这次流。设置 `Hold` 时，这一步会停顿，正数表示停顿的时长，负数表示一直等到请求被取消，可以用来测试打断和超时。

## 21.2 脚本辅助函数

逐个构造流事件比较繁琐，建议在测试中自己编写 helper。下面的两个 helper 分别用于构造“回答一段文本”和“调用一个工具”的脚本：

```go
var u = message.Usage{InputTokens: 100, OutputTokens: 10}

func producer() *message.Producer {
	return &message.Producer{API: string(provider.APIAnthropicMessages), Endpoint: "script.invalid", Model: "m"}
}

func textReply(text string) []providertest.Step {
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel},
		Content: message.Content{&message.TextBlock{Text: text}}, Producer: producer(), Usage: &u, StopReason: "end_turn"}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
		{Event: provider.StreamEvent{Kind: provider.SETextDelta, Index: 0, Channel: "text", Text: text}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: &message.TextBlock{Text: text}}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn, Raw: "end_turn"}, Usage: &u}},
	}
}

func toolReply(id, name, input string) []providertest.Step {
	b := &message.ToolCallBlock{ID: id, Name: name, Input: json.RawMessage(input)}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel},
		Content: message.Content{b}, Producer: producer(), Usage: &u, StopReason: "tool_use"}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeToolCall, ToolCallID: id, ToolName: name}},
		{Event: provider.StreamEvent{Kind: provider.SEToolArgsDelta, Index: 0, ToolCallID: id, Text: input}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: b}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopToolUse, Raw: "tool_use"}, Usage: &u}},
	}
}
```

如果一次回复中需要包含多个工具调用，请为每个调用各写一组 `SEBlockStart`、`SEToolArgsDelta` 和 `SEBlockStop` 事件，并让 `Index` 依次递增，同时在 `Message.Content` 中按顺序放入所有的 `ToolCallBlock`。`kernel/helpers_test.go` 中有这种写法的示例。

## 21.3 编写一个测试

下面的测试脚本化了两次模型回答。第一次请求时模型调用 `get_weather` 工具，第二次请求时模型给出最终回答。测试最后检查这一轮的结果和模型被调用的次数：

```go
func TestWeather(t *testing.T) {
	p := providertest.NewScriptProvider(provider.APIAnthropicMessages,
		toolReply("c1", "get_weather", `{"city":"Tokyo"}`), // 第一次请求时模型调用工具
		textReply("Tokyo is sunny."),                       // 第二次请求时模型给出回答
	)
	ag, err := kernel.New(
		kernel.WithModel(provider.ModelRef{Provider: p, Model: "m"}),
		kernel.WithTool(weather),
		kernel.WithPanicOnInternalError(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close(context.Background())

	var events []event.Event
	sess, _ := ag.NewSession(t.Context(), kernel.WithSessionFunc(func(_ context.Context, ev event.Event) error {
		events = append(events, ev) // 回调运行在专用 goroutine 上，读取 events 之前要先 ag.Close 或者加锁
		return nil
	}))

	rcpt, _ := sess.Send(kernel.Text("Tokyo weather?"))
	res, err := rcpt.Wait(t.Context())
	if err != nil || res.Outcome != kernel.OutcomeCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	if res.FinalText != "Tokyo is sunny." || res.Steps != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(p.Requests()) != 2 {
		t.Fatalf("model called %d times", len(p.Requests()))
	}
}
```

## 21.4 常用断言

一个完整的测试通常从以下几个方面做断言：

- 检查这一轮的结果，即 `TurnResult` 的 `Outcome`、`Steps`、`FinalText` 和 `Usage`。
- 检查发给模型的请求。`p.Requests()` 中的每个 `*provider.Request` 都带有 `System`、`Tools`、`Messages` 和 `Params`，其中 `System` 是提示词的各个分段，`Messages` 是历史消息。例如，你可以断言第二次请求的最后一条消息是工具结果。
- 检查事件。把事件收集下来后按类型过滤，就可以断言工具被调用过、审查确实发生了以及 hook 已经运行，分别对应 `tool.completed` 的 `Outcome`、`approval.requested` 和 `hook.completed` 事件。
- 检查历史。历史可以从 `sess.Messages()` 或存储中的行读取，其中每个工具调用都应该有且只有一个结果。
- 检查收尾。`ag.Close` 之后，`sess.Snapshot().Workers` 应当为空，表示没有泄漏的 goroutine。

## 21.5 保证结果可重复

时间戳和 id 每次运行都会变化，这会让测试结果难以比对。以下选项可以让测试结果保持稳定：

| 选项 | 作用 |
|---|---|
| `kernel.WithClock(c)` | 固定时间戳。传入的值实现 `Now() time.Time` 即可。 |
| `kernel.WithIDGenerator(g)` | 固定 id。传入的值需要实现 `NewID(prefix string) string`。 |
| `kernel.WithPanicOnInternalError(true)` | kernel 出现 bug 时直接 panic，而不会被当作 session 关闭而掩盖。 |
| `store.NewMemory()` | 使用内存存储，不写入磁盘。 |

固定了时间和 id 之后，事件和历史就可以保存成 golden 文件，逐字节比对。

下面是几类常见场景的测试方法：

- 测试打断和超时时，在脚本中放入 `Step{Hold: -1}`，让流一直挂起，然后在另一个 goroutine 中调用 `sess.Interrupt`。之后断言 `Outcome` 为 `interrupted`，并且工具调用已经补上了结果。
- 测试审查时，注册一个审查者，让它记录收到的 `ApprovalRequest` 并返回固定的结论。之后断言 `approval.resolved` 事件和工具的 `Outcome`，后者应为 `denied` 或 `success`。
- 测试 hook 时，在 hook 中把收到的输入记录到变量里，然后断言 hook 被调用的次数和它看到的内容。测试失败策略时，让 hook 返回错误，并断言结果按照 `Fail` 的设定处理。

## 21.6 回放真实的 HTTP 响应

如果你要测试适配器能否正确解析真实的响应，或者要测试从适配器到 kernel 的整条链路，可以把 `providertest.FixtureTransport` 设为 `Config.Doer`：

```go
resp, _ := providertest.LoadResponse("testdata/anthropic/tool_call") // 目录中放 response.sse 或 response.json，也可以附带 headers.json
ft := providertest.NewFixtureTransport(resp, resp2)
ft.ChunkSeed = 1 // 随机切分响应体，用来测试解码器处理分片数据的健壮性
claude := anthropic.New(anthropic.Config{APIKey: "test", Doer: ft})
// ...
ft.Requests() // 返回适配器实际发出的 HTTP 请求，包括 Method、URL、Header 和 Body，可以用来做 golden 比对
```

`Response` 的 `HoldAfter` 和 `HoldFor` 字段用于测试流在中途被打断的情况，其中 `HoldAfter` 指定发出几个事件之后挂起。

要录制 fixture，可以把真实的 `http.Client` 包装进 `providertest.RecordingTransport{Next: client}`。运行结束后，调用 `Exchanges()` 即可拿到已经去掉凭据的请求和响应，再把它们保存成文件。

## 21.7 建议做法

- 建议为每个工具至少编写一个三步测试，依次是模型调用工具、工具返回结果、模型总结。这是最常见的执行路径。
- 断言提示词时应使用 `p.Requests()`，而不是 `sess.Messages()`，因为前者才是模型真正看到的内容。
- 审查和 hook 的测试应覆盖拒绝路径，并断言模型在下一次请求中看到了拒绝理由。
- 集成测试需要连接真实模型时，用 `RecordingTransport` 录制响应，之后改为回放即可。这样 CI 中就不需要密钥。
- 不建议在测试中用 `time.Sleep` 等待事件，请改用 `Receipt.Wait`、`WaitQuiet` 或事件 channel。

---

[← 上一章：错误](20-errors.md) · [目录](README.md) · [下一章：常见问题 →](22-faq.md)
