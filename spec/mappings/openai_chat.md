# OpenAI Chat Completions mapping

This mapping covers `openai.chat`: OpenAI's Chat Completions API and the servers compatible with it (LiteLLM, vLLM, Ollama, OpenRouter, DeepSeek, Qwen and others). It is normative for every binding's adapter; conformance cases live under `conformance/{provider,render,errors}/openai.chat/`.

## Transport

- **Request:** `POST {base}/chat/completions`. The base includes the version path; the default is `https://api.openai.com/v1`.
- **Headers:**
  - `content-type: application/json`
  - `accept: text/event-stream`
  - `authorization: Bearer <key>` when a key is configured
- **Request header precedence:** `params.headers` are sent as given and win over the adapter's headers of the same name.
- **The Doer and timeouts** work as in `anthropic_messages.md`.
- **The OpenAI endpoint** is the one whose normalized host is `api.openai.com`. A few fields go to it alone, as noted below.

## Request body

Keys, with every request's `params.extra` merged in last:

| Key | Value |
|---|---|
| `model` | The request model. |
| `stream` | `true`. |
| `stream_options` | `{include_usage: true}`, plus `include_obfuscation: false` on the OpenAI endpoint. A server that rejects it is fixed by overriding the key through `params.extra`. |
| `messages` | See below. |
| `max_tokens` | Always sent. The value is `params.max_output_tokens`, or 64,000 when that is zero, clamped to a known `caps.max_output_tokens`. The key is `caps.max_tokens_field` when that is declared, for example `max_completion_tokens`. |
| `tools` | Only with tools: `{type:"function", function:{name, description, parameters}}`, where `parameters` is the input schema or `{type:"object"}`. `strict:true` is added only when the definition asks for strict mode and the schema meets it. A definition that asks for strict mode with a schema that does not meet it raises a `capability_adjusted` notice with `detail {field:"tools.<name>.strict", from:true, to:false}`. |
| `tool_choice` | Only with tools and a mode: `"auto"`, `"none"`, `"required"`, or `{type:"function", function:{name}}` for named. |
| `parallel_tool_calls` | `false`, only with tools and `tool_choice.disable_parallel`. It is never sent as `true`. |
| `reasoning_effort` | See Reasoning. |
| `temperature`, `top_p` | As configured, after gating. |
| `stop` | `params.stop_sequences`. |
| `prompt_cache_key` | The request's `cache_key`, on the OpenAI endpoint only. |

When a capability is unknown, the adapter sends what the caller configured and adds nothing.

The protocol has no tools the server runs, so `hosted_tools` are never sent: each raises a `hosted_tool_unsupported` notice, `detail {tool, reason:"api_unsupported"}`. A deferred function definition is sent as an ordinary one, since no tool search can load it. Hosted blocks from other APIs are dropped by `projection.md`.

### Messages

- **System:** the non-empty system blocks, joined with a blank line (`"\n\n"`), form one first message. Its role is `caps.system_role`, or `system` when that is not declared.
- **User-side messages** (every role except assistant and tool) render as role `user`:
  - A text block and a reminder are text parts. A reminder is encoded as in `runtime.md` §5.7.
  - An image is `{type:"image_url", image_url:{url}}`. The URL is a `data:<media type>;base64,<data>` URL when data is present.
  - A document with data is `{type:"file", file:{file_data, filename?}}`, where `file_data` is a data URL. A document with only a URL becomes the text part `document_link` of `prompts/reminders.md`, because the API has no URL form for files.
  - When every part is text, `content` is the texts joined with a blank line. Otherwise it is the list of parts, with text parts as `{type:"text", text}`.
  - A message with no parts is skipped.
- **Assistant messages:**
  - `content` is the non-empty text and refusal texts joined with a blank line, or `null` when there are none.
  - Each tool call is `{id, type:"function", function:{name, arguments}}`, in order. `arguments` is the stored input as compact JSON, or `"{}"` unless the input is a JSON object.
  - A message with neither text nor tool calls is skipped.
  - Reasoning replay is described below.
- **Tool messages:** each result becomes `{role:"tool", tool_call_id, content}`, in order. `content` is the result's text blocks joined with a blank line, and may be empty. Images and documents cannot sit in a tool message. They move to one user message right after the batch's tool messages: for each result that has any, the text part `tool_result_media` of `prompts/reminders.md` comes first, then that result's media parts.
- **Explicit caching**, only when `caps.prompt_cache.explicit` is declared (some servers bill marked prefixes as explicit cache writes and stop caching implicitly, so nothing is marked unless the caller says the model takes marks). The system message's `content` is then the list of its blocks as text parts, and `cache_control: {type:"ephemeral"}` marks, in the priority order and limit of `anthropic_messages.md` (4, or a smaller `caps.prompt_cache.max_breakpoints`): the last system part; the last rendered message of the last user-side source message (tool lines included), or, when `cache_tail` bounds the prefix, of the last user-side message within it; when `shared_prefix` is set, the last rendered message of the last user-side message among the first `shared_prefix` messages, when that is another message; the last rendered message of the epoch head when that is another message; the system part the kernel marked with `cache: true` when it is not the last. A marked message's string `content` becomes one text part, and the mark goes on its last part.

### Reasoning replay

Only an assistant message whose producer has the same endpoint and family as the request keeps its reasoning (`projection.md`). For such a message, the fields it arrived with are written back:

- A reasoning block whose `native.raw.field` is `reasoning_content` or `reasoning` puts its text in that field.
- The message's `native.raw.thinking_blocks` goes to `thinking_blocks`, keeping only items that are signed or of type `redacted_thinking`. Unsigned items come from aborted streams and are rejected upstream. The key is omitted when nothing is left.
- The message's `native.raw.reasoning_details` goes to `reasoning_details` unchanged.

A reasoning block without `native` is display only and is never sent.

### Reasoning parameters

- `params.reasoning.effort` goes to `reasoning_effort`. Only when the model's efforts are known is it remapped to the nearest supported level, as in `anthropic_messages.md`.
- When `caps.no_tools_with_reasoning` is true and the request has tools, `reasoning_effort` is `"none"` unless it already is. This applies even when no effort was configured.
- `mode`, `budget` and `hide` have no mapping. Vendor switches, such as DeepSeek's `thinking`, Qwen's `enable_thinking` or vLLM's `chat_template_kwargs`, go through `params.extra`.

### Gating

These rules act only on known capabilities. Each adjustment raises a `capability_adjusted` notice with `detail {field, from, to}`.

| Condition | Action |
|---|---|
| `no_sampling` | Drop `temperature` and `top_p`. |
| Forced tool choice (required or named) with `no_forced_tool_choice` | Downgrade to `"auto"`. No text is injected. |
| Configured output above a known `max_output_tokens` | Clamp to it. |
| `no_tools_with_reasoning` with tools | Force `reasoning_effort: "none"`. |

### Degraded replay (`replay: fallback`)

Every replayed reasoning field is removed: reasoning and opaque blocks from every assistant message, and each assistant message's `native`. Most servers treat these fields as optional input; one that requires them rejects the request again.

### Tool ids

A call id is valid when it matches `^[a-zA-Z0-9_-]{1,40}$`, OpenAI's own limit. Other ids are remapped as `projection.md` step 6 specifies.

## Stream decoding

Every data line is a JSON chunk, except the terminator `data: [DONE]`. SSE comments only keep the stream alive. Only choice index 0 is read.

| Chunk content | Stream events |
|---|---|
| The first chunk | `start` with `response_id` (the chunk `id`), `request_id` (the `x-request-id` header) and the captured `headers`. |
| A top-level `error` object | The stream ends with that error, classified by `data/errors.json` as a mid-stream error. It may be the only chunk. |
| `delta.reasoning_content`, `delta.reasoning`, `delta.thinking_blocks`, `delta.reasoning_details` | See Reasoning. |
| `delta.content` (a string) | `text_delta` with channel `text`. `<think>` tags are not parsed; they are visible text. |
| `delta.refusal` (a string) | `text_delta` with channel `refusal`, into one refusal block. |
| `delta.tool_calls[]` | See Tool calls. |
| `finish_reason` | Recorded; the first non-empty value counts, since some servers repeat it with the usage. The values `insufficient_system_resource`, `aborted` and `error` end the stream with a transient mid-stream error whose code is that value. |
| `usage` (not null) | `usage`. The latest value wins. |
| `data: [DONE]` | Every open block closes, then `end`. A `[DONE]` before any chunk is a transient protocol error. |
| A data line that is not JSON | A transient protocol error. |
| End of body before `[DONE]` | A transient error with code `stream_incomplete`. It is never a success. |

With `emit_raw`, every chunk is also sent as `raw`.

**Block boundaries.** The protocol has none, so the decoder makes them. Positions count blocks in the order they open.

- **Reasoning:** one reasoning block of kind `raw` per message. It opens at the first reasoning text and closes when the first content, refusal or tool call arrives, or at the end. Reasoning text that arrives after it closed is ignored.
- **Text:** a text block opens at the first content and closes when the first tool call arrives, or at the end. Content that arrives after it closed opens a new text block.
- **Tool calls:** a tool call block opens at the first fragment of each call (see Tool calls below) and closes at the end, in the order the calls opened, because the arguments of several calls can interleave.

### Reasoning

Each chunk supplies at most one piece of reasoning text, from the first of these sources with non-empty text:

1. `reasoning_content`, a string;
2. `reasoning`, only when it is a string (an object is ignored);
3. the `thinking` text of the chunk's `thinking_blocks` items of type `thinking`;
4. the `text`, or else `summary`, of the chunk's `reasoning_details` items.

Once a string source (1 or 2) has carried text in a message, sources 3 and 4 no longer supply text in that message. This keeps a gateway that sends both a string and an array from showing the text twice.

The reasoning block records the first string source it received in `native: {api:"openai.chat", raw:{field}}`. A block whose text came only from arrays has no `native`.

The arrays are also kept for replay, whatever supplied the text:

- **`thinking_blocks`** fragments merge in arrival order. A `thinking` fragment extends the last item when that item is an unsigned `thinking` item: its text is appended and its `signature`, when present, is set. Every other fragment, including one after a signed item and every `redacted_thinking`, starts a new item.
- **`reasoning_details`** items are appended in arrival order, unchanged.

When either is non-empty, the message's `native` is `{api:"openai.chat", raw:{thinking_blocks?, reasoning_details?}}`.

### Tool calls

- Fragments are matched by `index`, never by arrival order, and one chunk may carry several fragments for one index. A fragment without `index` belongs to the call the previous index-less fragment fed. A fragment whose non-empty `id` differs from the non-empty `id` its call started with opens a new call, which that index then feeds; some servers send every call at index 0 or omit `index`.
- The first fragment gives the call's `id` and `name`. When that `id` is empty, or is already used by a tool call in the request's messages or earlier in this message, the call gets `call_<n>` instead, with the smallest `n`, counting up from the number of ids used so far, that is not used either. Some compatible servers omit the id or number calls from `call_0` in every response, and the kernel reuses the result of a call id that already ran (I2). A later `id` never changes the call's id. A later name equal to the current one is ignored, and a different one is appended, so servers that split the name work too.
- `arguments` fragments are appended and each raises a `tool_args_delta`.
- At close, the arguments parse as in `anthropic_messages.md`: empty is `{}`, and anything that is not a JSON object leaves `input` as `{}` with `raw_input` and `input_error`.

### Stop

The stop reason is decided in this order:

1. `finish_reason: "length"` is `max_tokens`, even when tool calls accumulated. Every tool call with an `input_error` then has it set to `incomplete`. `reasoning_exhausted` is set when the message has no non-empty text or refusal and no tool call.
2. When tool calls accumulated and `finish_reason` is `stop` or missing, the reason is `tool_use`. Some servers report `stop` after a call.
3. Otherwise the value maps by `data/stop_reasons.json`.

### The message

The final message has role `assistant`, origin `model`, the producer as in `anthropic_messages.md`, `usage` and the mapped `stop_reason`. Its content is the closed blocks in position order, without empty text blocks.

**Usage:**

- `input_tokens` = `prompt_tokens`.
- `cache_read_tokens` = `prompt_tokens_details.cached_tokens`.
- `cache_write_tokens` = `prompt_tokens_details.cache_write_tokens`.
- `output_tokens` = `completion_tokens`.
- `reasoning_tokens` = `completion_tokens_details.reasoning_tokens`.
- `raw` is the wire object.

When the stream reports no usage at all, the usage is estimated and marked `estimated`. `input_tokens` is the estimate of the request body. `output_tokens` is the estimate of all text, refusal, reasoning and argument deltas. The estimate of a string is one token per four ASCII characters, rounded up, plus one per other character.

## Errors

HTTP errors and error chunks are classified by `data/errors.json`. The request id is the body's `request_id`, else the `x-request-id` header. The retry delay comes from `retry-after-ms`, else `retry-after`.

## Capability resolution

`GET {base}/models` is fetched once per endpoint, and every listed model is answered from it (`runtime.md` §9.5).
