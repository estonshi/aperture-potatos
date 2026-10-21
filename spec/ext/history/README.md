# ext/history

The `search_history` tool over the runtime's history search and line lookup (runtime.md §12.9). The tool definition is `search_history.json`; every text the model sees is a file here, sent byte for byte.

- The tool is registered at agent scope, so a session, its forks and its side chats send the same definition. It runs in parallel with other read-only calls.
- Exactly one of `query` and `seq` must be given, otherwise the result is an error with `need_one.md`.
- Without `parent` the session searches itself (`runtime.md` §12.9): its current context and what compaction took out of it, not what a rewind or clear discarded.
- `parent: true` searches the parent of a child session, and only the lines its current context holds, copies found at their original seq. A seq outside that context gives `no_line.md` as an unknown one does. A session without a parent gets an error with `no_parent.md`. A side chat, whose every request already carries its main session's current context, gets an error with `side_chat_parent.md`.
- A query is split on whitespace into words. Each hit is one line, newest first: `seq=<seq> epoch=<epoch> role=<role> turn=<turn id> in_context=<yes|no>: <snippet>`. `turn=` is left out for a line outside any turn. Line breaks in the snippet become spaces. No hit gives `no_matches.md`.
- A seq gives the header of a hit line without `in_context=` and the snippet, then the line's text on the following lines: text, reminders, tool calls as `<name> <input JSON>` and tool results, one block per line. Text longer than 20,000 code points is cut there and followed by `truncated.md` on its own line, with `{{limit}}` 20000. An unknown seq gives an error with `no_line.md`, `{{seq}}` the given number.
