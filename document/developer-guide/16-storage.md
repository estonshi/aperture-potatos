[← 上一章：上下文管理](15-context.md) · [目录](README.md) · [下一章：用量、调用记录与费用 →](17-usage-cost.md)

# 16. 存储与恢复

本章介绍如何把会话保存下来，使进程重启之后能够恢复会话并继续对话。本章还说明了存储的行格式，以及如何接入你自己的存储后端。

本章内容：[16.1 选择存储](#161-选择存储) · [16.2 恢复会话](#162-恢复会话) · [16.3 存储格式](#163-存储格式) · [16.4 实现自定义存储后端](#164-实现自定义存储后端) · [16.5 会话标签](#165-会话标签) · [16.6 建议做法](#166-建议做法)

## 16.1 选择存储

存储通过 `kernel.WithStore` 配置。SDK 自带两种实现：内存存储和 JSONL 文件存储。

```go
kernel.WithStore(store.NewMemory())       // 内存存储，进程退出后数据即丢失，这是不配置存储时的默认值
kernel.WithStore(jsonl.New("./sessions")) // 每个 session 对应一个 <session_id>.jsonl 文件，每行一条消息，目录在第一次写入时创建
```

JSONL 实现还会写一个 `<session_id>.calls.jsonl` 文件，用来保存模型调用记录，详见 [17.2](17-usage-cost.md#172-调用记录)。

写入失败时的处理方式由 `kernel.WithStorePolicy` 指定，可选策略如下。

| 策略 | 写入失败时的行为 |
|---|---|
| `StoreWarn`（默认） | 发出 `error{store_failed}` 事件，并在下次提交时重试没有写成功的行。 |
| `StoreStrict` | 以错误结束本轮，从而保证没有落盘的工具调用不会被执行。 |

## 16.2 恢复会话

配置了持久化存储之后，你可以先关闭一个 session，之后再用它的 id 调用 `Resume` 恢复。

```go
id := sess.Info().SessionID
_ = sess.Close(ctx)

again, err := ag.Resume(ctx, id)
```

恢复会话时，kernel 的行为如下：

- kernel 读出全部行，从最后一个 epoch 的开头之后继续运行。epoch 的开头是最近一次压缩、清空、回退、分叉或切换模型的位置。更早的行仍然保留在文件中，可以通过历史检索找到。
- 系统提示词使用当时渲染的那一份。如果模型声明了 `ThinkingPrefixBound`，工具定义也使用当时的那一份。模型则使用 agent 当前配置的模型。
- 文件中仍显示为运行中的任务会恢复为 `lost` 状态，并告知模型。恢复后的会话不会自动开始新的一轮。
- Pin 集合会根据历史重建。
- 如果存储保存了标签记录，会话的标签以记录为准，见 [16.5](#165-会话标签)。
- 恢复时会触发 `session_start` hook，其 `Source` 为 `resume`。

以下情况下 `Resume` 会返回错误。

| 情况 | 结果 |
|---|---|
| 同一个 id 已经在这个 agent 下打开 | 返回 `ErrSessionOpen`，以保证一个 session 只有一个写者。 |
| id 不存在 | 返回 `ErrSessionNotFound`。 |
| id 属于旁路对话 | 返回 `ErrSideChat`。旁路对话需要从主会话通过 `SideChat` 打开。 |
| id 属于任务的子会话 | 返回 `ErrChildSession`。任务子会话只能在任务中通过 `ChildOf` 恢复。 |
| 文件格式比当前版本新 | 返回 `ErrIncompatibleStore`。 |

如果需要列出已经保存的 session，可以调用 `store.List(ctx, prefix)`。

## 16.3 存储格式

存储中的每一行是一条 `message.Message` 的 JSON，schema 位于 `spec/schema/message.schema.json`。如果改用数据库存储，一行对应一条记录。下表列出你通常会用到的字段。

| 字段 | 说明 |
|---|---|
| `v` | 行格式的版本。 |
| `id`、`session_id`、`seq` | 分别是行 id、所属的 session 和序号。序号在同一个 session 内严格递增。 |
| `epoch`、`turn_id` | 该行所属的 epoch 和轮次。 |
| `role` | 取值为 `system`、`user`、`assistant` 或 `tool`。`system` 是每个 epoch 的第一行，`tool` 表示一批工具结果。 |
| `origin` | 该行的来源，包含 `kind` 和 `source`。`kind` 的取值有 human、command、agent、task、reminder、continuation、epoch_head、model、tool 和 system。 |
| `content` | 内容块数组，按 `type` 区分。`type` 的取值有 text、refusal、image、document、reasoning、tool_call、tool_result、reminder、opaque、hosted_call 和 hosted_result。 |
| `producer`、`usage`、`stop_reason` | 仅出现在 assistant 行中，分别表示生成该行的接口和模型、用量以及停止原因。 |
| `boundary` | 仅当该行开启一个 epoch 时出现，包含 `reason`、`trigger`、`strategy`、`pre_tokens` 等字段。`reason` 的取值有 new、clear、compaction、switch、rewind、fork、side_chat 和 dropped。文件第一行的 `boundary.session` 记录了 agent 名、父 session 和创建时的标签。 |
| `tools`、`hosted_tools` | 仅出现在 system 行中，记录该 epoch 的工具定义。 |
| `tasks` | 任务的启动参数和状态。这是任务唯一的持久记录。 |
| `partial` | 表示该行是在被打断后提交的，其中只包含已经结束的内容块。 |
| `context_window` | 仅当写这一行时会话自己设置了窗口才出现：该会话的上下文窗口，恢复会话时用它还原；副本沿用原行的值。 |
| `ts` | 时间。 |

`store.Current(msgs)` 从一组行中取出最后一个 epoch 开头之后的部分，这部分就是下一轮模型会看到的上下文。

## 16.4 实现自定义存储后端

如果你需要把会话保存到数据库或其他存储服务中，可以实现 `store.SessionStore` 接口。

```go
type SessionStore interface {
	Append(ctx context.Context, sessionID string, msgs []message.Message) error // 写入必须是原子的；同 Seq 且内容相同的行会被跳过，因此重试是安全的
	Load(ctx context.Context, sessionID string) ([]message.Message, error)      // 按 Seq 顺序返回，并去掉末尾写坏的批次
	List(ctx context.Context, prefix string) ([]string, error)                  // 返回以 prefix 开头的 session id，结果需要排序
}
```

实现时需要注意以下几点：

- `Append` 一次写入一批行，必须全部写入或全部不写入。kernel 重试写入时，可能会再次提交已经存过的行，因此如果同一个 `Seq` 已经存有内容相同的行，应当跳过。如果内容不同，则返回 `store.ErrSeqConflict`。这种情况说明存在两个写者，session 会因此失败。你可以用 `store.SameLine(a, b)` 比较两行是否相同。
- `Load` 需要去掉末尾没有写完的批次，即 `batch` 相同但没有 `batch_end` 的那一组行。
- `List` 必须支持前缀匹配。旁路对话的 id 是主会话 id 加上后缀，恢复时依靠前缀查找它们。
- store 中发生的 panic 不会导致 session 崩溃，kernel 会把它当作一次读写失败处理。

如果再实现 `store.CallStore` 接口，模型调用账本也会一起落盘，进程重启后仍能查询以前轮次的调用记录。该接口包含 `AppendCalls` 和 `LoadCalls` 两个方法。每条记录是一个带有严格递增 `seq` 的 JSON 对象，你可以用 `store.CallSeq(rec)` 取出它的 seq。

如果再实现 `store.LabelStore` 接口，就可以修改会话标签并按标签查询会话，见 [16.5](#165-会话标签)。

## 16.5 会话标签

标签是附在会话上的键值对，kernel 不解读它们的含义，由扩展和你的程序按约定使用，例如 `ext/memory` 读取 `user_id` 和 `project_id`。创建会话时用 `WithLabels` 设置标签，之后可以用 `SetLabels` 修改，并用 `FindSessions` 按标签查找会话：

```go
err := sess.SetLabels(ctx, map[string]string{
	"user_id":  "u_43",  // 写入或覆盖这个键
	"archived": "true",
	"draft":    "",      // 值为空字符串表示删除这个键
})

found, err := ag.FindSessions(ctx, map[string]string{"user_id": "u_43"})
for _, f := range found {
	fmt.Println(f.SessionID, f.Labels)
}
```

`SetLabels` 按键合并：只改传入的键，没提到的键保持不变，所以两处代码各自修改不同的键时不会互相覆盖。新的标签先写入存储，成功后才生效，写入失败时标签保持原样。对已经关闭的 session 调用会返回 `ErrSessionClosed`，需要先用 `Resume` 打开它。

修改之后，`Info().Labels` 立刻返回新值。各个读取标签的地方按自己的时机生效：`ext/memory` 在下一次 memory 工具调用和下一个 epoch 加载记忆时使用新值，`WithInstructionsFunc` 在下一个 epoch 重新渲染系统提示词时看到新值。

`FindSessions` 返回标签包含全部给定键值对的会话，按 session id 排序。条件为空时返回全部会话，条件中值为空字符串的键只匹配没有这个键的会话。结果只包含用 `NewSession` 或 `Fork` 创建的顶层会话，不包含旁路对话和任务的子会话。查询只读取标签，不会打开会话。

分叉和旁路对话在创建时复制源会话的标签，之后各自修改，互不影响。恢复会话时如果传入 `WithLabels`，这些标签会像 `SetLabels` 一样合并进存储的标签并写回。

标签与会话文件分开保存，由可选接口 `store.LabelStore` 负责。内存存储和 JSONL 存储都实现了它，JSONL 存储为每个会话写一个 `<session_id>.labels.json`。会话文件第一行记录的是创建时的标签，之后不再改变。如果你的存储没有实现 `LabelStore`，`SetLabels` 和 `FindSessions` 会返回 `ErrNoLabelStore`，创建时的标签照常保存在会话文件里。

```go
type LabelStore interface {
	SaveLabels(ctx context.Context, rec store.LabelRecord) error                          // 整条替换这个会话的标签记录
	LoadLabels(ctx context.Context, sessionID string) (store.LabelRecord, bool, error)    // 没有记录时返回 false
	FindLabels(ctx context.Context, match map[string]string) ([]store.LabelRecord, error) // 返回标签包含全部键值对的记录，按 session id 排序
}
```

`LabelRecord` 包含 `SessionID`、`ParentSessionID` 和 `Labels`。kernel 根据 `ParentSessionID` 是否为空判断是不是顶层会话。`LabelStore` 的调用可能与同一个 session 的其他存储调用同时发生，实现需要支持并发。用数据库实现时，可以给标签建索引，让 `FindLabels` 不必扫描全部记录。你可以用 `store.MatchLabels(labels, match)` 判断一条记录是否匹配。

## 16.6 建议做法

- 建议在生产环境中使用 `StoreStrict`。它的代价是存储故障时当前这一轮会失败，换来的是执行过的工具一定有记录。
- 用数据库实现存储时，建议表以 `(session_id, seq)` 作为主键，`content` 以 JSON 形式存储。`List` 对应的查询是 `WHERE session_id LIKE ? ORDER BY session_id`。
- 不要修改已经存储的行。kernel 把历史视为只追加的数据，如果需要修改历史，请使用回退、分叉或清空。
- 可以用 `WithIDGenerator` 为 session id 加上你自己的前缀，例如租户 id。这样 `List` 就可以按租户列出 session。
- 恢复会话后，应告诉用户哪些后台任务已经丢失。`tasks.snapshot` 事件和 `Tasks().List()` 中处于 `lost` 状态的任务不会自动重新运行，是否调用 `Restart` 由用户或你的程序决定。

---

[← 上一章：上下文管理](15-context.md) · [目录](README.md) · [下一章：用量、调用记录与费用 →](17-usage-cost.md)
