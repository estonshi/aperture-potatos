# A2A client mapping

How the client for remote agents (`ext/peer/a2a`) speaks A2A v0.3 and v1.0 over JSON-RPC and HTTP+JSON, and how it turns them into peer events (`README.md`). The fixtures in `conformance/a2a/` fix every request's method, url, version header and body.

## Discovery

The card is `GET <base>/.well-known/agent-card.json` with the caller's headers and `Accept: application/json`; a status other than 2xx, or a body that is not JSON, fails. Its interfaces, in card order: each `supportedInterfaces` item (`url`, `protocolBinding`, `protocolVersion`, 1.0 when absent, `tenant`), then, when there is a top-level `url`, that url with `preferredTransport` (`JSONRPC` when absent) and each `additionalInterfaces` item (`url`, `transport`), all at the top-level `protocolVersion` (0.3 when absent). A version is read by its first two numbers: `1.x` is spoken as 1.0, `0.3.x` and `0.2.x` as 0.3; others are dropped, and so are bindings other than `JSONRPC` and `HTTP+JSON`. Interfaces the caller's headers may not reach are dropped first: an interface's url must be http or https, https when the card was fetched over https, and its host (with port) must equal the card's, compared without letter case, unless the caller allows that host, as `host` (any port) or `host:port`. When the card has interfaces and none is left, discovery fails with `interface_host`, so a card cannot send the caller's credentials or requests to another host. The caller may keep one version or one binding. The choice is 1.0 before 0.3, then JSON-RPC before HTTP+JSON, then card order; none left is an error. The card's name, description, version, skills and `capabilities.streaming` become the peer's card, with the chosen url.

## Requests

Every request carries the caller's headers, then `A2A-Version` (`0.3` or `1.0`), `Accept`, and `Content-Type` when it has a body. JSON-RPC posts `{"jsonrpc":"2.0","id":<n>,"method":…,"params":…}` to the url, `id` counting from 1 per peer, with `Content-Type: application/json`. HTTP+JSON uses the paths below under the url (v0.3 prefixed by `/v1`, v1.0 by `/<tenant>` when the interface has one), with `Content-Type: application/json` in v0.3 and `application/a2a+json` in v1.0; a unary request's `Accept` is `application/json`, and `application/a2a+json, application/json` in v1.0 over HTTP+JSON. A stream's `Accept` is `text/event-stream`. A unary request times out after 30 s.

| Operation | v0.3 JSON-RPC | v1.0 JSON-RPC | HTTP+JSON |
|---|---|---|---|
| send | `message/send` | `SendMessage` | `POST /message:send` |
| stream | `message/stream` | `SendStreamingMessage` | `POST /message:stream` |
| get | `tasks/get` | `GetTask` | `GET /tasks/<id>` |
| cancel | `tasks/cancel` | `CancelTask` | `POST /tasks/<id>:cancel`, body `{}` |
| subscribe | `tasks/resubscribe` | `SubscribeToTask` | `POST /tasks/<id>:subscribe`, body `{}`; after a 405, `GET` for the rest of the peer's life |

- **The message.** v0.3 JSON-RPC: `{"kind":"message","messageId","contextId"?,"taskId"?,"role":"user","parts":[…],"metadata"?}` with parts `{"kind":"text","text"}`, `{"kind":"file","file":{"bytes"|"uri","mimeType"?,"name"?}}` and `{"kind":"data","data":{…}}`, a value that is not an object wrapped as `{"value":…}`. v0.3 HTTP+JSON: `{"messageId","contextId"?,"taskId"?,"role":"ROLE_USER","content":[…],"metadata"?}` with parts `{"text"}`, `{"file":{"fileWithUri"|"fileWithBytes","mimeType"?}}` and `{"data":{"data":{…}}}`. v1.0: `{"messageId","contextId"?,"taskId"?,"role":"ROLE_USER","parts":[…],"metadata"?}` with parts `{"text"}`, `{"raw"}` (base64), `{"url"}` or `{"data"}`, each with `mediaType` and `filename` when set.
- **Send and stream params** are `{"message":…,"configuration":…}`. In v0.3 a send's configuration is `{"blocking":false}`, a stream has none, and a history length is sent only when above 0. In v1.0 the configuration is `{"historyLength":<n>}`, a send's with `"returnImmediately":true`, and JSON-RPC adds `"tenant"` when the interface has one. HTTP+JSON sends the same object as its body, without the tenant.
- **Task params** are `{"id"}`; v1.0's get adds `"historyLength":0` (in the query over HTTP+JSON), and JSON-RPC its `"tenant"`.

## Answers

A JSON-RPC answer's `result` is the object; its `error` is the error. An HTTP+JSON answer's body is the object. Over a stream, each event's data is one answer of that kind; empty data is a keep-alive. A stream request answered with JSON instead of an event stream is read as one answer.

- **Objects.** A one-member wrapper `{"task"}`, `{"message"}` (or `{"msg"}`), `{"statusUpdate"}` or `{"artifactUpdate"}` names its object. Otherwise, for v0.3, `kind` does (`task`, `message`, `status-update`, `artifact-update`), and a missing `kind` is inferred: an `artifact` member makes an artifact update, a `status` with an `id` a task, a `status` without one a status update, a `messageId` a message. A task is `{id, contextId, status, artifacts}`, a status `{state, message}` (v0.3 ProtoJSON may name it `update`), an artifact `{artifactId, name, parts}`, a status update `{taskId, contextId, status, final}`, an artifact update `{taskId, contextId, artifact, append, lastChunk}`. Parts of every shape above are read on every interface, and a message's parts may be named `content`.
- **States.** `TASK_STATE_` is dropped and the rest read case-insensitively with `-` as `_`: `submitted`, `working`, `input_required`, `auth_required`, `completed`, `canceled` (or `cancelled`), `failed`, `rejected`. Any other value is `failed`, with `the peer reported the unknown task state "<value>"` appended to the status message. A role is read without `ROLE_`, in lower case.
- **Errors.** HTTP 401 and 403, and JSON-RPC codes -31401 and -31403, are an auth error with the HTTP status they stand for and the `WWW-Authenticate` header (or the error's message) as its detail. JSON-RPC -32001, a v1.0 `ErrorInfo` reason `TASK_NOT_FOUND` and HTTP 404 mean the peer no longer knows the task. A cancel answered with -32002, the reason `TASK_NOT_CANCELABLE` or, without a reason, HTTP 409 succeeds. A v1.0 HTTP+JSON error body is `{"error":{"code","status","message","details":[{"reason",…}]}}`. Every other error fails the exchange.

## A round

- **Events.** The first task snapshot becomes a task event, one artifact event per artifact, and a status event when its state ends the round. A later snapshot (after a gap, or the snapshot v1.0's subscribe starts with) becomes an artifact event for each artifact that is new or changed, then a status event when the state or its message changed or the state ends the round. A status update is a status event; an artifact update is an artifact event with its `append` and `lastChunk`. A message ends the round.
- **Streaming**, when the card says so, sends with stream and reads until the round ends. A v0.3 status update with `final` whose state does not end the round is read as the stream closing. A stream that closes before any answer fails, and the message is not sent again. One that closes later is checked: after 1, 2 and 4 seconds in turn, get, and when the round has not ended, subscribe again; a fourth close fails. A stream quiet for 5 minutes is checked with get, and read on while the task goes on.
- **Without streaming**, the send returns at once and get is polled after 1 second, doubling to at most 30, until the round ends.
- **Get** is tried again after a network error or a 5xx, after 1, 2 and 4 seconds; then it fails. A send is never tried again.
- **Subscribe** starts with get, whose task is its first event, ends there when the round has ended, and otherwise follows the task like a stream (or polls).
