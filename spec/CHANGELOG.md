# Changelog

Every change to a schema, data file, prompt or expected fixture is recorded here; the release notes carry the detail.

## 0.5 (2026-10-31)

A MINOR release: command names and subagent type names may carry a namespace colon (`plugin:command`, `plugin:agent`), `ext/skills` lets a host rewrite a skill's body with the session's permissions before the model sees it, rendering it again before deduplication so a repeated load sees fresh text, and the `skill` tool takes `args` to expand the body's prompt template. See [v0.5.0 release notes](../document/releases/v0.5.0.md) (in Chinese).

## 0.4 (2026-10-29)

A MINOR release: an MCP server's name keeps its case, a sync hook's own events are published before its `hook.completed` (now fixed in `runtime.md` §10.2), and the conformance suite grows cases for extension hook points, `command.output`, `provider.raw`, model listings, `session_end` and the kernel points that had none. See [v0.4.0 release notes](../document/releases/v0.4.0.md) (in Chinese).

## 0.3 (2026-10-28)

A MINOR release: a session may set and change its context window at run time, rewind and fork may cut at an input id, and the `subagent` start tool takes a model and an effort per call. See [v0.3.0 release notes](../document/releases/v0.3.0.md) (in Chinese).

## 0.2 (2026-10-26)

A MINOR release: `model.anomaly`, optional `scope` on matched rules, `notice.kind`/`detail`, `registry.changed` for runtime rules, and binding-native rule predicates. Rule ids are now unique across the scopes a session sees, and an inherited rule cannot be hidden. See [v0.2.0 release notes](../document/releases/v0.2.0.md) (in Chinese).

## 0.1.0 (2026-10-21)

The first version of the spec: runtime semantics, schemas, data, prompts, mappings, extensions and conformance. See [v0.1.0 release notes](../document/releases/v0.1.0.md) (in Chinese).
