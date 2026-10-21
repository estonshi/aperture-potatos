[← Previous: Storage and resume](16-storage.md) · [Contents](README.md) · [Next: Limits and defaults →](18-limits.md)

# 17. Usage, call records and cost

This chapter explains how to look up the tokens each turn consumed, the model requests that were actually sent and the cost they incurred, and how to match these records one to one with gateway logs. If you are building billing, quota control or a cost dashboard, start with this chapter.

In this chapter: [17.1 Usage](#171-usage) · [17.2 Call records](#172-call-records) · [17.3 Tagging requests](#173-tagging-requests) · [17.4 Querying cost](#174-querying-cost) · [17.5 Best practices](#175-best-practices)

## 17.1 Usage

`message.Usage` describes the usage of one model call. It has the following fields:

| Field | Description |
|---|---|
| `InputTokens` | The token count of the entire input, **including** the part served from the cache. |
| `CacheReadTokens`, `CacheWriteTokens` | The number of tokens read from the cache and written to the cache. |
| `OutputTokens` | The number of output tokens, including reasoning tokens. |
| `ReasoningTokens` | The number of tokens taken up by the reasoning part of the output. |
| `HostedCalls` | The number of times server-side tools (web search, for example) were called, counted separately for each tool name. |
| `Estimated` | True when the usage is an estimate, which happens when the stream is cut off or the API does not report usage. |
| `Raw` | The raw usage JSON reported by the API. Some gateways include cost, speed and similar information in it. |

You can get usage from three places. The `usage.updated` event is emitted after each model call and carries the `Bucket`, `StepUsage`, `SessionTotal`, `Context` and `Forwarded` fields, where `Context` describes how much of the context window is used and `Forwarded` indicates that the usage comes from a child session. `TurnResult.Usage` is the total usage for the turn. `sess.Snapshot().Usage` returns a `UsageReport` with the session's cumulative usage.

`UsageReport` contains the following:

- `Total` is the total usage.
- `Buckets` breaks usage down by purpose, such as main, compaction, side, subagent and peer.
- `ByModel` breaks usage down by `api/model`.
- `LastStep` is the usage of the most recent step.
- `Turns`, `Steps` and `Retries` are the number of turns, steps and retries.
- `Discards` is the number of discarded replies, and `Abandons` is the number of abandoned tools.

`UsageReport` only ever grows, and it is independent of the per-turn usage data.

## 17.2 Call records

The kernel records one `ModelCall` for every HTTP request. Requests that were replaced by a retry are recorded too, so the call records match the gateway logs one to one. `ModelCall` has the following fields:

| Field | Description |
|---|---|
| `Seq` | The sequence number of the record in the ledger. |
| `TurnID`, `SessionID`, `RootSession`, `MessageID` | What the call belongs to. `RootSession` is the top-level session, and calls made by subagents belong to it as well. |
| `Bucket` | What the call was for. |
| `API`, `Endpoint`, `Model` | The API, endpoint and model the request was sent to. |
| `ResponseID`, `RequestID` | `ResponseID` is the response id returned by the API, which is the key for searching the cost logs of gateways such as LiteLLM. `RequestID` is the request id, which you use when you contact the vendor's support. |
| `Headers` | The response headers captured as specified with `WithCaptureHeaders`. |
| `Outcome` | The result of the call. `committed` means the reply was written to the history, `discarded` means the reply was discarded, and `failed` means the call failed. |
| `Usage`, `StartedAt` | The usage and start time of the call. |
| `Step`, `Attempt` | The step in the main loop and the attempt number. An `Attempt` greater than 1 means the call is a retry. |
| `ParentSeq` | Used only for side chat calls. It is the last line of the main session that the request carried. |

Use `TurnCalls` to query the call records of a turn:

```go
calls, err := sess.TurnCalls(turnID)
```

This method does not call the model. Records for the most recent 100 turns are returned straight from memory, and older records are read from the `CallStore` (see [16.4](16-storage.md#164-implementing-a-custom-store)). If no `CallStore` is configured, querying an older turn returns `ErrTurnNotFound`. The result contains every request of the turn, including the requests for compaction summaries and the requests of the subagents and in-process peers that the turn started. If `TurnCalls.Pending` is true, the turn or a task it started is still running, and you can query again later.

If you sent requests on behalf of this session outside the SDK, for example by calling the model directly yourself, you can record those calls in the ledger with `sess.RecordCalls(bucket, turnID, calls)`, and their usage is counted in the totals as well.

## 17.3 Tagging requests

The following two options attach request headers to model requests and capture selected headers from the responses, which makes it easier to correlate requests on the gateway or vendor side:

| Option | Effect |
|---|---|
| `kernel.WithRequestHeaders(fn)` | Calls `fn(RequestInfo)` before each model request is sent and adds the returned headers to the request. `RequestInfo` contains `RootSessionID`, `SessionID`, `TurnID`, `Bucket`, `API` and `Model`. Requests for compaction, side calls and subagents also go through this function. |
| `kernel.WithCaptureHeaders(names...)` | Records the response headers with the given names in `ModelCall.Headers` and in the `Producer.Headers` of the reply message. |

## 17.4 Querying cost

Once a cost source is configured, you can call `sess.TurnCost(ctx, turnID)` to look up the cost of each request in a turn and add them up. If no cost source is configured, this method returns `ErrNoCostSource`.

```go
cost, err := sess.TurnCost(ctx, res.TurnID)
// cost.Total is the total (Amount, Currency), cost.Priced holds each request's ResponseID and Cost, and cost.Missing lists the response ids whose cost was still not found when ctx ended
```

Gateways usually write their cost logs asynchronously. For requests whose cost cannot be found yet, `TurnCost` retries with backoff until ctx ends. Give `ctx` a reasonable timeout, and check `Missing` after the call returns.

When you call models through a LiteLLM gateway, you can configure it like this:

```go
lo := litellm.Options{
	BaseURL:       os.Getenv("LITELLM_URL"),
	APIKey:        os.Getenv("LITELLM_MASTER_KEY"), // must be a key that is allowed to read spend logs
	SessionHeader: "x-litellm-session-id",          // if empty, requests are not tagged and cost can only be looked up call by call
}

kernel.WithCaptureHeaders("x-litellm-call-id"),
kernel.WithRequestHeaders(litellm.RequestHeaders(lo)),
kernel.WithCostSource(litellm.CostSource(lo)),
```

To connect another billing system, implement the `kernel.CostSource` interface:

```go
type CostSource interface {
	// Returns costs keyed by each call's index in the slice. Calls whose cost is not available yet can be left out, and TurnCost will ask again
	Price(ctx context.Context, calls []ModelCall) (map[int]Cost, error)
}
```

When the billing backend returns an HTTP error, `Price` should return `*kernel.CostError{Status, Message}`. `TurnCost` retries on status code 429 and on 5xx, and gives up on any other status code.

## 17.5 Best practices

- For quota control, you can use `SessionTotal` from the `usage.updated` event directly. The event is emitted in real time, so you do not need to poll.
- To bill tenants, aggregate by `ModelCall.RootSession`. The costs of subagents, side chats and compaction are all attributed to the root session, and you can also use `Bucket` to separate the cost of the conversation itself from auxiliary costs.
- Save the `ResponseID` of every call. When you reconcile accounts, it is the one key that matches the records in the gateway and in the vendor's console.
- Look up cost asynchronously after a turn ends, and give ctx a few tens of seconds. If `Missing` is not empty, query again later.
- Keep in mind that retries cost money too. The vendor charges for calls whose `Outcome` is `failed` or `discarded` just the same.

---

[← Previous: Storage and resume](16-storage.md) · [Contents](README.md) · [Next: Limits and defaults →](18-limits.md)
