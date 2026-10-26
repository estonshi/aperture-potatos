# Agent runtime specification

This directory is the language-neutral contract of the agent runtime. Every binding (Go is the reference) reads these files as data and must produce the same event sequences, history, request bodies and session file lines. Order of authority: this directory, then the Go reference implementation, then other bindings.

The key words MUST, MUST NOT, SHOULD and MAY are used as in RFC 2119.

## Layout

| Path | Contents |
|---|---|
| `VERSION` | The spec version (SemVer). It is written into `session.started.spec_version` and into `boundary.session.spec_version` on the first line of every session file. |
| `CHANGELOG.md` | Every change to a schema, data file, prompt or expected fixture MUST have an entry. |
| `runtime.md` | Normative runtime semantics: invariants, ordering and delivery guarantees (§1–2); messages and the session file (§3); sessions, turns, tools, the model call ledger, hosted tools and labels (§4); the input queue (§5); commands (§6); the system prompt skeleton and its template language (§7); the fixed-text table (§8); models and capabilities (§9); hooks (§10); approval (§11); context management: compaction, pins, clear, rewind, forks, side chats and history search (§12); and tasks (§13). |
| `schema/` | JSON Schema 2020-12 definitions, with `examples/valid` and `examples/invalid` for each schema. |
| `data/` | Protocol data every binding reads instead of hard-coding it: error classification (`errors.json`), stop reason mapping (`stop_reasons.json`), defaults (`limits.json`, hook defaults included) and the runtime tags that foreign text must not forge (`runtime_tags.json`). |
| `prompts/` | Text the runtime sends to the model: the fixed system sections (`system.core.md`, `identity.md`, `user.md`), the prefix of a message from another agent (`agent_message.md`), the task texts (`task_started.md`, `task_notification.md`), and `reminders.md`, which holds fixed texts, one per level-two section. |
| `mappings/` | Normative request and stream mappings per API, and the render-time projection shared by all of them. |
| `ext/` | The texts, schemas and options of the built-in extensions, one directory each (`ext/README.md`). |
| `embed.go` | Not part of the spec: it lets the Go binding embed these files. |

An example at `schema/examples/<valid|invalid>/<schema>/<target>.<case>.json` is checked against `schema/<schema>.schema.json`, at `$defs/<target>`, or the whole schema when the target is `root`. A `.jsonl` example is checked line by line. Valid examples MUST pass and decode and re-encode unchanged in every binding. Invalid examples MUST be rejected. An extension keeps its schemas and their examples the same way under `ext/<extension>/` (see `ext/README.md`).

## Implementing a binding

A binding is an implementation of this spec in one language. It reads the files here as data rather than copying their contents into code, and proves itself against the fixtures in [`../conformance/`](../conformance/README.md):

1. Read `runtime.md` §1–3 and the wire conventions below; they hold for everything else.
2. Layer 1, pure functions: the template language (`conformance/prompt/`), error classification (`conformance/errors/`), then for each API its render and decode cases (`conformance/render/`, `conformance/provider/`), following `mappings/`.
3. Session files: the reader cases under `conformance/store/`.
4. Layer 2, the loop: `conformance/loop/` and the resume cases under `conformance/store/`, which script a model and compare events, session file lines and requests.
5. Extensions and A2A: `conformance/loop/ext/` and `conformance/a2a/`, for the extensions the binding ships.

The binding's runner writes each case's outputs as [`conformance/runner-protocol.md`](../conformance/runner-protocol.md) describes, marking a case it does not implement yet with `actual.skip`, and `conform diff` decides whether they match. `conformance/README.md` describes each kind of case and the comparison rules.

## Wire conventions

| Item | Convention |
|---|---|
| Field names | snake_case ASCII, `^[a-z][a-z0-9_]*$`. |
| Discriminator | `type`. |
| Event types | `domain.action`, lowercase snake case. Extension events use the extension name as the domain. Third-party extensions SHOULD use `x_<org>.<name>`. |
| Block types and enum values | Lowercase snake case strings, never integers. |
| Ids | Opaque strings. Ids the runtime mints carry a prefix: `ses_`, `turn_`, `msg_`, `in_`, `cmd_`, `op_` (a queued `Compact`, `Clear` or `Rewind` call), `task_`, `ins_` (a task instruction), `hook_`, `apr_`, `batch_` (an approval batch), `tcall_` (the call id of each run of a `tool_exec` task), `b_` (a session file write batch). Provider ids are kept verbatim. |
| Times and durations | Times are RFC 3339 in UTC. Durations are integer milliseconds in fields ending in `_ms`. A tool timeout of "never" is written `timeout_ms: -1`, and any negative value reads back as "never". |
| Token counts | Integers, in fields ending in `_tokens`. |
| Binary data | Standard base64, next to a `media_type` field. |
| Optional fields | Absent, null and the default value mean the same thing. Producers omit absent fields and MUST NOT write null. |
| Numbers | Integers stay within 2^53. |
| Opaque data | `native.raw` is kept semantically verbatim: no field is dropped and no value changes, though key order may. The only allowed transformations are the stripping rules this spec lists. |
| Array order | Order is significant unless the spec declares a set: tool lists, tool results, reasoning items and hook order are ordered. |

## Versioning

The spec follows SemVer but is numbered `MAJOR.MINOR` only: there is no PATCH release. A wording fix or a data correction no binding can observe ships with the next SDK patch and does not change the spec version. Events carry no version of their own. Session file lines carry a line format version `v` (currently 1).

An SDK release declares the spec version it satisfies as `vMAJOR.MINOR.PATCH`, where `vMAJOR.MINOR.y` satisfies spec `MAJOR.MINOR`; a binding supports one `0.MINOR` at a time. Before 1.0, a MINOR version may also make the changes listed under MAJOR; from 1.0 the rules below hold as written.

- **MINOR** (backward compatible): new event types, optional fields, enum values, hook points, built-in tools or commands, and error classification rules.
- **MAJOR**: removing or renaming a field, event or enum value; making an optional field required; changing existing semantics or default behavior, such as merge rules or the reminder format.

Text the model sees (`prompts/`, the texts under `ext/`, tool definitions) is behavior: changing it is MINOR, since it changes what a model does without breaking a consumer, a typo fix included.

Consumers MUST:

1. ignore unknown event types;
2. ignore unknown fields;
3. map unknown enum values to a fallback (`other`, `unknown`) and keep the original value;
4. keep unknown block types as opaque blocks without failing;
5. ignore unknown fields in hook output, and log them;
6. read session files by the rules in `runtime.md`.

Producers MUST NOT write fields, enum values or types that are absent from the spec version they declare.

Deprecated items are marked `deprecated: true` in the schema. They stay for at least one MINOR version and are removed in the next MAJOR. Each binding declares the range of spec versions it supports in its own documentation, as `MAJOR.MINOR` before 1.0 and `MAJOR.x` with a lowest MINOR after, together with the SDK releases that satisfy it; its conformance runner skips the cases of any other version.
