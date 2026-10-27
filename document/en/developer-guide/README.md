# potatOS Developer Guide

potatOS is an agent core SDK written in Go. You use it to build agents inside your own program that can call tools and work over multiple turns; Chapter 1 introduces it in full. This guide is for developers who use potatOS. It explains how to integrate, configure and extend it, and the constraints you need to follow when you use it. The guide covers only how to use the interfaces, not how they are implemented. If you want to learn how it works internally first, read [architecture.html](../architecture.html) in the parent directory.

> Note: This guide covers the module `github.com/estonshi/aperture-potatos`, which requires Go 1.25. Its content matches spec version 0.3.

## How to read this guide

If you are integrating potatOS for the first time, read Chapters 1, 2 and 3 in order, get the quick-start example running, and then jump to later chapters as you need them. If your agent is already running, you can go straight to the relevant content through "Find a chapter by task" below.

Each chapter opens with a short introduction that explains what problem the chapter solves and when you need to read it. Every chapter except Frequently asked questions lists its contents before the main text. Most chapters end with a "Best practices" section that collects recommended patterns learned from real use.

## Contents

| Chapter | Description |
|---|---|
| [1. Introduction](01-overview.md) | What potatOS is, its use cases, how work is divided between the SDK and your program, installation, and the most common packages. |
| [2. Quick start](02-quickstart.md) | A minimal agent with a tool, approval and streaming output. |
| [3. Core objects](03-core-objects.md) | Agent, Session, Receipt and events, plus terminology, a Session method reference and lifecycle rules. |
| [4. Connecting models](04-models.md) | The three adapters, parameters, model capabilities, reasoning, switching models at runtime, server-side tools and gateways. |
| [5. Sending input and waiting for results](05-input.md) | Send, Run and Command, the three modes while the agent is busy, interrupting, withdrawing a queued input or sending it now, and TurnResult. |
| [6. Subscribing to events](06-events.md) | How to subscribe, backpressure, which session an event belongs to, the event reference, UI rendering rules and passing events across processes. |
| [7. Registering tools](07-tools.md) | Defining tools, concurrency, timeouts, background execution, dynamic definitions, and how to write a good tool. |
| [8. Approval (human in the loop)](08-approval.md) | The approval rules file, the approver interface, related events, and how to integrate approval with a terminal or a web UI. |
| [9. Hooks](09-hooks.md) | The 17 hook points, synchronous and asynchronous execution, what each point can change, and common uses. |
| [10. Commands](10-commands.md) | Template commands, custom commands, busy policies and built-in commands. |
| [11. System prompt and dynamic context](11-prompt.md) | The prompt skeleton, your instructions, extension sections and content that changes every turn. |
| [12. Tasks](12-tasks.md) | The protocol for background work, including task kinds, the runtime, the manager and notifications. |
| [13. Extensions](13-extensions.md) | Extension, Registrar, scopes, std and derived agents. |
| [14. Built-in extensions](14-builtin-extensions.md) | compact, history, tasks, subagent, skills, memory, peer, mcp, slash commands and litellm. |
| [15. Context management](15-context.md) | Automatic compaction, manual compaction, clearing, rewind, forks, side chats, history search and Pin. |
| [16. Storage and resume](16-storage.md) | In-memory and JSONL storage, plugging in your own backend, resuming sessions, and changing session labels and querying sessions by label. |
| [17. Usage, call records and cost](17-usage-cost.md) | Usage for each step, a record of every request, tagging requests and querying cost. |
| [18. Limits and defaults](18-limits.md) | Every limit and timeout you can adjust. |
| [19. Calling rules in callbacks](19-callback-rules.md) | The rules for calling Session methods from tools, hooks, commands and tasks. |
| [20. Errors](20-errors.md) | Sentinel errors, structured errors, and how model call errors are classified. |
| [21. Testing](21-testing.md) | How to test an agent without connecting to a real model, and how to replay recorded HTTP responses. |
| [22. Frequently asked questions](22-faq.md) | The questions that come up most often during integration. |

## Find a chapter by task

| Task | See |
|---|---|
| Connect the agent to your own CLI or web service and stream output to users. | [5](05-input.md), [6](06-events.md) |
| Withdraw a message the user still has in the queue, or have it handled right away. | [5.7](05-input.md#57-withdrawing-a-queued-input-or-sending-it-now) |
| Let the model call your functions or APIs. | [7](07-tools.md) |
| Have a person confirm before a dangerous operation runs. | [8](08-approval.md) |
| Audit, rewrite or block a step the agent takes. | [9](09-hooks.md) |
| Give users shortcut commands of the form `/xxx`. | [10](10-commands.md) |
| Put the tenant, the project, the current time or similar information into the prompt. | [11](11-prompt.md) |
| Keep work that takes several minutes from blocking the conversation. | [12](12-tasks.md), [7.6](07-tools.md#76-moving-a-call-to-the-background) |
| Use subagents, MCP servers, skills or long-term memory, or call other agents. | [14](14-builtin-extensions.md) |
| Keep the context under control when a conversation gets too long. | [15](15-context.md) |
| Continue an earlier conversation after the process restarts. | [16](16-storage.md) |
| Change session labels, or list sessions by labels such as user or project. | [16.5](16-storage.md#165-session-labels) |
| Work out what each turn costs and reconcile it with the gateway logs. | [17](17-usage-cost.md) |
| Switch models, connect through a gateway, or connect to an OpenAI-compatible service. | [4](04-models.md) |
| Find out why the agent stopped or why it is waiting so long. | [18](18-limits.md), [22](22-faq.md) |
| Fix a tool or hook that blocks when it calls a Session method. | [19](19-callback-rules.md) |
| Write tests without connecting to a real model. | [21](21-testing.md) |

## Conventions

- In the code examples, `kernel`, `provider`, `event`, `message` and `store` are all packages in this module. Their import paths are listed in [1.5](01-overview.md#15-common-packages).
- The terms turn, step and epoch have the specific meanings defined in [3.2](03-core-objects.md#32-terminology).
- In tables, "default" means the value used when you do not set anything. For fields marked "negative means unlimited", setting a negative value removes that limit.
