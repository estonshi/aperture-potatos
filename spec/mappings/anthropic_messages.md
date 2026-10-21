# Anthropic Messages mapping

This mapping covers `anthropic.messages`. It is normative for every binding's adapter; conformance cases live under `conformance/{provider,render,errors}/anthropic.messages/`.

## Transport

- **Request:** `POST {base}/v1/messages`. The default base is `https://api.anthropic.com`, and a gateway such as LiteLLM replaces it.
- **Headers:**
  - `content-type: application/json`
  - `accept: text/event-stream`
  - `anthropic-version: 2023-06-01`
  - `x-api-key` when a key is configured
  - `anthropic-beta`: the configured betas, then the betas the request's features need, then any values in the request's own `anthropic-beta` header. They are trimmed, de-duplicated in that order, and comma-joined. A beta is added only when a feature that needs it is in use.
- **Request header precedence:** `params.headers` are sent as given and win over the adapter's headers of the same name. `anthropic-beta` is the exception: it is merged as described above.
- **The Doer:** every request goes out through the configured Doer, which may rewrite the URL, headers and body, and may replace the response body. For example, a Doer can transcode a cloud backend's frames into SSE. The adapter does not re-validate what the Doer changed.
- **Timeouts:** the first-event timeout runs from sending the request, connecting and waiting for the response headers included, to the first byte of the body; when it expires the request is canceled, or the body closed once headers have arrived. After that the idle timeout counts between reads, comments included, and closes the body. Their defaults are in `data/limits.json`. A timeout is a transient error with code `first_event_timeout` or `idle_timeout`.

## Request body

Keys, with every request's `params.extra` merged in last:

| Key | Value |
|---|---|
| `model` | The request model. |
| `stream` | `true`. |
| `max_tokens` | Always sent. The value is `params.max_output_tokens`, or 64,000 when that is zero, clamped to a known `caps.max_output_tokens`. |
| `system` | One `{type:"text", text}` per non-empty system block. |
| `messages` | See below. |
| `tools` | `{name, description, input_schema}`, plus `strict:true` only when the definition asks for strict mode and the schema meets it: every object closes `additionalProperties` and requires all its properties. A definition that asks for strict mode with a schema that does not meet it raises a `capability_adjusted` notice with `detail {field:"tools.<name>.strict", from:true, to:false}`. A deferred definition adds `defer_loading:true` when the request sends the neutral tool search, and is an ordinary definition otherwise. Hosted tools follow the function tools (see Hosted tools); `tools` is sent when either list is non-empty. |
| `tool_choice` | Only when there are tools and a mode or `disable_parallel` is set: `auto`, `none`, `any` (required) or `{type:"tool", name}` (named). `disable_parallel_tool_use:true` is added to every mode except `none`. |
| `thinking`, `output_config` | See Reasoning. |
| `temperature`, `top_p` | As configured, after sampling gating. |
| `stop_sequences` | As configured. |

When a capability is unknown, the adapter sends what the caller configured and adds nothing.

### Messages

- System lines are never messages.
- Assistant messages render as role `assistant`. Every other role renders as `user`.
- **Blocks:**
  - A text block with native data from this API is sent as that raw block. Otherwise it is `{type:"text", text}`, and empty text blocks are skipped.
  - A refusal block is sent as a text block.
  - A reminder is a text block, encoded as `runtime.md` §5.7 specifies: `<system-reminder source=VALUE>\n…\n</system-reminder>`, where VALUE is the source written as an attribute value (a JSON string after runtime-tag escaping) and the body has its runtime tags escaped (`data/runtime_tags.json`).
  - An image or document is `{type, source}`. The source is `{type:"base64", media_type, data}` when data is present, otherwise `{type:"url", url}`. A document's `filename` becomes `title`.
  - Assistant blocks:
    - A tool call is `{type:"tool_use", id, name, input}`, where `input` is `{}` unless the stored input is a JSON object.
    - Reasoning, opaque and hosted blocks from this API are sent as their raw block, unchanged, except as Open server calls below says.
  - A tool result is `{type:"tool_result", tool_use_id, content?, is_error?}`. Its content holds the text, image and document blocks of the result, and is omitted when empty.
- **Open server calls:** a server call is a hosted call, or an opaque block whose raw `type` ends in `_tool_use` (an older session's `server_tool_use`, or `mcp_tool_use`); its result is a hosted result, or an opaque block with a `tool_use_id`. A call is open when no result in its own message answers it. An open call stays only when:
  - it is in the last message of a `pause_turn` resend, since the server goes on with that call; or
  - the user-side messages after it, up to the next assistant message or the end of the request, hold at least one tool result and nothing else, and either a later assistant message carries its result or no assistant message follows. This is the mixed call: the API ran the client calls first and runs the server call once their results arrive (`runtime.md` §4.4).

  Any other open call is left out (such as one cut off by the output cap), because the API takes nothing but tool results after one. A result whose call is left out, or is not in the request, is left out too. The stored messages are unchanged.
- **Merging:** a message left with no blocks is skipped. Adjacent user-side messages merge into one, which includes a tool message and the user message after it. In the merged message every tool result comes first, in order, then the other blocks in order.

### Hosted tools

Each of the request's `hosted_tools`, in order, after the function tools:

- **Neutral web search** (`web_search` set): `{type:"web_search_20250305", name}`, plus `max_uses`, `allowed_domains`, `blocked_domains`, and `user_location` as `{type:"approximate", city?, region?, country?, timezone?}`, each when set. `context_size` has no field here: it is dropped with a `hosted_tool_adjusted` notice, `detail {tool, field:"context_size", from, to:null}`. A known `caps.no_web_search` leaves the tool out with a `hosted_tool_unsupported` notice, `detail {tool, reason:"no_web_search"}`.
- **Neutral tool search** (`tool_search` set): `{type:"tool_search_tool_bm25_20251119", name:"tool_search_tool_bm25"}`. A known `caps.no_tool_search` leaves it out with a `hosted_tool_unsupported` notice, reason `no_tool_search`, and the deferred definitions go as ordinary ones.
- **Raw definition:** `raw["anthropic.messages"]` is appended verbatim. A hosted tool with none of these leaves the request with a `hosted_tool_unsupported` notice, reason `no_definition`.
- **Removed tools a search loaded:** when a replayed `tool_search_tool_result` names, in its `tool_references`, a tool that is not among the request's tools, one definition per such name, in byte order, follows the function tools: `{name, description, input_schema:{type:"object"}}`, the description being the `tool_removed` text of `prompts/reminders.md`, plus `defer_loading:true` when the request sends the neutral tool search. Without it the API refuses the whole request; a call to it gets `unknown_tool` like any unregistered name. The stored messages are unchanged.

`tool_choice` never names a hosted tool and is decided by the function tools alone.

### Counting tokens

The adapter counts a request's input with `POST /v1/messages/count_tokens`: the body is the one the request renders to, keeping only `model`, `messages`, `system`, `tools`, `tool_choice` and `thinking`, with the same headers. The answer's `input_tokens` is the count.

### Cache breakpoints

Breakpoints are `cache_control: {type:"ephemeral"}` on a block. They are assigned in this priority order, up to a limit. The limit is the API's 4, or a smaller declared `caps.prompt_cache.max_breakpoints`.

| Priority | Breakpoint | Where |
|---|---|---|
| B1 | System end | The last system block. |
| B2 | Rolling tail | The last block of the last user-side message. When the request sets `cache_tail`, the last block rendered from the last user-side message among its first `cache_tail` messages, even when that message merged with a later one. |
| B2s | Shared prefix | When the request sets `shared_prefix`, the last block rendered from the last user-side message among its first `shared_prefix` messages, even when that message merged with a later one, when that is not the B2 block. |
| B3 | Epoch head | The last block of the first message with origin `epoch_head`, when that is neither the B2 nor the B2s block. |
| B4 | Agent/session cut | The last system block before the final one that the kernel marked with `cache: true`. |

Breakpoints beyond the limit are dropped from the lowest priority up, so B1 and B2 are always kept. Tools carry no breakpoint: the prefix is hashed tools, then system, then messages, so B1 covers the tools too.

### Reasoning

`params.reasoning.mode` decides whether `thinking` is sent:

| Mode | `thinking` |
|---|---|
| `off` | `{type:"disabled"}`. If the model's modes are known and it declares `no_disable`, nothing is sent and a `capability_adjusted` notice is raised. |
| `on` | Sent. |
| `auto` or absent | Sent only when `caps.reasoning.default_on` is true; otherwise the model's default applies. |

When thinking is sent, its form is chosen as follows:

- **Manual form** (`{type:"enabled", budget_tokens}`) is chosen when a budget is given. If the known modes lack `enabled` but include `adaptive`, the adaptive form is used instead, with a notice.
- **Adaptive form** (`{type:"adaptive", display}`) is used otherwise. If the known modes lack `adaptive` but include `enabled`, the manual form is used.
- **Display:** the adaptive form's `display` is `"summarized"`, or `"omitted"` when `hide` is set. The manual form sends no `display`.
- **Budget:** the budget is the configured one, or half of `max_tokens` when none is given. It is clamped to `[max(budget_min, 1), max_tokens − 1]`, where the lower bound applies only when `budget_min` is known. If that range is empty, the adapter fails locally with class `capability`.
- **Interleaved beta:** `interleaved-thinking-2025-05-14` is added only for the manual form with tools, when `caps.reasoning.interleaved_beta` is true.

`params.reasoning.effort` goes to `output_config.effort`. Only when the model's efforts are known is it remapped to the nearest supported level, using the order none, minimal, low, medium, high, xhigh, max. A tie goes to the level closer to `high`.

### Gating

These rules act only on known capabilities (the tool_choice downgrade under manual thinking is an API fact). Each adjustment raises a `capability_adjusted` notice with `detail {field, from, to}`.

| Condition | Action |
|---|---|
| `no_sampling` | Drop `temperature` and `top_p`. |
| Thinking on and `sampling_with_thinking` is `none` | Drop both. |
| Thinking on and `sampling_with_thinking` is `top_p_only` | Drop `temperature` and clamp `top_p` to [0.95, 1]. |
| Forced tool choice (`any` or `tool`) with `no_forced_tool_choice`, or with the manual thinking form | Downgrade to `auto`. No text is injected. |
| Configured output above a known `max_output_tokens` | Clamp to it. |

The window-based cap ("a quarter of a known window") is applied by the kernel before the request reaches the adapter.

### Degraded replay (`replay: fallback`)

- **Models that declare `thinking_prefix_bound`:** the history is unchanged. The adapter adds beta `thinking-binding-controls-2026-08-01` and sets `thinking.block_binding.prefix_mismatch_behavior` to `"drop_block"`. If thinking was not otherwise sent, it sends `{type:"adaptive"}` with that binding.
- **Other models:** reasoning, opaque and hosted blocks are removed from every assistant message except the latest.

## Stream decoding

| Wire event | Stream events |
|---|---|
| `message_start` | `start` with `response_id` (`message.id`), `request_id` (the `request-id` or `x-request-id` header) and the captured `headers`; then `usage`. If `message.input_transformations` is present, it is kept on the message's `native` as `{input_transformations}`, and one containing `thinking_dropped` also raises a `thinking_dropped` notice whose `detail` is `{input_transformations}`. |
| `content_block_start` | `block_start`, and see the block rules below. |
| `text_delta` | `text_delta` with channel `text`. |
| `thinking_delta` | `reasoning_delta`. |
| `signature_delta` | None; the signature is kept for the raw block only. |
| `citations_delta` | None; the citation is appended to the text block's raw form and its citations. |
| `input_json_delta` | `tool_args_delta` for a tool call; for a hosted call or an opaque block, it is accumulated into the raw `input`. |
| `content_block_stop` | `block_stop` with the final block. |
| `message_delta` | `usage` when usage is present. The stop reason is recorded. `stop_details` is read from `delta` and from the top level. |
| `message_stop` | `end` with the stop, the final message and the usage. |
| `ping`, unknown event types, unknown delta types | Ignored. With `emit_raw`, every event is also sent as `raw`, with the event name in `text`. |
| `error` | The stream ends with that error, classified by `data/errors.json` as a mid-stream error. |
| End of body before `message_stop` | A transient error with code `stream_incomplete`. It is never a success. |

A block's index is its position within the message.

**Blocks:**

- **`text`:** a text block. It keeps its raw wire form in `native` only when citations arrived. Each citation with a `url` (such as `web_search_result_location`) becomes `{url, title, cited_text}` in `citations`.
- **`thinking`:** a reasoning block with the accumulated `thinking` and `signature` in `native.raw`. Its kind follows the requested display: `summary`, `omitted`, or `progress` for `updates`. A summary block that ends with empty text becomes `omitted`.
- **`redacted_thinking`:** a reasoning block of kind `redacted`, with the whole wire block in `native.raw`.
- **`tool_use`:** a tool call.
  - An empty accumulated input is `{}`.
  - Input that is not a JSON object leaves `input` as `{}`, keeps the text in `raw_input`, and sets `input_error` to `invalid_json` or `not_object`.
  - When the stop reason is `max_tokens`, any `input_error` becomes `incomplete`.
- **`server_tool_use`:** a hosted call with `id`, `name`, the accumulated `input` (or the start block's), and the raw block with that input in `native`. The name `tool_search_tool_bm25`, the neutral tool search's, becomes `tool_search`. Its `block_start` carries `tool_call_id` and `tool_name`.
- **A block whose `type` ends in `_tool_result` and whose `tool_use_id` names a `server_tool_use`** of this response, or a hosted call (or opaque `server_tool_use`) in an assistant message of the request: a hosted result with `call_id`, that call's `name`, and the raw block in `native`. For `web_search_tool_result`, a `content` list gives `sources`, one `{url, title, age}` per item with a `url` (`age` is `page_age`), and an error object gives `error`, its `error_code`. For `tool_search_tool_result`, `content.tool_references` gives `tools`, each `tool_name` in order, and a `tool_search_tool_result_error` gives `error`, its `error_code`.
- **Anything else** is an opaque block of kind `anthropic.messages:<type>`, with its raw block in `native` and any string `text` as readable text. This covers `mcp_tool_use`, `mcp_tool_result`, other results no server call names, `connector_text`, `compaction`, `fallback` and unknown types. A `fallback` block also raises a `fallback` notice whose `detail` is the block.

The final message has role `assistant` and origin `model`:

- `producer`:
  - `api`, `endpoint` (the normalized host), `model` (the request model);
  - `family` only when a declared family differs from the model;
  - `response_id`, `request_id`, `headers`.
- `usage`, and `stop_reason`, the mapped stop reason.

Its content is the closed blocks in order, with these changes:

- Empty text blocks without native data are dropped.
- Before the last `fallback` block, reasoning blocks, tool calls and `connector_text` blocks are dropped, so calls from the abandoned generation never run.

**Usage** is cumulative. Each field of a later `usage` object replaces the earlier value, and values are never summed.

- `input_tokens` = `input_tokens` + `cache_read_input_tokens` + `cache_creation_input_tokens`.
- `cache_read_tokens` = `cache_read_input_tokens`.
- `cache_write_tokens` = `cache_creation_input_tokens`.
- `output_tokens` = `output_tokens`.
- `reasoning_tokens` = `output_tokens_details.thinking_tokens`.
- `hosted_calls`: each positive `server_tool_use.<name>_requests` as `<name>: count`, such as `web_search_requests` as `web_search`.
- `raw` is the merged wire object.

Stop values map by `data/stop_reasons.json`.

## Errors

HTTP errors and mid-stream `error` events are classified by `data/errors.json`:

- **Request id:** the body's `request_id`, else the `request-id` header.
- **Retry delay:** `retry-after-ms`, else `retry-after`, whether given in seconds or as an HTTP date.
- **No retry:** `x-should-retry: false` sets `no_retry`, so the error is not retried whatever its class.

## Capability resolution

`GET {base}/v1/models/{model}` maps these fields and leaves every other field unknown:

| Response field | Capability |
|---|---|
| `max_input_tokens` | `context_window` |
| `max_tokens` | `max_output_tokens` |
| `capabilities.thinking.types.<adaptive or enabled>.supported` | `reasoning.modes` |
| `capabilities.effort.<level>.supported`, for level in low, medium, high, xhigh, max | `reasoning.efforts` |
| `capabilities.server_tools.web_search.supported` | `no_web_search` when it is `false`; absent leaves it unknown |
