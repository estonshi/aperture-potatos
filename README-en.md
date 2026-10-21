# potatOS

[中文](README.md) · English

potatOS is an agent core SDK written in Go, and the smallest module of the Aperture series of agents. The series is named after Aperture Science from the Portal games, and potatOS takes its name from PotatOS, the form GLaDOS is reduced to in Portal 2 when her core is wired to a potato battery: only the essential part is left, and it still runs on a single potato.

You use potatOS to build agents, programs driven by a large language model that call tools on their own to get work done, inside your own application.

The SDK does three things itself: it calls the model, runs the agent loop and manages the context. Everything else is registered by you, including tools, background tasks, slash commands, hooks, approval rules, skills, long-term memory, other agents the model may call, and storage. Everything that happens during a run is emitted as an event, so your program can show it to users or record it. potatOS is a library embedded in your program, not a standalone service, and it has no user interface.

## Features

- Three model APIs: Anthropic Messages, OpenAI Chat Completions and OpenAI Responses. The Chat Completions adapter also works with DeepSeek, Qwen, vLLM, Ollama, OpenRouter, LiteLLM and other compatible services.
- An agent loop with an input queue, parallel tool execution, retries, interrupts and an event stream.
- Context management with a fixed-structure system prompt, automatic compaction, rewind, forks, side chats and history search.
- Hooks, approval rules and approvers, and a protocol for background tasks.
- Built-in extensions for subagents, skills, memory, A2A and MCP clients, slash commands and cost lookup.
- JSONL storage, session resume and session labels.
- No dependencies beyond the Go standard library, except the MCP client `ext/mcp`, which uses the official MCP Go SDK together with `golang.org/x/oauth2` and `jsonschema-go`. potatOS uses no model vendor SDK.

## Installation

```bash
go get github.com/estonshi/aperture-potatos
```

potatOS requires Go 1.25 or later.

## Example

The program below registers one tool and prints the model's answer as it streams in.

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
)

type weatherIn struct {
	City string `json:"city"`
}

func main() {
	ctx := context.Background()
	claude := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY")})

	// A tool the model can call; its input is decoded into weatherIn.
	weather := kernel.NewTool(kernel.ToolSpec{
		Name:        "get_weather",
		Description: "Current weather of a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}, func(ctx context.Context, tc *kernel.ToolContext, in weatherIn) (kernel.ToolResult, error) {
		return kernel.TextResult(in.City + ": sunny, 22°C"), nil
	})

	ag, err := kernel.New(
		std.Defaults(), // compaction, history search, task tools, subagents and the built-in commands
		kernel.WithModel(provider.ModelRef{Provider: claude, Model: "claude-sonnet-5-5"}),
		kernel.WithTool(weather),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer ag.Close(ctx)

	sess, err := ag.NewSession(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer sess.Close(ctx)

	// Run sends one input and yields every event of the turn until turn.completed.
	for ev, err := range sess.Run(ctx, kernel.Text("What's the weather in Tokyo?")) {
		if err != nil {
			log.Fatal(err)
		}
		switch d := ev.Data.(type) {
		case *event.MessageDelta:
			fmt.Print(d.Text)
		case *event.TurnCompleted:
			fmt.Printf("\n[turn %s, %d steps]\n", d.Outcome, d.Steps)
		}
	}
}
```

A longer example with approval and storage is in the [Quick start](document/en/developer-guide/02-quickstart.md).

## Documentation

- [Developer guide](document/en/developer-guide/README.md): every feature, with examples and best practices, starting from the [Introduction](document/en/developer-guide/01-overview.md).
- [Architecture walkthrough](document/en/architecture.html): how the SDK is put together inside.
- [Specification](spec/README.md): the language-neutral specification of events, the session file format, model-facing texts and defaults, with conformance fixtures in [conformance](conformance/README.md). The Go implementation supports spec 0.1.x, as described in [Supported spec versions](document/en/developer-guide/01-overview.md#supported-spec-versions).

## Repository layout

| Directory | Contents |
|---|---|
| `kernel` | Agent, Session and the whole public API: tools, hooks, tasks, commands and context management. |
| `provider` | Model references, parameters, capabilities, retries and the three API adapters. |
| `ext`, `std` | The extensions, and the one-line installer for the default set. |
| `event`, `message`, `store` | Events, messages, and the storage interfaces with their memory and JSONL implementations. |
| `spec`, `conformance` | The language-neutral specification and its conformance fixtures. |
| `document` | The developer guide and the architecture walkthrough, in Chinese and in English (`document/en`). |

## License

potatOS is released under the [MIT License](LICENSE), which also covers `spec/` and `conformance/`.
