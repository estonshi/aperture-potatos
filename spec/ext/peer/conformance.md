# ext/peer conformance

Cases live in `conformance/loop/ext/peer/`. A case names the extension in `agent.extensions` as `"peer": options`, where `peers` are scripted remote peers and `local` are in-process agents; the extension gets the scripted peers first, then the local ones, each in order. Another extension a case names beside this one is built with its default options, and a case naming one the binding does not have is skipped. The remote A2A client has its own fixtures in `conformance/a2a/` (`conformance/README.md`).

## Scripted peers

| Field | Meaning |
|---|---|
| `name`, `description`, `skills` | The card: skills as the peer form's card skills (`id`, `name`, `description`, `tags`, `examples`). The card always streams |
| `rounds` | What the peer does with each message it is sent, one round per message in order; a message after the last round gets no events |
| `subscribe` | Task id → the events a subscribe to that task returns; any other task id fails as unknown |

A round:

| Field | Meaning |
|---|---|
| `events` | Peer events (`README.md`) yielded in order. In their message and artifact part texts, `{{text}}`, `{{context_id}}` and `{{task_id}}` become the received message's text (its parts' texts joined), context id and task id, in one pass |
| `start_ms` | Virtual time before the first event |
| `delay_ms` | Virtual time before the second event. A cancel during it ends the round with a `canceled` status for the canceled task id instead |
| `hold` | After the events, wait for a cancel, then yield a `canceled` status for that task id |
| `auth_error` | `{status, detail}`: the round fails with an auth error instead of yielding events |

A cancel of a task records it and wakes the round that waits for one. A round whose request is canceled ends with the cancellation error.

## In-process peers

| Field | Meaning |
|---|---|
| `nickname`, `description`, `instructions` | The other agent |
| `replies` | Its model's answers, one per request in order: `text` for a reply, or `tool` and `input` for one tool call with id `call_` and the letter of the reply's position (`a` for the first); `hold_ms` holds the response that long before it starts |
| `peers` | Scripted peers of its own, as above, given to its own peer extension |

Each in-process agent has its own scripted model (`anthropic.messages`, model `m`), an in-memory store, a clock starting at 2026-10-02T00:00:00Z that steps 1 ms per reading, and sequential ids prefixed with its nickname and `_`. The in-process agents close when the case's session ends.
