# OpenAI Responses mapping

This mapping covers `openai.responses`, used statelessly: nothing is stored server-side, so every request carries the whole history. It is normative for every binding's adapter; conformance cases live under `conformance/{provider,render,errors}/openai.responses/`.

## Transport

- **Request:** `POST {base}/responses`. The base includes the version path; the default is `https://api.openai.com/v1`.
- **Headers:**
  - `content-type: application/json`
  - `accept: text/event-stream`
  - `authorization: Bearer <key>` when a key is configured
- **Request header precedence, the Doer and timeouts** work as in `openai_chat.md`.

## Request body

Keys, with every request's `params.extra` merged in last:

| Key | Value |
|---|---|
| `model` | The request model. |
| `stream` | `true`. |
| `stream_options` | `{include_obfuscation: false}`. |
| `store` | `false`. |
| `truncation` | `"disabled"`: the runtime manages the context, so the server must never drop items on its own. |
| `instructions` | The non-empty system blocks joined with a blank line, sent with every request. |
| `input` | See below. |
| `max_output_tokens` | Always sent, computed as `max_tokens` is in `anthropic_messages.md`. It includes reasoning. |
| `tools` | Only with tools or hosted tools: `{type:"function", name, description, parameters, strict}`, then the hosted tools (see Hosted tools). A deferred definition adds `defer_loading:true` when the request sends the neutral tool search, and is an ordinary definition otherwise. `strict` is always present, because the API turns strict mode on when it is absent. It is `true` only when the definition asks for strict mode and the schema meets it. A definition that asks for strict mode with a schema that does not meet it raises a `capability_adjusted` notice with `detail {field:"tools.<name>.strict", from:true, to:false}`. |
| `tool_choice` | Only with tools and a mode: `"auto"`, `"none"`, `"required"`, or `{type:"function", name}` for named. |
| `parallel_tool_calls` | `false`, only with tools and `tool_choice.disable_parallel`. |
| `reasoning`, `include` | See Reasoning and Hosted tools; `include` is sent when either adds to it, web search first. |
| `temperature`, `top_p` | As configured, after gating. |
| `prompt_cache_key` | The request's `cache_key`. |
| `prompt_cache_options` | `{mode:"explicit"}`, only with `caps.prompt_cache.explicit`; see Explicit caching. |

The API has no stop parameter. `params.stop_sequences` is dropped with a `capability_adjusted` notice.

### Explicit caching

Only when `caps.prompt_cache.explicit` is declared (GPT-5.6 and later take it; on other models automatic caching applies and nothing is marked). `prompt_cache_breakpoint: {mode:"explicit"}` marks the last content part of an input item, in the priority order and limit of `anthropic_messages.md` (4, or a smaller `caps.prompt_cache.max_breakpoints`) without the system marks, since `instructions` take none:

1. The last rendered item of the last user-side source message, tool messages included, or, when `cache_tail` bounds the prefix, of the last user-side message within it.
2. When `shared_prefix` is set, the last rendered item of the last user-side message among the first `shared_prefix` messages, when that is another message.
3. The last rendered item of the epoch head, when that is another message.

A marked `message` item marks its last part. A marked `function_call_output` whose `output` is a string turns it into one `input_text` part carrying the mark; an empty one is left unmarked.

### Hosted tools

Each of the request's `hosted_tools`, in order, after the function tools:

- **Neutral web search** (`web_search` set): `{type:"web_search"}`, plus `filters` with `allowed_domains` or `blocked_domains`, `user_location` as `{type:"approximate", city?, region?, country?, timezone?}`, and `search_context_size` from `context_size`, each when set. `include` gains `"web_search_call.action.sources"`, without which a search call names only some of the pages it read. `max_uses` has no field here: it is dropped with a `hosted_tool_adjusted` notice, `detail {tool, field:"max_uses", from, to:null}`. A known `caps.no_web_search` leaves the tool out with a `hosted_tool_unsupported` notice, `detail {tool, reason:"no_web_search"}`.
- **Neutral tool search** (`tool_search` set): `{type:"tool_search"}`. A known `caps.no_tool_search` leaves it out with a `hosted_tool_unsupported` notice, reason `no_tool_search`, and the deferred definitions go as ordinary ones.
- **Raw definition:** `raw["openai.responses"]` is appended verbatim. A hosted tool with none of these leaves the request with a `hosted_tool_unsupported` notice, reason `no_definition`.

`tool_choice` never names a hosted tool.

### Input items

Items follow the history in order.

- **User-side messages** (every role except assistant and tool): `{type:"message", role:"user", content:[parts]}`, skipped when it has no parts.
  - A text block or a reminder is `{type:"input_text", text}`. A reminder is encoded as in `runtime.md` §5.7.
  - An image is `{type:"input_image", image_url}`, where `image_url` is a data URL when data is present.
  - A document is `{type:"input_file", file_data | file_url, filename?}`, where `file_data` is a data URL.
- **Assistant messages** become one item per block, in order:
  - A non-empty text block with native data from this API is that raw item. Otherwise it is `{type:"message", role:"assistant", content:[{type:"output_text", text}]}`. A refusal block renders the same way.
  - A reasoning block with native data from this API is that raw item, as the done event delivered it: `summary` (which the input schema requires, even when empty) and `content`, such as `[{type:"reasoning_text", text}]`, are kept. `projection.md` has already dropped reasoning from another family, and reasoning that has neither `encrypted_content` nor, for raw reasoning, the same endpoint.
  - A tool call is `{type:"function_call", call_id, name, arguments}`, built on its raw item when it has one from this API. `call_id`, `name` and `arguments` always come from the canonical block, and `arguments` is the input as compact JSON.
  - An opaque block or hosted call from this API is its raw item. A hosted result with native data from this API is its raw item too; one without, such as a `web_search_call`'s result, is not sent, since the call's item already holds it.
  - Every raw item is sent without its `id`, because nothing is stored and an id makes the request fail.
  - A run of reasoning items at the end of a message is dropped. The API rejects a reasoning item without the item it precedes, and an interrupted reply can leave one.
- **Tool messages:** each result is `{type:"function_call_output", call_id, output}`. `output` is the result's text blocks joined with a blank line, or, when the result has an image or document, the list of its parts as for user messages.

### Reasoning

`reasoning` is sent when `params.reasoning.effort` is set, when `params.reasoning.mode` is `on`, or when the model's `caps.reasoning.modes` is known and the mode is not `off`. So a model that may not reason never receives it.

- `effort` is the configured effort, remapped only when the model's efforts are known.
- `summary` is `"auto"` unless `hide` is set.
- Whenever `reasoning` is sent, `include: ["reasoning.encrypted_content"]` is sent too. Without it a stateless request never receives reasoning it can replay.

### Gating

These rules act only on known capabilities, each with a `capability_adjusted` notice.

| Condition | Action |
|---|---|
| `no_sampling` | Drop `temperature` and `top_p`. |
| Forced tool choice with `no_forced_tool_choice` | Downgrade to `"auto"`. |
| Configured output above a known `max_output_tokens` | Clamp to it. |

### Degraded replay (`replay: fallback`)

Every reasoning item is removed. Without them a request only loses the earlier reasoning context. Ids are never sent, so no function call is left pointing at a missing reasoning item.

### Tool ids

A call id is valid when it matches `^[a-zA-Z0-9_-]{1,64}$`. Other ids are remapped as `projection.md` step 6 specifies.

## Stream decoding

Each event's `type` field names it; the SSE event name is used when the field is absent. Unknown event types are ignored.

| Event | Stream events |
|---|---|
| `response.created`, `response.in_progress`, `response.queued` | The first one: `start` with `response_id` (`response.id`), `request_id` (the `x-request-id` header) and the captured `headers`. |
| `response.output_item.added` | `block_start` for a `message` (text), `reasoning`, `function_call`, `web_search_call`, `tool_search_call` or `tool_search_output` item; for a `web_search_call` it carries `tool_call_id` (the item id) and `tool_name` `web_search`, for the tool search items the call's id and `tool_search`. An item of any other type opens only when it is done. |
| `response.output_text.delta` | `text_delta` with channel `text`. |
| `response.refusal.delta` | `text_delta` with channel `refusal`, into a refusal block of that message item. |
| `response.reasoning_summary_part.added` | For every part after the first, when text already arrived: a `reasoning_delta` of `"\n\n"`. |
| `response.reasoning_summary_text.delta` | `reasoning_delta` of kind `summary`. |
| `response.reasoning_text.delta` | `reasoning_delta` of kind `raw`. |
| `response.function_call_arguments.delta` | `tool_args_delta`. |
| `response.output_item.done` | `block_stop` with the final block; see below. |
| `response.completed` | `usage`, then `end`. |
| `response.incomplete` | `usage`, then `end`; see Stop. |
| `response.failed` | The stream ends with `response.error`, classified as a mid-stream error. |
| `error` | The stream ends with that error, classified as a mid-stream error. Both shapes are accepted: fields at the top level, or nested under `error`. |
| End of body before a terminal event | A transient error with code `stream_incomplete`. |

**`sequence_number`** is checked to increase and nothing more. When it does not, a `sequence_regression` notice is raised with `detail {from, to}`.

Items are matched by `item_id`, then by `output_index`. A block's position counts blocks in the order they open.

**The done item is authoritative.** Every final block keeps the done item as `native.raw`.

- **`message`:** a text block. The text is the item's `output_text` parts joined, or the streamed text when it has none.
- **`reasoning`:** a reasoning block. `encrypted_content` is taken from here, never from the added item, which may hold a partial value.
  - The text is the streamed text, else the item's `summary` texts joined with a blank line, else its `reasoning_text` content.
  - The kind is `raw` when reasoning text arrived, else `encrypted` when the text is empty and `encrypted_content` is present, else `summary`.
  - The block starts as kind `summary`, since the kind is not known until text arrives.
- **`function_call`:** a tool call run by `call_id`, never by the `fc_` item id. The item's `arguments` override the streamed fragments and parse as in `anthropic_messages.md`. A `status` other than `completed` sets `input_error` to `incomplete`, so the call is never run.
- **`message` citations:** every `url_citation` annotation of an `output_text` part becomes `{url, title, start, end}` in the text block's `citations`. `start_index` and `end_index` are taken as code point offsets into their part and shifted by the code points of the parts before it, since the block joins them.
- **`web_search_call`:** a hosted call with `id` (the item id), `name` `web_search`, `status`, and `input`, the item's `action` without `sources`. A hosted result follows it right away, with `block_start` and `block_stop` of its own: `call_id` and `name` of the call, `sources` one `{url, title?}` per `action.sources` item with a `url`, and no native data.
- **`tool_search_call`:** a hosted call with `id` (the item id), `name` `tool_search`, `status`, `input` the item's `arguments` (an object, or a string holding one), and the item in `native`.
- **`tool_search_output`:** a hosted result with `call_id` the id of the latest `tool_search_call` of this response (empty when there is none), `name` `tool_search`, `tools` the `name` of each definition in its `tools`, in order, a namespace contributing the names of its own `tools`, and the item in `native`. Both are replayed as their raw items like any hosted block; that the API takes them back without ids under `store:false` is to be verified live.
- **Any other type**, including other hosted tool items and `compaction`: an opaque block of kind `openai.responses:<type>`, with any string `text` as readable text.

### Stop

- `response.completed`: `tool_use` when the final content has a tool call, otherwise `end_turn`. The raw value is `completed`.
- `response.incomplete`: `incomplete_details.reason` maps by `data/stop_reasons.json` (`max_output_tokens` to `max_tokens`, `content_filter` to `content_filter`, anything else to `other`) and becomes the raw value. `reasoning_exhausted` is set when the output held reasoning and nothing else. `stop.details` is `incomplete_details`.

### The message

The final message has role `assistant`, origin `model`, the producer as in `anthropic_messages.md`, `usage` and the mapped `stop_reason`. Its content is the closed blocks in position order, with two removals:

- empty text blocks;
- every run of reasoning blocks not followed right away by another block. That covers a trailing reasoning item and one whose following item never closed. Replaying such an item fails the request.

**Usage:**

- `input_tokens` = `input_tokens`.
- `cache_read_tokens` = `input_tokens_details.cached_tokens`.
- `cache_write_tokens` = `input_tokens_details.cache_write_tokens`.
- `output_tokens` = `output_tokens`.
- `reasoning_tokens` = `output_tokens_details.reasoning_tokens`.
- `hosted_calls`: `{web_search: n}`, where n counts this response's `web_search_call` items whose `action.type` is `search`, when n is positive. The usage object reports no count of its own.
- `raw` is the wire object.

## Errors

HTTP errors, `error` events and `response.failed` are classified by `data/errors.json`, as in `openai_chat.md`.

## Capability resolution

As in `openai_chat.md`.
