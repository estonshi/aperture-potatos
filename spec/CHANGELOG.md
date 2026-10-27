# Changelog

Every change to a schema, data file, prompt or expected fixture is recorded here; the release notes carry the detail.

## Unreleased

A session's window may now be set and changed at run time. `ContextPolicy.MaxContextTokens` names the window itself, overriding the one the model reports, and `SetContextWindow` / `SessionContextWindow` set it per session. A line a session writes records it in the new optional `message.context_window`, and a resume restores the last value an original line recorded. `window_tokens` in `usage.updated` now reports that effective window, so the expected events carrying it were regenerated. The `subagent` extension adds a `model` and an `effort` to its start tool: `models`, `efforts` and `resolve_model` options, `fixed_model` on a type, `model`/`effort` on `subagent_start` input and output, and the `subagent_call_time_model` case.

## 0.2 (2026-10-26)

A MINOR release: `model.anomaly`, optional `scope` on matched rules, `notice.kind`/`detail`, `registry.changed` for runtime rules, and binding-native rule predicates. Rule ids are now unique across the scopes a session sees, and an inherited rule cannot be hidden. See [v0.2.0 release notes](../document/releases/v0.2.0.md) (in Chinese).

## 0.1.0 (2026-10-21)

The first version of the spec: runtime semantics, schemas, data, prompts, mappings, extensions and conformance. See [v0.1.0 release notes](../document/releases/v0.1.0.md) (in Chinese).
