# ext/subagent conformance

Cases live in `conformance/loop/ext/subagent/`. A case names the extension in `agent.extensions` as `"subagent": options`; an absent field takes the extension's default (`README.md`). The options:

| Field | Meaning |
|---|---|
| `defs` | The agent types, in order: `name`, `description`, `instructions`, `tools` (absent gives every tool of the parent, an empty list none), `deny_tools`, `model` (`{provider, model}`, a case provider and the model it runs), `fixed_model`, `max_steps` (0 or absent is 50) and `ask_timeout_ms` (0 or absent waits until an answer arrives). A type without `model` uses the parent session's current model |
| `max_concurrent` | Subagents of one session running at once; 0 or absent is 8 |
| `models` | Model names the start tool offers in its enum; empty or absent exposes no model choice. `inherit` runs on the parent session's model |
| `efforts` | Reasoning effort names the start tool offers in its enum; empty or absent exposes no effort choice |
| `resolve_model` | Maps a name in `models` to a case provider and its model: `{"small": {"provider": "claude", "model": "claude-small"}}`. A name outside the map fails the attempt |

Another extension a case names beside this one is built with its default options, and a case naming one the binding does not have is skipped; `subagent_search_history_*` and `subagent_search_parent_current_context_only` name `"history": {}` this way.
