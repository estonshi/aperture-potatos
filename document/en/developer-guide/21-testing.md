[← Previous: Errors](20-errors.md) · [Contents](README.md) · [Next: Frequently asked questions →](22-faq.md)

# 21. Testing

This chapter shows how to test your agent without connecting to a real model. You can simulate the model's replies with a script and assert on the requests the kernel sends and the events it produces. To test an adapter, you can replay HTTP responses recorded in advance.

In this chapter: [21.1 Simulating a model with a script](#211-simulating-a-model-with-a-script) · [21.2 Script helpers](#212-script-helpers) · [21.3 Writing a test](#213-writing-a-test) · [21.4 Common assertions](#214-common-assertions) · [21.5 Keeping results reproducible](#215-keeping-results-reproducible) · [21.6 Replaying real HTTP responses](#216-replaying-real-http-responses) · [21.7 Best practices](#217-best-practices)

## 21.1 Simulating a model with a script

`providertest.ScriptProvider` implements the `provider.Provider` interface. It answers each request in turn from the scripts you give it, which makes it a good stand-in for a real model:

```go
p := providertest.NewScriptProvider(provider.APIAnthropicMessages, script1, script2)
p.Push(script3)      // append another script partway through
reqs := p.Requests()  // every request the kernel actually sent
```

Each reply is a sequence of `providertest.Step` values. A step is usually a `provider.StreamEvent`, but it can also be an error or a pause. When `Err` is set, the step returns that error and ends the stream. When `Hold` is set, the step pauses: a positive value is how long to pause, and a negative value waits until the request is canceled, which is useful for testing interrupts and timeouts.

## 21.2 Script helpers

Building stream events one at a time is tedious, so it is a good idea to write your own helpers in your tests. The two helpers below build a script that replies with a piece of text and a script that calls a tool:

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

If a single reply needs to contain several tool calls, write a separate set of `SEBlockStart`, `SEToolArgsDelta` and `SEBlockStop` events for each call, increasing `Index` by one for each call, and put all the `ToolCallBlock` values in `Message.Content` in the same order. `kernel/helpers_test.go` contains an example of this.

## 21.3 Writing a test

The test below scripts two model replies. On the first request, the model calls the `get_weather` tool; on the second, it gives its final answer. At the end, the test checks the result of the turn and how many times the model was called:

```go
func TestWeather(t *testing.T) {
	p := providertest.NewScriptProvider(provider.APIAnthropicMessages,
		toolReply("c1", "get_weather", `{"city":"Tokyo"}`), // on the first request, the model calls the tool
		textReply("Tokyo is sunny."),                       // on the second request, the model answers
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
		events = append(events, ev) // the callback runs on a dedicated goroutine, so call ag.Close or take a lock before reading events
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

## 21.4 Common assertions

A thorough test usually makes assertions in the following areas:

- Check the result of the turn: the `Outcome`, `Steps`, `FinalText` and `Usage` of the `TurnResult`.
- Check the requests sent to the model. Each `*provider.Request` in `p.Requests()` carries `System`, `Tools`, `Messages` and `Params`, where `System` holds the sections of the prompt and `Messages` holds the message history. For example, you can assert that the last message of the second request is a tool result.
- Check the events. Collect the events and filter them by type. You can then assert that a tool was called, that approval actually took place and that a hook ran, through the `Outcome` of `tool.completed`, the `approval.requested` event and the `hook.completed` event respectively.
- Check the history. You can read it from `sess.Messages()` or from the rows in the store, and every tool call in it should have exactly one result.
- Check shutdown. After `ag.Close`, `sess.Snapshot().Workers` should be empty, which shows that no goroutines leaked.

## 21.5 Keeping results reproducible

Timestamps and ids change on every run, which makes test results hard to compare. The following options keep test results stable:

| Option | Effect |
|---|---|
| `kernel.WithClock(c)` | Fixes timestamps. The value you pass only needs to implement `Now() time.Time`. |
| `kernel.WithIDGenerator(g)` | Fixes ids. The value you pass must implement `NewID(prefix string) string`. |
| `kernel.WithPanicOnInternalError(true)` | Makes the kernel panic as soon as it hits a bug, instead of masking the bug as a session close. |
| `store.NewMemory()` | Uses an in-memory store that writes nothing to disk. |

Once time and ids are fixed, you can save events and history as golden files and compare them byte for byte.

Here is how to test a few common scenarios:

- To test interrupts and timeouts, put `Step{Hold: -1}` in the script so that the stream hangs, and call `sess.Interrupt` from another goroutine. Then assert that `Outcome` is `interrupted` and that the tool calls have been given results.
- To test approval, register an approver that records the `ApprovalRequest` it receives and returns a fixed decision. Then assert on the `approval.resolved` event and on the tool's `Outcome`, which should be `denied` or `success`.
- To test a hook, have it record the input it receives in a variable, then assert how many times the hook was called and what it saw. To test a failure policy, have the hook return an error and assert that the result is handled as its `Fail` setting specifies.

## 21.6 Replaying real HTTP responses

To test whether an adapter parses real responses correctly, or to test the whole path from the adapter to the kernel, set a `providertest.FixtureTransport` as `Config.Doer`:

```go
resp, _ := providertest.LoadResponse("testdata/anthropic/tool_call") // the directory holds response.sse or response.json, and optionally headers.json
ft := providertest.NewFixtureTransport(resp, resp2)
ft.ChunkSeed = 1 // split the response body at random points to test how robustly the decoder handles chunked data
claude := anthropic.New(anthropic.Config{APIKey: "test", Doer: ft})
// ...
ft.Requests() // returns the HTTP requests the adapter actually sent, with Method, URL, Header and Body, for golden comparison
```

The `HoldAfter` and `HoldFor` fields of `Response` let you test a stream that is interrupted partway through. `HoldAfter` sets how many events are sent before the stream hangs.

To record a fixture, wrap a real `http.Client` in `providertest.RecordingTransport{Next: client}`. After the run, call `Exchanges()` to get the requests and responses with credentials already stripped, and save them to files.

## 21.7 Best practices

- Write at least one three-step test for each tool: the model calls the tool, the tool returns a result, and the model summarizes it. This is the most common execution path.
- To assert on the prompt, use `p.Requests()` rather than `sess.Messages()`, because the requests are what the model actually sees.
- Tests for approval and hooks should cover the rejection path and assert that the model sees the reason for the rejection in its next request.
- When an integration test needs a real model, record the responses with `RecordingTransport` and then switch the test to replay them. That way, CI does not need any keys.
- Do not use `time.Sleep` in tests to wait for events. Use `Receipt.Wait`, `WaitQuiet` or an event channel instead.

---

[← Previous: Errors](20-errors.md) · [Contents](README.md) · [Next: Frequently asked questions →](22-faq.md)
