# ext/skills conformance

Cases live in `conformance/loop/ext/skills/`. A case names the extension in `agent.extensions` as `"skills": options`:

| Field | Meaning |
|---|---|
| `files` | Paths to file contents. The extension's first source is a file-system source (`README.md`) over an in-memory file system holding exactly those files, its root the file system's root |
| `host` | Skills a host source lists, name → `{description, body, user_invocable, model_invocable}` (`user_invocable` and `model_invocable` default true). The extension's second source lists exactly these, as a host's own `Source` does; it has no resources. Absent means no host source |
| `rewrite` | The scripted rewriter. `replace` lists `{old, new}` pairs applied in order; `sequence` lists the bodies returned in order, the last repeating; `error` makes it fail with that text. It sees the body after `$ARGUMENTS` expansion. Absent means no rewriter |

Everything else takes the extension's defaults.
