# Changelog

Every change to a schema, data file, prompt or expected fixture is recorded here; the release notes carry the detail.

## Unreleased

An MCP server's name may now hold capitals and underscores: `^[A-Za-z0-9_-]{1,64}$` in place of the old lower-case form, which lets a host keep the names of its own configuration, as Claude Code's do. The case is kept in tool names, and the command a prompt becomes lower-cases the server's part as it already lower-cased the prompt's. A name holding `__` makes a prefix rule such as `mcp__<server>__*` reach another server's tools, which the host has to avoid.

`ext/mcp` now has conformance cases: `conformance/loop/ext/mcp/` holds the new `server_name_case`, `search_mode`, `error_classes`, `elicitation_accept` and `elicitation_declined` fixtures, and `spec/ext/mcp/conformance.md` describes the case options, the servers a binding serves in process for them and how a scripted tool or prompt answers. No case loaded the extension before, so its tool names, prompt commands, catalog text, discovery mode and failure classes went uncompared across bindings. The elicitation cases answer a server's request through `mcp_elicitation`, once with a hook and once declined without one; the case servers are now served statefully over streams, which a server needs to send that request at all.

Two kernel cases close points that had none: `post_tool_batch_stop` fires the `post_tool_batch` hook, the one kernel point a case never reached, and `tool_abandoned` covers the `tool.abandoned` event a call still running after the cancel grace takes. `command_output` covers the `command.output` bulk event, which no case emitted before: a scripted command's new `outputs` field emits each text in order before its `command.completed`. `session_end_hooks` adds the other kernel point a case never reached: an async `session_end` hook sees the close reason and final usage, the close waits for it, and the reminder its injection queued is dropped as the session closes.

Two case shapes close output gaps: `provider_raw` turns on the new `agent.raw_events` field and compares every `provider.raw` bulk event with the wire frame it carries; `model_listing` gives a provider a `models` listing file, which the binding answers from `GET {base}/models`, so the case runs §9.5 end to end — the listing's window reaches the session and `usage.updated` reports it.

`ext/skills` gains four cases beside `skill_pin_survives_compaction`: `skill_load_and_resources` loads a skill and its resource list, loads it again, reads one resource and is refused an invalid path and a file it does not have; `skill_hidden_and_unknown` keeps a skill with `disable-model-invocation` out of the model's reach, and a name no skill has is answered with the skills that can be loaded; `skill_command` runs `/release-notes`, a command the extension registers when the session starts, for a skill only a person may invoke, so its body expands with the arguments and the skill is pinned in memory; `skill_activate_denied` refuses a load through the extension's own `skill_activate` point, once for the model's tool call and once for a person's command, with neither a pin nor `skills.activated`.

The conformance runner now takes the hook points an extension declares, so a case under `loop/ext/<x>/` may script them by name like a kernel point; `conformance/README.md` states it. `ext/peer` and `ext/subagent` pass their points as `ext/skills` does: `a2a_auth_retry` answers a peer's 401 through `peer_auth_required`, which retries the same message so the second round answers, and `subagent_start_denied` refuses a subagent through `subagent_start`, failing the attempt with the hook's reason and opening no child session. `subagent_stop` takes no loop case: its events are published while the child session's events reach the parent bus, an order O4 leaves free, so a fixture would compare one interleaving of many.

The kernel publishes every event a sync hook caused through the session before that hook's `hook.completed`, an order `runtime.md` §10.2 now fixes. A command registered in a `session_start` hook exposed the gap: its `registry.changed` was announced through the actor while the end event came from the hook's own goroutine, so the two could arrive either way, and the case failed about one run in five.

## 0.3 (2026-10-28)

A MINOR release: a session may set and change its context window at run time, rewind and fork may cut at an input id, and the `subagent` start tool takes a model and an effort per call. See [v0.3.0 release notes](../document/releases/v0.3.0.md) (in Chinese).

## 0.2 (2026-10-26)

A MINOR release: `model.anomaly`, optional `scope` on matched rules, `notice.kind`/`detail`, `registry.changed` for runtime rules, and binding-native rule predicates. Rule ids are now unique across the scopes a session sees, and an inherited rule cannot be hidden. See [v0.2.0 release notes](../document/releases/v0.2.0.md) (in Chinese).

## 0.1.0 (2026-10-21)

The first version of the spec: runtime semantics, schemas, data, prompts, mappings, extensions and conformance. See [v0.1.0 release notes](../document/releases/v0.1.0.md) (in Chinese).
