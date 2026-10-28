# Changelog

Every change to a schema, data file, prompt or expected fixture is recorded here; the release notes carry the detail.

## Unreleased

An MCP server's name may now hold capitals and underscores: `^[A-Za-z0-9_-]{1,64}$` in place of the old lower-case form, which lets a host keep the names of its own configuration, as Claude Code's do. The case is kept in tool names, and the command a prompt becomes lower-cases the server's part as it already lower-cased the prompt's. A name holding `__` makes a prefix rule such as `mcp__<server>__*` reach another server's tools, which the host has to avoid. No schema, data, prompt or fixture changed.

## 0.3 (2026-10-28)

A MINOR release: a session may set and change its context window at run time, rewind and fork may cut at an input id, and the `subagent` start tool takes a model and an effort per call. See [v0.3.0 release notes](../document/releases/v0.3.0.md) (in Chinese).

## 0.2 (2026-10-26)

A MINOR release: `model.anomaly`, optional `scope` on matched rules, `notice.kind`/`detail`, `registry.changed` for runtime rules, and binding-native rule predicates. Rule ids are now unique across the scopes a session sees, and an inherited rule cannot be hidden. See [v0.2.0 release notes](../document/releases/v0.2.0.md) (in Chinese).

## 0.1.0 (2026-10-21)

The first version of the spec: runtime semantics, schemas, data, prompts, mappings, extensions and conformance. See [v0.1.0 release notes](../document/releases/v0.1.0.md) (in Chinese).
