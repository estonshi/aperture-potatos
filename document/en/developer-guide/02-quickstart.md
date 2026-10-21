[← Previous: Introduction](01-overview.md) · [Contents](README.md) · [Next: Core objects →](03-core-objects.md)

# 2. Quick start

This chapter walks through a minimal example that you can run as is. It shows the complete code for an agent, from configuration and session creation to receiving a streamed answer. Later chapters cover each part of this example in detail.

In this chapter: [2.1 A complete example](#21-a-complete-example) · [2.2 How the code works](#22-how-the-code-works) · [2.3 Next steps](#23-next-steps)

## 2.1 A complete example

The program below implements a minimal agent. It registers a tool that looks up the weather, asks an approver for confirmation before the tool is called, and prints the model's answer to the terminal as it streams in.

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/anthropic"
	"github.com/estonshi/aperture-potatos/std"
	"github.com/estonshi/aperture-potatos/store/jsonl"
)

type weatherIn struct {
	City string `json:"city"`
}

func main() {
	ctx := context.Background()

	// 1. Model: one Provider instance can be shared by many sessions
	claude := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY")})

	// 2. Tool: the input is decoded strictly according to its json tags
	weather := kernel.NewTool(kernel.ToolSpec{
		Name:        "get_weather",
		Description: "Current weather of a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
		Concurrency: kernel.Parallel,
	}, func(ctx context.Context, tc *kernel.ToolContext, in weatherIn) (kernel.ToolResult, error) {
		return kernel.TextResult(in.City + ": sunny, 22°C"), nil
	})

	// 3. Approval rules: ask the approver before calling get_weather
	rules, err := kernel.ParseApprovalRules([]byte(`[{"id":"weather","tool":"get_weather","action":"ask"}]`))
	if err != nil {
		log.Fatal(err)
	}

	// 4. Agent: one configuration
	ag, err := kernel.New(
		std.Defaults(), // default extensions: compaction, history search, task management tools, subagent, and commands such as /compact
		kernel.WithNickname("Tudou"),
		kernel.WithInstructions("Answer in the user's language."),
		kernel.WithModel(provider.ModelRef{Provider: claude, Model: "claude-sonnet-5-5"}),
		kernel.WithStore(jsonl.New("./sessions")),
		kernel.WithTool(weather),
		kernel.WithApprovalRules(rules...),
		kernel.WithApprover(func(ctx context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
			fmt.Printf("\n[approve] %s %s\n", req.Call.Name, req.Call.Input)
			return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer ag.Close(ctx)

	// 5. Session: one conversation
	sess, err := ag.NewSession(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer sess.Close(ctx)

	// 6. Run sends one input and yields every event of the turn until turn.completed
	for ev, err := range sess.Run(ctx, kernel.Text("What's the weather like in Tokyo?")) {
		if err != nil {
			log.Fatal(err)
		}
		switch d := ev.Data.(type) {
		case *event.MessageDelta:
			fmt.Print(d.Text)
		case *event.ToolCompleted:
			fmt.Printf("\n[tool %s: %s]\n", d.Name, d.Outcome)
		case *event.TurnCompleted:
			fmt.Printf("\n[turn %s, %d steps]\n", d.Outcome, d.Steps)
		}
	}
}
```

## 2.2 How the code works

- `kernel.New` does not install the default extensions on its own, so you have to add `std.Defaults()` explicitly. The agent still runs without it, but it has no automatic compaction, no slash commands and no task management tools.
- You create the Provider for the model yourself and then hand it to the kernel with `WithModel`. To change models, you only need to change this one line.
- If you do not configure `WithStore`, sessions are kept only in memory and are lost when the process exits.
- `Run` suits command-line programs, where you send one message and wait for the turn to finish. To push streaming output to a web page, use `Send` together with a subscription instead; see [5.10](05-input.md#510-common-integrations).
- The approver in the example approves the call outright. In a real application, the approver should show `req.Call` to a person and wait for that person to decide; see Chapter [8](08-approval.md).

## 2.3 Next steps

| Task | See |
|---|---|
| Use OpenAI, DeepSeek or your own gateway instead. | [4. Connecting models](04-models.md) |
| Connect the agent to an HTTP service and push streaming output to the front end. | [5. Sending input](05-input.md), [6. Subscribing to events](06-events.md) |
| Register more tools. | [7. Registering tools](07-tools.md) |
| Have a person approve dangerous operations. | [8. Approval](08-approval.md) |
| Turn off or replace one of the default extensions. | [13.4 std](13-extensions.md#134-installing-the-default-extensions-with-std) |

---

[← Previous: Introduction](01-overview.md) · [Contents](README.md) · [Next: Core objects →](03-core-objects.md)
