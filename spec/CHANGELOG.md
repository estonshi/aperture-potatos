# Changelog

Every change to a schema, data file, prompt or expected fixture is recorded here.

## 0.1.0 (2026-10-21)

The first version of the spec: the runtime semantics, the schemas and data every binding reads, the texts the model sees, the mappings for three model APIs, the built-in extensions, and the conformance cases that check a binding against all of them.

### Schemas

JSON Schema 2020-12 under `schema/`, each with valid and invalid examples.

- `common.schema.json`: ids, times, usage (`hosted_calls` included), model identity, event hops, the session `label_record`, and the model call ledger (`model_call`, `turn_calls`, `turn_cost`, `call_cost`, `cost`).
- `message.schema.json`: one committed message, which is also one session file line. Blocks `text`, `refusal`, `image`, `document`, `reasoning`, `tool_call`, `tool_result`, `reminder`, `hosted_call` and `hosted_result`, with opaque blocks for anything else; epoch boundaries with reasons `new`, `clear`, `compaction`, `switch`, `rewind`, `fork`, `side_chat` and `dropped`; tool definitions, hosted tools, and the storage-only `batch` and `batch_end` fields. `tool_call.input_error` is one of `invalid_json`, `not_object`, `incomplete`.
- `event.schema.json`: the event envelope and the 57 kernel payloads.
- `provider.schema.json`: the neutral request, stream events, errors, notices and capabilities that layer-1 conformance compares.
- `hook.schema.json`: the input and output of each of the 17 kernel hook points, the hook environment and the async injection.
- `tool.schema.json`: `tool_spec`, `tool_result` and `pin`. `command.schema.json`: `command_result`, `input`, `compact_report` and `usage_report`.
- `task.schema.json`: task spec, instruction, progress, result, status and info.
- `approval_rule.schema.json`: approval rules and their conditions; among matching rules `deny` wins.

### Runtime semantics

`runtime.md` is normative for every binding:

- §1 Invariants I1–I10.
- §2 Events: ordering O1–O5 and delivery guarantees G1–G6.
- §3 Messages and the session file: the message rules, file rules RULE-STORE-001 to RULE-STORE-011 (one append-only `<session_id>.jsonl` per session, epochs and write batches, an optional calls file and label record), and resume.
- §4 Sessions and turns: session phases, the turn loop and its outcomes, attempts and retries, tool groups after the gate and interruption, the model call ledger (one call per HTTP request), hosted tools with deferred tools and the neutral tool search, and session labels.
- §5 The input queue: items, barriers, who starts work when idle, safe points, capacity and merging, delivery, runtime tags, and withdrawing a queued input. A tag attribute value (a reminder's `source`, an agent message's `from`) is written as a JSON string by RFC 8785 §3.2.2.2.
- §6 Commands: parsing, the busy policies `run_now`, `wait_idle`, `interrupt` and `reject`, and prompt-command placeholders.
- §7 System prompt skeleton: sections in slot order, the mustache subset templates are written in, and `system_hash`.
- §8 Fixed texts: the table of every text in `prompts/reminders.md`, where it goes and its variables.
- §9 Models and capabilities: capability sources and globs, the output cap, the recoveries that allow one changed retry (with `capability_hint` fields `thinking_prefix_bound`, `max_output_tokens` and `max_tokens_field`), switching models, and the model listing field table.
- §10 Hooks: the 17 kernel points with their key, timeout and failure policy, running a point, merging, matchers and the portable expression subset, where outputs go, async hooks, and context providers.
- §11 Approval: input normalization, rules, the gate's decision, and batch review with rewrites and `deny_and_stop`.
- §12 Context management: the size estimate and threshold, when a session compacts (automatically, on overflow, before a switch, manually), one compaction, pins, partial compaction, clear, rewind, forks and side chats, history search, and the context breakdown.
- §13 Tasks: kinds and the manager, starting, ending, instructions and questions, restart, waiting and the start tool, notifications, persistence and resume, background tools and `tool_exec`, and derived agents with their child sessions.

### Data

- `data/errors.json`: ordered classification rules for `anthropic.messages`, `openai.chat` and `openai.responses`, the first match winning, with the conditions `status`, `status_range`, `type`, `code`, `code_match`, `message`, `header_absent` and `mid_stream`, the named rules `max_output_too_large`, `max_tokens_field_unsupported`, `reasoning_summary_unavailable` and `thinking_signature_bound`, and `limit`.
- `data/stop_reasons.json`: each API's wire stop values mapped to the neutral stop reason, `other` as the fallback.
- `data/limits.json`: the `provider`, `retry`, `limits`, `tasks`, `subscription`, `context`, `hooks` and `input` defaults. A zero setting takes the default and a negative one lifts the limit, except that a negative retry `max_attempts` or `max_elapsed_ms` means no retry. Among them: `max_steps` -1, `max_parallel_tools` 8, `max_running_tasks` 8, `max_tool_result_chars` 100000, `first_event_timeout_ms` 120000 and `context.auto_compact_ratio` 0.9.
- `data/runtime_tags.json`: `system-reminder`, `task-notification`, `agent-message` and `runtime-conventions`, the tags only the runtime may open or close.

### Prompts

- `prompts/system.core.md`, `identity.md` and `user.md`: the fixed system sections. `agent_message.md`: the prefix of a message from another agent.
- `prompts/task_started.md` and `task_notification.md`: the result of a start that leaves a task running, and the notification text.
- `prompts/reminders.md`: 44 fixed texts, one per level-two section, each a template; `runtime.md` §8 lists them.

### Mappings

- `mappings/projection.md`: the render-time steps every adapter runs, among them the replay matrix by API, endpoint and family, degraded replay, healing dangling calls with `dangling_tool_call`, and tool id remapping. Reasoning from another provider never becomes visible text.
- `mappings/anthropic_messages.md`: the request body, cache breakpoints B1, B2, B2s, B3 and B4 up to the API's 4, reasoning, gating, degraded replay, stream decoding, web search and tool search as hosted blocks, and token counting with `count_tokens`. The first-event timeout runs from sending the request, waiting for the response headers included.
- `mappings/openai_chat.md`: reasoning read from `reasoning_content`, `reasoning`, `thinking_blocks` and `reasoning_details` and replayed only to the same endpoint and family; the output cap under `caps.max_tokens_field`; explicit cache marks with `caps.prompt_cache.explicit`; a fresh `call_<n>` for an empty or reused call id. Hosted tools are never sent.
- `mappings/openai_responses.md`: stateless use with `store:false`, every item sent without its id, and `include:["reasoning.encrypted_content"]` whenever `reasoning` is sent; explicit caching with `caps.prompt_cache.explicit`; web search and tool search as hosted blocks.

### Extensions

`ext/README.md` sets the shared rules: texts are templates in the language of `runtime.md` §7, events and hook points have their own schemas, and `conformance.md` lists each extension's options. The command extensions `cmd.clear`, `cmd.compact`, `cmd.context`, `cmd.fork`, `cmd.help`, `cmd.rewind` and `cmd.usage` are described there and have no directory.

- `ext/compact`: the default compaction strategy, its templates (`summary.md`, `carry_forward.md`, `merge.md`, `summary_from.md`) and clearing old tool results before summarizing.
- `ext/history`: the `search_history` tool over history search and line lookup; `parent: true` searches a child session's parent and is refused in a side chat.
- `ext/mcp`: a client for MCP servers (protocol 2026-07-28): server tools as `mcp__<server>__<tool>`, the `<mcp_servers>` catalog, discovery through `mcp_search_tools` and `mcp_call_tool`, native mode with deferred tools, server prompts as slash commands, OAuth 2.1, the `mcp_elicitation` hook point and the `mcp.*` events.
- `ext/memory`: instruction files and memories tagged by `user` and `project`, written into each new epoch's head; `memory_save` and `memory_forget`; consolidation; `user_tokens` and `shared_tokens` 10000 each; the `memory.loaded` and `memory.saved` events.
- `ext/peer`: the `a2a` task kind with `a2a_send`, the `<available_agents>` catalog, in-process peers, the call chain that refuses cycles, the `peer_auth_required` hook point and event; `a2a.md`, the client for A2A v0.3 and v1.0 over JSON-RPC and HTTP+JSON, with discovery from the agent card.
- `ext/skills`: Agent Skills (`<name>/SKILL.md`) in a catalog section, the `skill` tool, `/<name>` commands, the `skill_activate` hook point, and the `skills.activated` and `skills.catalog_changed` events.
- `ext/subagent`: the `subagent` task kind and start tool, `ask_parent` in the child session, and the `subagent_start` and `subagent_stop` hook points; a child session starts no subagent of its own.
- `ext/tasks`: `task_status`, `task_send`, `task_stop` and `task_restart`, the capabilities section `guidance.md`, and the list of unfinished tasks after compaction.

### Conformance

- Layer 1, `provider/` decode cases: `anthropic.messages` 27, `openai.chat` 26, `openai.responses` 26, four of them recorded from live endpoints. `render/`: 34, 20 and 22. `errors/`: 18, 21 and 16.
- `prompt/mustache`: 23 cases, 14 templates and 9 malformed ones. `prompt/system`: 5 skeleton cases.
- `store/`: 17 cases, 8 reading session files and 9 resuming them.
- Layer 2, `loop/`: 136 kernel cases. `loop/ext/`: 64 extension cases (`compact` 11, `compactcmd` 3, `clearcmd` 1, `contextcmd` 1, `forkcmd` 5, `helpcmd` 1, `history` 4, `memory` 3, `peer` 10, `rewindcmd` 4, `skills` 1, `subagent` 14, `tasks` 5, `usagecmd` 1). Every case also asserts I1, I2, I4, I6, I9 and I10.
- `a2a/`: `v03/httpjson` 11, `v03/jsonrpc` 12, `v10/httpjson` 12 and `v10/jsonrpc` 10 client cases, and 14 `discovery` cases.
- `conformance/runner-protocol.md`: a binding's runner writes each case's outputs as `actual.*` files, or `actual.skip` or `actual.fail` with a reason, and the shared `conform diff` and `conform update` commands compare them with the expected files, or accept them, by the rules in `conformance/README.md`.
