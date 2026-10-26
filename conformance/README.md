# Conformance

Language-neutral fixtures that every binding runs. A binding runs them with a runner of its own and compares its outputs with the shared `conform` command ([runner-protocol.md](runner-protocol.md)). The Go reference generates the expected files with `go test ./conformance/... ./ext/... -update`: the loop/ext and A2A cases are driven by the tests of their extensions. A person reviews every change against `spec/runtime.md` before it is committed, and each change needs an entry in `spec/CHANGELOG.md`.

## Layer 1: provider (pure functions)

| Path | Input | Expected |
|---|---|---|
| `provider/<api>/<case>/` | `meta.json`, `response.sse` (the raw body), optional `headers.json` (`{status, headers}`), optional `request.json` (a request per `schema/provider.schema.json#/$defs/request`; when absent: the meta model and one user message "hi") | `expected.stream.jsonl`: one stream event per line; a stream that fails ends with `{"error": …}`. `expected.message.json`: the final message, when the stream ends normally |
| `render/<api>/<case>/` | `input.json`: a request | `expected.body.json`: the body sent. `expected.headers.json`: the compared request headers. `expected.notices.jsonl`: notices raised while rendering. `expected.error.json`: the local error, when rendering refuses to send |
| `errors/<api>/<case>.json` | `status`, `headers`, `body` (a JSON document, or a JSON string holding a non-JSON body) | `expected`: the classified error |
| `prompt/mustache/<case>/` | `case.json`: `template` and `vars` (name → string), rendered by the template language of `spec/runtime.md` §7 | `expected.json`: `{"text": …}`, or `{"error": true}` when the template is malformed (the error's wording is not specified) |
| `prompt/system/<case>/` | `vars.json`: `nickname`, `description`, `instructions`, and `sections` (`slot`, `name`, `order`, `text`), each already rendered | `expected.system.json`: `blocks`, the system blocks in order with their `slot`, `name` and `text`, and `system_hash` (`spec/runtime.md` §7). Cache marks are not part of it |

Prompt cases compare strings byte for byte.

## Layer 2: loop (the kernel)

Each case in `loop/<case>/` declares an agent, a scripted model and a list of actions; the runner builds them with the binding's own API, runs the actions, and compares what came out. No extension is loaded; cases for an extension live in `loop/ext/<x>/` and are driven by that extension's tests, which build the extensions `agent.extensions` names from their options (`spec/ext/README.md`).

`script.json`:

| Field | Meaning |
|---|---|
| `spec_version` | The spec the case is written for, as `MAJOR.MINOR`. Before 1.0 a binding runs a case only for its own `MAJOR.MINOR`; from 1.0 also for an older minor of its major. A case it does not run is skipped (runner-protocol.md) |
| `description` | What the case checks, for readers; runners ignore it |
| `agent` | `nickname`, `description`, `instructions`, `input_mode`, `auto_continue_truncated_text`, `store_policy`, `limits` (the fields of `data/limits.json`'s `limits`, durations in `_ms`), `retry` (`max_attempts`, `base_delay_ms`, `max_delay_ms`), `tools`, `hosted_tools` (`message.schema.json#/$defs/hosted_tool`), `commands`, `capabilities`: declarations `{api, model_glob, caps}` in order (`runtime.md` §9.1), `hooks`, `approval_rules` (a rule file), `sections` and `context_providers`, `request_headers` (header name → the request field it carries: `root_session_id`, `session_id`, `turn_id`, `bucket`, `api` or `model`), `capture_headers` (response headers kept on each model call), `cost_source` (below), `context_policy` (the fields of `data/limits.json`'s `context`, plus `max_context_tokens` and `disabled`), `compactor` (below), `task_kinds` (below) and `extensions` (name → options, for a case under `loop/ext/<x>/`, built by that extension's test; a case naming an extension the module does not have is skipped) |
| `session` | `input_mode`, `instructions`, `labels` of the one session the case opens, and `tools`, `hooks` and `approval_rules` registered on that session before its first epoch |
| `providers` | The models, the first one the agent's: `name`, `api`, `model`, `params` (a request's params), `responses`, one per request to that model in order, and what the scripted resolver answers for it, `resolved` (capabilities; absent means all unknown) or `resolve_error`. The runner resolves every model through this scripted resolver only, so no response is spent on a models request. Every provider of a case answers with SSE, or every one with events |
| `clock` | `start` and `step_ms`: every timestamp the runtime takes is `start` plus `step_ms` per earlier timestamp; `interleaved: true` marks a case whose tasks or async hooks take timestamps while the loop does, so its timestamps are not compared (below) |
| `ids` | `sequential`: ids number from 1 per prefix, `ses_01`, `turn_01`, `msg_01`, `in_01`, `cmd_01`, `b_01` |
| `resume` | `session_id` and `file`: the store starts with that session file and the case resumes the session instead of creating one; `calls` names a calls file the store also starts with (`runtime.md` RULE-STORE-010), and `labels` a label record (`common.schema.json#/$defs/label_record`, RULE-STORE-011) |
| `expect_calls` | Compare the session's calls file with `expected.calls.jsonl` |
| `actions` | Run in order, each once the runtime has nothing left to do without time passing |

A response is either `{"sse": path, "status", "headers"}`, replayed through the binding's real adapter (a bare path is shorthand), or `{"events": path}`, a file of neutral stream events (`schema/provider.schema.json#/$defs/stream_event`, or `{"error": …}` to fail the stream) fed to the kernel without an adapter. Either may set `hold_after_events`: after that many SSE events or stream events the stream holds, for `hold_ms`, or until the request is canceled when `hold_ms` is absent. Paths are relative to the case.

A tool is a tool definition plus a `behavior`: `type` `echo` (returns its input), `fixed` (returns `text`, an error result with `is_error`), `error` (fails with `text`) or `panic`; `sleep_ms` before returning, `ignore_ctx` to sleep through cancellation, `progress` texts to report first. A `strict` tool rejects a key its schema's `properties` do not name exactly, as `invalid_input_detail` with `unknown field "<key>"`. A `background` tool lets the model run it as a `tool_exec` task (`runtime.md` §13.9). A command is `name`, `aliases`, `busy`, and what it returns: `text`, and `prompt` with `$ARGUMENTS` replaced by its arguments; `sleep_ms` and `error` as for tools. A command with a `template` is a prompt command instead (`runtime.md` §6), whose `arg_names` name its positional arguments.

A rule in `approval_rules` may also carry `match`, the case-only form of the binding-native predicate (`runtime.md` §11.2): `{"type": "always"}`, `{"type": "never"}`, `{"type": "contains", "value": "…"}` (the normalized input holds that text) or `{"type": "panic"}`. The runner removes it before parsing the rule file and attaches the behavior as a native predicate, so this field never appears in a real rule file.

A hook is registered through its point's JSON form: `point`, `name`, `match` (the declarative matcher form, `runtime.md` §10.4), `priority`, `timeout_ms`, `fail` (`open` or `closed`), `async`, `wake_limit` (`{n, per_ms}`), and what each call does. `cases` are checked first: the first whose `when` paths (dot paths into the input, numbers indexing arrays) all hold the given values answers. Otherwise `calls` answer in order, the last one repeating, and without either the hook answers `{}`. A call waits `sleep_ms` (`ignore_ctx` to wait through cancellation) or, with `hold`, until its context ends, then answers `output`, fails with `error`, or panics with `panic`. An async hook's output is an injection. A case whose async hook must end after the turn that fired it gives the hook call a `sleep_ms` and waits for its `hook.completed`, since an async hook that answers at once is not ordered against that turn's events. A section is `name`, `slot`, `order`, fixed `text` and `announce_changes`; a context provider is `name`, `when` (`turn_start` or `every_step`) and `texts`, answered in order, the last one repeating.

Actions:

| Action | Effect |
|---|---|
| `{"send": {text, mode, origin, no_wake, no_command_parsing, metadata}, "as": name}` | One input |
| `{"command": {name, args}, "as": name}` | One command without text parsing |
| `{"interrupt": {reason, drop}}` | Interrupt, or interrupt and drop queued inputs |
| `{"withdraw": name}` | Withdraw the input a `send` named, still queued (`runtime.md` §5.8) |
| `{"interrupt_with": name}` | Interrupt the running turn with the queued input a `send` named (`runtime.md` §5.8) |
| `{"switch_model": {provider, model}}` | Switch to that provider's model, or to `model` on that provider |
| `{"add_tool": tool}`, `{"remove_tool": name}` | Register a tool on the session, or remove a session tool |
| `{"add_hosted_tool": hosted_tool}`, `{"remove_hosted_tool": name}` | Register a hosted tool on the session, or remove one the case added |
| `{"add_section": section}`, `{"remove_section": name}` | Register a prompt section on the session, or remove a session section |
| `{"wait": "idle"}` | Until no turn or history command runs and every command the script started has completed |
| `{"wait": {"event": type, "count": n, "where": {field: value}}}` | Until the n-th event of that type whose data has those fields |
| `{"wait": {"receipt": name}}` | Until that input or command is done |
| `{"sleep_ms": n}` | Let virtual time pass |
| `{"compact": {instructions, from, up_to}}` | Compact, optionally from or up to that turn |
| `{"clear": true}` | Clear |
| `{"unpin": key}` | Unpin |
| `{"rewind": turn_id}` | Rewind to that turn |
| `{"fork": {as, at, tools}}` | Fork the session, at that turn when `at` is set, limited to `tools` when it is set (an empty list allows none), and name the new session `as` |
| `{"side_chat": {name, as, tools}}` | Open that side chat and name it `as`; it must not be open already under another name |
| `{"drop_side_chat": name}` | Drop that side chat |
| `{"background_call": call_id}` | Move that running foreground call to the background (`runtime.md` §13.9) |
| `{"resume": session_id}` | Resume that session id again; while the case's session is open this must fail |
| `{"resume": session_id, "as": name}` | Resume that stored session, such as one a command forked, and keep it open under `name` like a fork; its events start at the resume, and the lines stored before it are left out of its invariants |
| `{"set_labels": {key: value}}` | Merge into the session's labels (`runtime.md` §4.7), an empty value removing its key |
| `{"labels": true}` | Record the session's labels as `{session_id, labels}` |
| `{"find_sessions": {key: value}}` | Record `{found: [{session_id, labels}]}`, the top-level sessions whose labels hold every pair, in id order |
| `{"ledger": {turn, cost, timeout_ms, usage}}` | Read that turn's model calls, price them within `timeout_ms` of virtual time, or with `usage` read the session's usage totals (`hook.schema.json#/$defs/usage_report`); a failure is recorded as `{"error": code}` with `turn_not_found`, `no_cost_source` or `cost_http_<status>` |
| `{"task": {"start": spec}}` | Start a task as the host, with a `task.schema.json#/$defs/task_spec` |
| `{"task": {"send": id, "text", "mode"}}`, `{"task": {"stop": id, "reason"}}`, `{"task": {"restart": id, "input"}}` | Send an instruction, stop, or restart (with `input` when given) as the host |
| `{"task": {"step": id, "do": step}}` | Hand that scripted task its next step |
| `{"task": {"get": id}}`, `{"task": {"wait": id}}`, `{"task": {"list": true}}` | Record the task's info, after waiting for it, or every task's info, in `expected.tasks.jsonl` |
| `{"wait_quiet": true}` | Until the session is quiet: idle, no command, no unfinished task, no notification in `task_notify` |
| `{"close": true}` | Close the session; the runner closes it after the last action otherwise |

Any action may set `"error": text`: the action must then fail with an error whose message contains the text. Any action may set `"on": as` to run on a session that a `fork` or `side_chat` action named instead of the case's session. An action with `"async": true` runs on its own thread, and the next action starts once the runtime is quiet, so it can meet the first half done; its error fails the case at the end.

A scripted task kind is `name`, `caps` (`instructions`, `continue`, `restart`), `auto` (steps every attempt runs first), `new_error` (the constructor fails with this text) and `start_tool` (`name`, `description`, `input_schema`, `timeout_ms`): the kind's start tool, which titles the task with the input's `title`, else the kind's name. After its `auto` steps an attempt waits for the steps actions hand it, and a stop ends it. A step is one of `report` (a progress), `ask` (a question; empty withdraws it), `save` (a state), `read` (take the inbox's instructions into the notes), `await` (read the inbox into the notes until the answer arrives), `succeed` (a summary, returned with the notes on the lines after it, as the summary and a text block), `fail` (an error text; the notes are the summary), `ignore_ctx` (from then on a stop does not end it) and `wait_quiet` (wait on its own session, noting `wait_quiet refused: true` when that is refused as re-entrant). A note is an instruction's text and mode in parentheses, followed by ` [answer]` for the answer; a continuation starts its notes with `continued with "<instruction>" from <state>`. Two tool behaviors act on a task with the call's context, as the model would: `task_stop` stops `task` with `text` as the reason and returns once it has ended, `task_restart` restarts it with `task_input`; their result is `task_stop <id>` or `task_restart <id>`, or an error result with a refusal's text.

A scripted compactor summarizes with the model when it has `instructions` (the whole context as the cached prefix, or the lines before the cut when compacting up to a turn; the per-call instructions follow its own), or uses a fixed `summary`. It keeps what `keep` names, `none`, `current_turn` or `after_cut`, reports `strategy` (default `scripted`), and with `fail` fails instead: `nothing_to_compact`, or any other text as an error. A tool behavior and a command may carry a `pin`, which their result declares.

A scripted cost source prices a call by its response id from the round named in `prices` (`{amount, currency, from_round}`), counting rounds from 1, and fails a round named in `errors` with `{status, message}`.

Expected files: `expected.events.jsonl` (every event of the session, from creation on), `expected.store.jsonl` (every line of the session file), `expected.hooks.jsonl` when the case scripts hooks (`{point, name, input}` for every call, the input in `schema/hook.schema.json` form) and `expected.requests/NN.body.json` (the body of each request, for SSE responses) or `NN.request.json` (the neutral request, for event responses), numbered in the order the requests were sent across every provider, with `NN.headers.json` holding the request headers `request_headers` names, `expected.ledger.json` (what each `ledger` action returned, in order), `expected.labels.jsonl` (what each `labels` and `find_sessions` action recorded) and `expected.tasks.jsonl` (what each `task` action that reads recorded, as `task.schema.json#/$defs/task_info` without `started_at` and `ended_at`). With `expect_calls`, `expected.calls.jsonl` holds every record of the session's calls file. Each session a `fork` or `side_chat` action named has `expected.<as>.events.jsonl` (its events from creation on) and `expected.<as>.store.jsonl` (its whole file, which a side chat reopened after a drop shares with the earlier one); the invariants are checked on each session's own lines, and a tool run belongs to the session whose original line committed its call.

Whatever a case scripts, the runner also asserts: seqs run from 1 without gaps and end with `session.closed`; every turn, step, tool, command, hook, approval and task attempt start has exactly one end, a blocked command having only its end and a lost attempt only its `task.finished`; every accepted input is delivered or dropped exactly once; the lines rebuilt from the session's own `message.completed` events, not those relayed from its child sessions, equal the session file apart from native data and batch markers (I4); every call in the current context has exactly one result (I1); no call runs twice, and only calls of committed lines run (I2); in the session file epochs never go back, each opens with a system line carrying a boundary and holds no other system line but a side chat's dropped mark, and a copy points back to an earlier epoch (I6); no request ends with an assistant message except a `pause_turn` continuation (I9); the main loop's requests of one turn, those whose last message is a committed line, send the same model, reasoning parameters and tools, and on a model whose signatures bind the prefix the same tools for a whole epoch (I10). These checks read each request in neutral form, whatever the response kind. The other invariants are not visible in a case's output; bindings test them on their own.

## A2A client cases

`a2a/<v03|v10>/<jsonrpc|httpjson>/<case>/` drives the remote A2A client (`spec/ext/peer/a2a.md`) against a replayed server. `card.json` is served at `<base_url>/.well-known/agent-card.json`; `call.json` holds `base_url`, optional `options` (`protocol_version`, `binding`) and `calls`, each `{"op":"send","message":…,"history_length"}` (a message in the peer form of `spec/ext/peer/README.md`), `{"op":"subscribe","task_id"}` or `{"op":"cancel","task_id"}`. `exchange.jsonl` lists the other round trips in order, each `{"request":{method, url, headers, body}, "response":{status, headers, body | sse, hold}}`: the client's request must have that method and url (with its query), every listed header with that value, and a body equal as JSON (`null` for none); the response is the JSON `body`, or the raw event-stream text `sse`, which with `hold` stays open until the request is canceled. Every exchange must be used and no other request sent. `expected.events.jsonl` holds each call's events in order, then `{"end":"ok"}`, or `{"end":"error","kind":…}` with `kind` `auth` (with `status` and `detail`), `unknown_task` or `failed`. JSON-RPC ids count from 1 per peer. Timers run virtually, so reconnects and polling take no time.

`a2a/discovery/<case>/` holds a `card.json` and an `options.json` (`protocol_version`, `binding`, and `base_url` with `allowed_hosts` for the host check); `expected.json` is the chosen interface `{url, binding, protocol_version, tenant}` or `{"error":"no_supported_interface"}` or `{"error":"interface_host"}`.

## Store cases

`store/<case>/` holds a `session.jsonl`. When the case also has a `script.json`, it is a resume case: it runs like a loop case with `resume` naming that file, and its expected files are those of a loop case. The lines the file already held are not announced again, so the event checks start with the resumed session. Otherwise the file is read as a session file (`runtime.md` §3.1), and `expected.json` records what a reader returns: `lines`, the seqs read; `current_from`, the seq where the current context starts; `error` when reading fails (`incompatible_store`); and `append_error` when appending the case's `append.jsonl` fails (`seq_conflict`). Every line read must also encode back to the same JSON value as the line in the file.

## Comparison rules

`conform diff` applies these rules ([runner-protocol.md](runner-protocol.md)); the decode replay and the choice of compared headers are the runner's.

- An output file is compared with the expected file of the same name. The two sides must have the same files, an absent JSONL file counting as an empty one.
- Documents are compared as JSON values: key order and formatting do not matter, and numbers are compared exactly by value, so `1000`, `1e3` and `1000.0` are equal. A JSONL file is compared line by line.
- Every integer value (by value, so `12.0` too) of a field whose name ends in `_ms` is set to 0 on both sides before comparing. A value of another type, or a fraction, is compared as it is.
- In the events files of loop and store cases, a `model.notice` event is compared without its `detail`. Render notices and decode streams compare it.
- The calls of a parallel group interleave in no fixed order. So in the events file, each run of consecutive `tool.*` and per-call `hook.*` events of one group with more than one call (named by the turn, the step and `tool.started{group}`) is sorted by `call_id`, keeping each call's own order, and the run's `seq` and `time` values are handed back out in their original order; this is done on both sides before comparing. The group's bounds, and every event outside it, compare as they are.
- The reviews of one batch with more than one call run concurrently too: each run of consecutive `approval.resolved` and reviewer `hook.*` events of such a batch is sorted the same way. In the hooks file, each run of consecutive `approval` or `post_tool_use` calls is sorted by the call id in its input.
- A task's own events, those under its hop and its `task.progress`, interleave with the loop's in no fixed order (`runtime.md` §2.1, O4). So each of them is moved to just after the latest `task.started` of its task, keeping the task's own order, and within a child session's events those of its own tasks, under a second hop, are moved the same way, and `seq` and `time` values are handed back out in their original order, as is each relay hop's own `seq` among the events under that hop, on both sides; this is done before the parallel groups are sorted.
- In a case whose clock is `interleaved`, every `time`, `ts`, `started_at` and `ended_at` string in the events, store, calls and ledger files is blanked on both sides, since a task's or an async hook's timestamps and the loop's are taken in no fixed order and each one moves every later timestamp.
- `hook_id` values are replaced, on both sides, by their order of first appearance, because concurrent workers take them in no fixed order.
- A message in a stream event or in `expected.message.json` is compared without its commit envelope (`v`, `id`, `session_id`, `seq`, `epoch`, `ts`). The kernel fills those fields only when it commits the message.
- Fields in the output that the expected file lacks are a failure, and so are missing fields.
- Decode cases are replayed a second time with the body split into random small reads, and the stream must not change.
- Render cases also assert that an Anthropic body carries at most 4 `cache_control` breakpoints; `conform update` refuses such a body too.
- A render case's compared headers are `anthropic-beta`, `anthropic-version` and `x-test`. Credentials and transport headers are not compared.
