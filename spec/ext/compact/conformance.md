# ext/compact conformance

Cases live in `conformance/loop/ext/compact/`. A case names the extension in `agent.extensions` as `"compact": options`; an absent field takes the extension's default (`README.md`). The options:

| Field | Meaning |
|---|---|
| `drop_current_turn` | Keep no line of the running turn |
| `summary_prompt` | The caller's template, replacing `summary.md` |
| `prune` | Turn on clearing old tool results: `keep_recent` (0 or absent is the default 5, a negative value keeps none) and `min_tokens` (0 or absent is the default 1000) |
