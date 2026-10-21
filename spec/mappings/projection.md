# Render-time projection

Before rendering any request, every adapter reshapes the canonical history. The steps below run in order. They change only the request, never the history.

1. **Project for the target** by the replay matrix below.
2. **Degraded replay**, only when the request asks for `replay: fallback`. Each API defines the degraded form in its own mapping.
3. **Heal dangling calls** (the second layer of I1). Every tool call without a result in the tool message right after its assistant message gets a result with `outcome: not_executed` and `is_error: true`. That result's content is the `dangling_tool_call` text of `prompts/reminders.md`. Results are ordered by call. A result that answers no call of the preceding assistant message is dropped, and so is a tool message that follows no assistant message with calls. When anything was healed, the adapter raises a `dangling_tool_call_healed` notice with `detail.call_ids`.
4. **API-specific merging.** For example, Anthropic merges adjacent user-side messages.
5. **Tail check** (I9). If the projected history ends with an assistant message and the request is not a pause continuation (`pause_tail`), the adapter fails locally with class `invalid_request` and sends nothing.
6. **Tool id remap.** A call id the target cannot accept is replaced by `h_` followed by the first 24 characters of the unpadded standard base32 encoding of its SHA-256 digest. The same mapping applies to the matching result's `call_id`.

## Replay matrix

A **family** is the producer's `family`, or its `model` when `family` is absent. The target family is the resolved capabilities' `family`, or the request model when none is known.

| Source \ target | anthropic.messages | openai.chat (endpoint E, family F) | openai.responses (family F) |
|---|---|---|---|
| Anthropic thinking / redacted thinking | Replayed whole and in order, empty text included; the API filters what it cannot read | Dropped | Dropped |
| Chat reasoning (endpoint E', family F') | Dropped | Replayed only when E' = E and F' = F | Dropped |
| Responses reasoning (endpoint E', family F') | Dropped | Dropped | Replayed only when F' = F and either the item has `encrypted_content`, or the block's kind is `raw` (its text came from `reasoning_text`) and E' = E |

Other blocks:

- **Opaque blocks, hosted calls and hosted results** are kept only when their `native.api` is the target API; one without native data is never sent. A dropped hosted block counts as a dropped opaque block (`provider.switched.dropped_opaque`). Degraded replay that strips reasoning strips them too. A hosted result can arrive in a later assistant message than its call; `anthropic_messages.md` (Open server calls) says when such a pair is kept.
- **Citations** on a text block are neutral and stay with it; only its native data follows the rule below.
- **Native data on text, refusal and tool-call blocks** from another API is removed from the copy sent. The block itself is kept.
- **The message-level `native`** is kept only for the same API; for openai.chat it also needs the same endpoint and family.
- **Reasoning from another provider is never turned into visible text.**
