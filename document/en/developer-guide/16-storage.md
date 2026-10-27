[← Previous: Context management](15-context.md) · [Contents](README.md) · [Next: Usage, call records and cost →](17-usage-cost.md)

# 16. Storage and resume

This chapter explains how to save sessions so that you can resume them and continue the conversation after the process restarts. It also describes the line format of the store and how to plug in your own storage backend.

In this chapter: [16.1 Choosing a store](#161-choosing-a-store) · [16.2 Resuming a session](#162-resuming-a-session) · [16.3 Storage format](#163-storage-format) · [16.4 Implementing a custom store](#164-implementing-a-custom-store) · [16.5 Session labels](#165-session-labels) · [16.6 Best practices](#166-best-practices)

## 16.1 Choosing a store

You configure the store with `kernel.WithStore`. The SDK ships with two implementations: an in-memory store and a JSONL file store.

```go
kernel.WithStore(store.NewMemory())       // in-memory store whose data is lost when the process exits; the default when no store is configured
kernel.WithStore(jsonl.New("./sessions")) // one <session_id>.jsonl file per session, one message per line; the directory is created on the first write
```

The JSONL implementation also writes a `<session_id>.calls.jsonl` file that holds the model call records; see [17.2](17-usage-cost.md#172-call-records).

`kernel.WithStorePolicy` sets what happens when a write fails. The available policies are listed below.

| Policy | Behavior when a write fails |
|---|---|
| `StoreWarn` (default) | Emits an `error{store_failed}` event and retries the unwritten lines at the next commit. |
| `StoreStrict` | Ends the turn with an error, which guarantees that a tool call that was not persisted is never executed. |

## 16.2 Resuming a session

With a persistent store configured, you can close a session and later resume it by calling `Resume` with its id.

```go
id := sess.Info().SessionID
_ = sess.Close(ctx)

again, err := ag.Resume(ctx, id)
```

When it resumes a session, the kernel behaves as follows:

- The kernel reads all lines and continues with the context that begins at the head of the last epoch. The head of an epoch is where the most recent compaction, clear, rewind, fork or model switch took place. Earlier lines remain in the file, and you can find them with history search.
- The system prompt is the one that was rendered at the time. If the model declares `ThinkingPrefixBound`, the tool definitions are also the ones from that time. The model, however, is the one currently configured on the agent.
- Tasks that the file still shows as running are restored in the `lost` state, and the model is told about them. A resumed session does not start a new turn on its own.
- The Pin set is rebuilt from the history.
- If the store holds a label record, the session's labels come from that record; see [16.5](#165-session-labels).
- Resuming runs the `session_start` hook with `Source` set to `resume`.

`Resume` returns an error in the following cases.

| Case | Result |
|---|---|
| The same id is already open under this agent | Returns `ErrSessionOpen`, which ensures that a session has only one writer. |
| The id does not exist | Returns `ErrSessionNotFound`. |
| The id belongs to a side chat | Returns `ErrSideChat`. A side chat must be opened from the main session with `SideChat`. |
| The id belongs to a task's child session | Returns `ErrChildSession`. A task's child session can be resumed only from within the task, through `ChildOf`. |
| The file format is newer than the current version | Returns `ErrIncompatibleStore`. |

To list the sessions that have been saved, call `store.List(ctx, prefix)`.

## 16.3 Storage format

Each line in the store is one `message.Message` encoded as JSON, and the schema is at `spec/schema/message.schema.json`. If you store sessions in a database instead, each line corresponds to one record. The table below lists the fields you will usually work with.

| Field | Description |
|---|---|
| `v` | The version of the line format. |
| `id`, `session_id`, `seq` | The line id, the session the line belongs to and its sequence number. Sequence numbers increase strictly within a session. |
| `epoch`, `turn_id` | The epoch and the turn the line belongs to. |
| `role` | One of `system`, `user`, `assistant` or `tool`. `system` is the first line of every epoch, and `tool` marks a batch of tool results. |
| `origin` | Where the line came from, with `kind` and `source`. `kind` is one of human, command, agent, task, reminder, continuation, epoch_head, model, tool and system. |
| `content` | An array of content blocks, distinguished by `type`. `type` is one of text, refusal, image, document, reasoning, tool_call, tool_result, reminder, opaque, hosted_call and hosted_result. |
| `producer`, `usage`, `stop_reason` | Present only on assistant lines: the API and model that produced the line, its usage and its stop reason. |
| `boundary` | Present only when the line starts an epoch, with fields such as `reason`, `trigger`, `strategy` and `pre_tokens`. `reason` is one of new, clear, compaction, switch, rewind, fork, side_chat and dropped. On the first line of the file, `boundary.session` records the agent name, the parent session and the labels at creation. |
| `tools`, `hosted_tools` | Present only on system lines, where they record the tool definitions for that epoch. |
| `tasks` | The start parameters and state of tasks. This is the only persistent record of a task. |
| `partial` | Marks a line that was committed after an interrupt. Such a line contains only the content blocks that had finished. |
| `context_window` | Present only when the session had set a window of its own when the line was written: that window, restored on resume. A copy keeps its original's value. |
| `ts` | The timestamp. |

`store.Current(msgs)` returns the lines from the head of the last epoch onward. That part is the context the model sees on the next turn.

## 16.4 Implementing a custom store

If you need to save sessions to a database or another storage service, implement the `store.SessionStore` interface.

```go
type SessionStore interface {
	Append(ctx context.Context, sessionID string, msgs []message.Message) error // writes must be atomic; a line with the same Seq and the same content is skipped, so retries are safe
	Load(ctx context.Context, sessionID string) ([]message.Message, error)      // returns lines in Seq order and drops a broken batch at the end
	List(ctx context.Context, prefix string) ([]string, error)                  // returns the session ids that start with prefix, sorted
}
```

Keep the following points in mind when you implement it:

- `Append` writes a batch of lines at a time and must write either all of them or none. When the kernel retries a write, it may commit lines that are already stored, so if a line with the same content is already stored at the same `Seq`, skip it. If the content differs, return `store.ErrSeqConflict`. That means there are two writers, and the session fails as a result. You can compare two lines with `store.SameLine(a, b)`.
- `Load` must drop an unfinished batch at the end, that is, a group of lines that share the same `batch` but have no `batch_end`.
- `List` must support prefix matching. A side chat's id is the main session's id plus a suffix, and resume finds side chats by prefix lookup.
- A panic in the store does not crash the session. The kernel treats it as a failed read or write.

If you also implement the `store.CallStore` interface, the model call ledger is persisted as well, so you can still query the call records of earlier turns after the process restarts. The interface has two methods, `AppendCalls` and `LoadCalls`. Each record is a JSON object with a strictly increasing `seq`, which you can read with `store.CallSeq(rec)`.

If you also implement the `store.LabelStore` interface, you can change session labels and look up sessions by label; see [16.5](#165-session-labels).

## 16.5 Session labels

Labels are key-value pairs attached to a session. The kernel does not interpret them; extensions and your program use them by convention. For example, `ext/memory` reads `user_id` and `project_id`. Set labels with `WithLabels` when you create a session, change them later with `SetLabels`, and look up sessions by label with `FindSessions`:

```go
err := sess.SetLabels(ctx, map[string]string{
	"user_id":  "u_43",  // set or overwrite this key
	"archived": "true",
	"draft":    "",      // an empty string deletes this key
})

found, err := ag.FindSessions(ctx, map[string]string{"user_id": "u_43"})
for _, f := range found {
	fmt.Println(f.SessionID, f.Labels)
}
```

`SetLabels` merges by key. It changes only the keys you pass and leaves the others as they are, so two pieces of code that change different keys do not overwrite each other. The new labels are written to the store first and take effect only after the write succeeds; if the write fails, the labels stay as they were. Calling `SetLabels` on a session that has been closed returns `ErrSessionClosed`, so open the session with `Resume` first.

After a change, `Info().Labels` returns the new values right away. Each place that reads labels picks up the change at its own time: `ext/memory` uses the new values on the next memory tool call and when it loads memories for the next epoch, and `WithInstructionsFunc` sees them when the system prompt is rendered again for the next epoch.

`FindSessions` returns the sessions whose labels contain all of the given key-value pairs, sorted by session id. An empty condition returns all sessions, and a key whose value in the condition is an empty string matches only sessions that do not have that key. The results include only top-level sessions created with `NewSession` or `Fork`; they do not include side chats or the child sessions of tasks. The query reads only the labels and does not open any session.

Forks and side chats copy the labels of their source session when they are created. After that, each session changes its own labels without affecting the other. If you pass `WithLabels` when you resume a session, those labels are merged into the stored labels and written back, just as with `SetLabels`.

Labels are stored separately from the session file and are handled by the optional `store.LabelStore` interface. Both the in-memory store and the JSONL store implement it, and the JSONL store writes a `<session_id>.labels.json` file for each session. The first line of the session file records the labels at creation and never changes after that. If your store does not implement `LabelStore`, `SetLabels` and `FindSessions` return `ErrNoLabelStore`, and the labels given at creation are still saved in the session file as usual.

```go
type LabelStore interface {
	SaveLabels(ctx context.Context, rec store.LabelRecord) error                          // replaces this session's entire label record
	LoadLabels(ctx context.Context, sessionID string) (store.LabelRecord, bool, error)    // returns false when there is no record
	FindLabels(ctx context.Context, match map[string]string) ([]store.LabelRecord, error) // returns the records whose labels contain all the pairs, sorted by session id
}
```

`LabelRecord` contains `SessionID`, `ParentSessionID` and `Labels`. The kernel treats a session as top-level when its `ParentSessionID` is empty. Calls to `LabelStore` can happen at the same time as other store calls for the same session, so the implementation must be safe for concurrent use. In a database implementation, you can index the labels so that `FindLabels` does not have to scan every record. You can use `store.MatchLabels(labels, match)` to check whether a record matches.

## 16.6 Best practices

- Use `StoreStrict` in production. The cost is that the current turn fails when the store fails; in exchange, every tool that ran is guaranteed to have a record.
- When you implement the store on a database, make `(session_id, seq)` the table's primary key and store `content` as JSON. The query behind `List` is `WHERE session_id LIKE ? ORDER BY session_id`.
- Do not modify lines that are already stored. The kernel treats history as append-only data. If you need to change the history, use rewind, fork or clear.
- You can use `WithIDGenerator` to give session ids your own prefix, such as a tenant id. `List` can then list sessions by tenant.
- After you resume a session, tell the user which background tasks were lost. Tasks in the `lost` state, as reported by the `tasks.snapshot` event and `Tasks().List()`, do not run again automatically. Whether to call `Restart` is up to the user or your program.

---

[← Previous: Context management](15-context.md) · [Contents](README.md) · [Next: Usage, call records and cost →](17-usage-cost.md)
