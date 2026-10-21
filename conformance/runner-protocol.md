# Runner protocol

Every binding runs the conformance cases with a runner of its own and leaves the comparison to `conform`, so the comparison rules exist once: in [README.md](README.md#comparison-rules) as text and in the Go package `conformance/compare` as code.

| | Runner (one per binding) | `conform` (shared) |
|---|---|---|
| Reads | A case | A case and the runner's outputs for it |
| Does | Builds the case with the binding's own API, runs it, and writes its outputs; asserts what the outputs cannot show (below) | Compares the outputs with the expected files, or accepts them as the new expected files |

## Cases and ids

A case is a directory holding `expected.*` files, or an `errors/<api>/<case>.json` file, whose expected value is its `expected` field. A case's id is its path below `conformance/`, without `.json` for an error case: `loop/simple_turn`, `render/anthropic.messages/text_basic`, `errors/openai.chat/auth_401`.

## Outputs

For each case it runs, the runner writes into `<out>/<id>/` one actual file per expected file: `actual.<rest>` for `expected.<rest>`, inside a directory too (`actual.requests/03.body.json` for `expected.requests/03.body.json`), and `actual.json` for an error case. Each holds what [README.md](README.md) describes for its expected file, in any JSON formatting. An empty JSONL file may be left out.

| Case | Outputs |
|---|---|
| `provider/<api>/<case>` | `actual.stream.jsonl`; `actual.message.json` when the stream ends with a message |
| `render/<api>/<case>` | `actual.body.json`, `actual.headers.json` and `actual.notices.jsonl`; or `actual.error.json` and `actual.notices.jsonl` when rendering refuses to send |
| `errors/<api>/<case>` | `actual.json` |
| `prompt/mustache/<case>` | `actual.json` |
| `prompt/system/<case>` | `actual.system.json` |
| `store/<case>` without a `script.json` | `actual.json` |
| `loop/<case>`, `loop/ext/<x>/<case>`, `store/<case>` with a `script.json` | `actual.events.jsonl` and `actual.store.jsonl`; when the case produces them, `actual.hooks.jsonl`, `actual.ledger.json`, `actual.labels.jsonl`, `actual.tasks.jsonl`, `actual.calls.jsonl`, `actual.<as>.events.jsonl` and `actual.<as>.store.jsonl` for each named session, and `actual.requests/NN.body.json` or `NN.request.json` with `NN.headers.json` |
| `a2a/<version>/<binding>/<case>` | `actual.events.jsonl` |
| `a2a/discovery/<case>` | `actual.json` |

Two files report on the run instead, each holding one line of reason:

- `actual.skip`: the runner did not run the case, because the binding does not support its `spec_version`, lacks an extension it names, or does not implement it yet. A skipped case is not a failure; this is how a binding marks a case as pending.
- `actual.fail`: one of the runner's own assertions failed, or the case could not be run. It is a failure.

A case with no directory under `<out>` was not run, which is a failure too.

## What the runner asserts

Some requirements cannot be seen in the outputs, so the runner checks them and writes `actual.fail` when one does not hold:

- Loop and resume cases: the invariants README.md lists after the expected files ("Whatever a case scripts, the runner also asserts").
- Decode cases: replayed again with the body split into random small reads, the stream does not change.
- A2A cases: each request matches its exchange, every exchange is used, and no other request is sent (README.md, "A2A client cases").
- A loop or resume case whose `spec_version` the binding does not support is skipped, with `actual.skip`; README.md, under `script.json`, says which versions a binding runs.

## `conform`

```
conform diff   <cases> <out>
conform update <cases> <out>
```

`<cases>` is the conformance directory, a subtree of it, or one case; `<out>` holds the outputs of `<cases>` at the same relative paths. Install it with `go install github.com/estonshi/aperture-potatos/tools/cmd/conform@<version>`; it uses only the Go standard library.

`diff` compares each case by the comparison rules. The files on the two sides must match, an absent JSONL file counting as an empty one; a JSONL file is compared line by line and must have as many lines, and any other file is compared as one JSON document. It prints every case that did not pass with the first difference in each file that differs, then the numbers passed, failed, skipped and not run. It exits with 1 when a case failed or was not run, and with 2 on a usage error.

`update` makes the expected files of every case that ran equal to its outputs. It rewrites only the files that differ by the comparison rules, so a difference the rules ignore, such as an order that is free or a duration, never changes a fixture. It removes expected files the outputs have no counterpart for. It writes `.json` files indented by two spaces and `.jsonl` files one compact document per line, both with keys sorted. It refuses a case with `actual.fail`, and an Anthropic body over the breakpoint limit. It exits with 1 when it refused or could not write a case, and with 2 on a usage error. Every change it makes is reviewed against `spec/runtime.md` before it is committed: the text of the spec decides what is right, not the output of any binding. The change also needs an entry in `spec/CHANGELOG.md`.

## The Go binding

The Go tests are the Go binding's runner. Each case writes its outputs into a scratch directory and compares them through `conformance/compare`, and `-update` accepts them through it. With `POTATOS_CONFORM_OUT=<dir>` set, the tests also keep the outputs under `<dir>/<id>/`, and `conform diff conformance <dir>` then checks them like any other binding's: a case the tests skip leaves `actual.skip`, and one that fails before it has outputs leaves `actual.fail`.
