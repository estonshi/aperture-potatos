# ext/memory conformance

Cases live in `conformance/loop/ext/memory/`. A case names the extension in `agent.extensions` as `"memory": options`; an absent field takes the extension's default (`README.md`). The options:

| Field | Meaning |
|---|---|
| `instructions` | Instruction files, path → content, in an in-memory file system |
| `order` | The paths the file source reads, in order; without it the extension has no instruction source |
| `items` | Memories the store holds before the case starts, oldest first: `content`, and the tags `user` and `project` (absent or empty is untagged) |
| `consolidate_on` | The points that consolidate, as the extension's option |
| `no_tools` | Register no memory tools |
| `shared_writes` | Let the model change memories without a user tag |

The store keeps memories in memory. It starts with `items` added in one change, in that order; each add gets the id `mem_<n>`, numbering from 1 in the order of adding, and its content trimmed. Updates and deletes name a memory the scope sees, else they fail as not found.
